//go:build integration && (linux || darwin)

package executor

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	v1 "github.com/Autarquico/codefabric/internal/api/v1"
	"github.com/Autarquico/codefabric/internal/domain"
	"github.com/Autarquico/codefabric/internal/support/clock"
	"github.com/Autarquico/codefabric/internal/testutil/fakeharness"
	"github.com/Autarquico/codefabric/internal/worker/execwrap"
	"github.com/Autarquico/codefabric/internal/worker/outputship"
	"github.com/Autarquico/codefabric/internal/worker/state"
)

// recordingUploader is the Controller's end of the output: it takes every
// entry and keeps what it was sent, per Run.
type recordingUploader struct {
	mu   sync.Mutex
	data map[string][]byte
	eof  map[string]bool
}

func (u *recordingUploader) UploadOutput(_ context.Context, batch v1.OutputBatch) (v1.OutputBatchResponse, error) {
	u.mu.Lock()
	defer u.mu.Unlock()
	var resp v1.OutputBatchResponse
	for _, e := range batch.Entries {
		if uint64(len(u.data[e.RunID])) == e.Offset {
			u.data[e.RunID] = append(u.data[e.RunID], e.Data...)
		}
		u.eof[e.RunID] = u.eof[e.RunID] || e.EOF
		resp.Acks = append(resp.Acks, v1.OutputAck{RunID: e.RunID, LeaseEpoch: e.LeaseEpoch, End: uint64(len(u.data[e.RunID])), Closed: u.eof[e.RunID]})
	}
	return resp, nil
}

func (u *recordingUploader) sent(run string) ([]byte, bool) {
	u.mu.Lock()
	defer u.mu.Unlock()
	return bytes.Clone(u.data[run]), u.eof[run]
}

// TestScenario_SecretsLocal_Redaccion (the part of T14a): a harness under
// real tmux and the real wrapper gets a secret in its environment and prints
// it in two goes. output.log, what is shipped to the Controller and the
// reports hold [REDACTED:<ref>] and never the value; launch.json holds the
// ref only. That secrets/ is gone once the Run is stopped and its result
// sealed is T14b's (the release), and is checked there.
func TestScenario_SecretsLocal_Redaccion(t *testing.T) {
	for _, mode := range []domain.Mode{domain.ModeHeadless, domain.ModeInteractive} {
		t.Run(string(mode), func(t *testing.T) {
			f := newFakeFixture(t, 2*time.Second, every(configWith(t, "dark",
				fakeharness.Step{EmitEnv: &fakeharness.EmitEnv{Name: "GH", SplitAt: 9, PauseMS: 300}},
			)))
			f.opts.Secrets = newFakeStore(map[string]string{"github.personal": ghValue})
			f.e = f.build()
			f.e.SetLimits(Limits{MaxConcurrentRuns: 4, CPU: 16, MemoryMB: 65536})
			up := &recordingUploader{data: map[string][]byte{}, eof: map[string]bool{}}
			ship := outputship.New(outputship.Options{DB: f.db, Uploader: up, Paths: f.paths, Clock: clock.Real{}, Log: slog.New(slog.DiscardHandler)})
			ctx, cancel := context.WithCancel(t.Context())
			shipped := make(chan struct{})
			go func() { _ = ship.Run(ctx); close(shipped) }()
			defer func() { cancel(); <-shipped }()

			k := f.assign(secretSpecFor(fakeharness.DefaultID, "do it", string(mode), map[string]string{"GH": "github.personal"}))
			defer f.runLoops()()
			f.waitPhase(k, state.PhaseStopped)

			reports := f.reports(k)
			if got := kinds(reports); !slices.Equal(got, []v1.RunReportKind{v1.ReportStarted, v1.ReportExited, v1.ReportStopped}) ||
				reports[1].ExitCode == nil || *reports[1].ExitCode != 0 {
				t.Fatalf("reports = %+v", reports)
			}
			noValueInReports(t, reports)

			dir := f.runDir(k)
			marker := "[REDACTED:github.personal]"
			log, err := os.ReadFile(filepath.Join(dir, execwrap.OutputLog)) //nolint:gosec // a file of the test
			if err != nil {
				t.Fatal(err)
			}
			// The marker says the harness got the value and printed it.
			if !strings.Contains(string(log), marker) || bytes.Contains(log, []byte(ghValue)) {
				t.Errorf("output.log = %q", log)
			}
			var sent []byte
			waitFor(t, "the end of the output at the Controller", func() bool {
				var eof bool
				sent, eof = up.sent(k.RunID)
				return eof
			})
			if !strings.Contains(string(sent), marker) || bytes.Contains(sent, []byte(ghValue)) {
				t.Errorf("sent to the Controller = %q", sent)
			}

			var l execwrap.Launch
			if ok, err := execwrap.Read(dir, execwrap.LaunchFile, &l); !ok || err != nil {
				t.Fatalf("launch.json: %v %v", ok, err)
			}
			if !slices.Equal(l.SecretEnv, []execwrap.SecretEnv{{Name: "GH", Ref: "github.personal"}}) {
				t.Errorf("secret_env = %+v", l.SecretEnv)
			}
			noValueIn(t, dir)
			noValueIn(t, f.worktree(k))
			raw, _ := json.Marshal(f.run(k))
			if bytes.Contains(raw, []byte(ghValue)) {
				t.Error("state.db holds the value")
			}
			if mode == domain.ModeInteractive {
				if ack, err := os.ReadFile(filepath.Join(dir, execwrap.CaptureAckFile)); err != nil || !strings.HasPrefix(string(ack), "ready") { //nolint:gosec // a file of the test
					t.Errorf("the capture filter acknowledged %q (%v)", ack, err)
				}
			}
		})
	}
}
