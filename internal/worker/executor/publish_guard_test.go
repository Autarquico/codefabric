//go:build linux || darwin

package executor

import (
	"bytes"
	"encoding/json"
	"errors"
	"log/slog"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	v1 "github.com/Autarquico/codefabric/internal/api/v1"
	"github.com/Autarquico/codefabric/internal/domain"
	"github.com/Autarquico/codefabric/internal/worker/execwrap"
	"github.com/Autarquico/codefabric/internal/worker/gitops"
	"github.com/Autarquico/codefabric/internal/worker/state"
)

// syncBuffer is a log destination safe for the goroutines of the executor.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// guardFixture is a Worker that holds a secret, with a fake git whose guard
// blocks whatever is checked against a value when block is set, and a log
// that is kept.
func guardFixture(t *testing.T) (*fixture, *fakeGit, *syncBuffer) {
	t.Helper()
	g := &fakeGit{head: headOID}
	logs := &syncBuffer{}
	f := secretFixture(t, newFakeStore(map[string]string{"github.personal": ghValue}), func(o *Options) {
		o.Git = g
		o.RelayTLS = RelayTLS{Cert: "/c/cert.pem", Key: "/c/key.pem", CA: "/c/ca.pem"}
		o.Log = slog.New(slog.NewTextHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
	})
	return f, g, logs
}

func blockOnValues(f gitops.Forbidden) error {
	if len(f.Values) > 0 {
		return &gitops.BlockedError{Reason: `the result contains the value of secret "` + f.Values[0].Ref + `"`}
	}
	return nil
}

// noValueAnywhere is the sentinel: no value in the reports, the logs, the
// intents of the sealings or the files of the Worker but the delivery.
func noValueAnywhere(t *testing.T, f *fixture, g *fakeGit, k state.Key, logs *syncBuffer) {
	t.Helper()
	noValueInReports(t, f.reports(k))
	noValueIn(t, f.paths.DataDir)
	if strings.Contains(logs.String(), ghValue) {
		t.Error("the log holds a value")
	}
	g.mu.Lock()
	intents, _ := json.Marshal(g.seals)
	g.mu.Unlock()
	if bytes.Contains(intents, []byte(ghValue)) {
		t.Error("an intent holds a value")
	}
}

func lastFailure(t *testing.T, f *fixture, k state.Key) v1.RunReportEntry {
	t.Helper()
	var last *v1.RunReportEntry
	for _, r := range f.reports(k) {
		if r.Kind == v1.ReportPublicationFailed {
			last = &r
		}
	}
	if last == nil {
		t.Fatalf("no publication_failed among %v", kinds(f.reports(k)))
	}
	return *last
}

func TestTheSealingIsCheckedAgainstTheValuesOfTheRun(t *testing.T) {
	f, g, logs := guardFixture(t)
	k := f.endedSecretRun()
	f.turn()
	local, err := state.RunBy(t.Context(), f.db.Reader(), k)
	if err != nil {
		t.Fatal(err)
	}
	g.mu.Lock()
	checked := append([]gitops.Forbidden(nil), g.forbidden...)
	g.mu.Unlock()
	if len(checked) != 1 || checked[0].Since != local.SourceCommit || len(checked[0].Values) != 1 ||
		checked[0].Values[0].Ref != "github.personal" || string(checked[0].Values[0].Value) != ghValue {
		t.Fatalf("the sealing was checked against %d sets", len(checked))
	}
	f.turn() // the push is checked too
	g.mu.Lock()
	pushed := len(g.forbidden) == 2 && len(g.forbidden[1].Values) == 1
	g.mu.Unlock()
	if !pushed || g.pushCount() != 1 {
		t.Errorf("the push was not checked against the values (%d pushes)", g.pushCount())
	}
	noValueAnywhere(t, f, g, k, logs)
}

func TestAResultWithAValueIsNotPublishedAndTheSecretsGo(t *testing.T) {
	f, g, logs := guardFixture(t)
	g.guard = blockOnValues
	k := f.endedSecretRun()
	f.turn()
	if p := f.publication(k); p.State != state.PubFailed || g.pushCount() != 0 {
		t.Fatalf("publication = %s after %d pushes", p.State, g.pushCount())
	}
	failed := lastFailure(t, f, k)
	if failed.Phase != v1.PhaseSeal || !strings.Contains(failed.Message, `"github.personal"`) {
		t.Errorf("failure = %+v", failed)
	}
	f.releaseTick()
	if f.delivered(k) {
		t.Error("the secrets are left after the publication failed")
	}
	noValueAnywhere(t, f, g, k, logs)
}

func TestAPushTheGuardStopsEndsThePublication(t *testing.T) {
	f, g, logs := guardFixture(t)
	k := f.endedSecretRun()
	f.turn()
	g.guard = blockOnValues
	f.turn()
	if p := f.publication(k); p.State != state.PubFailed || g.pushCount() != 0 {
		t.Fatalf("publication = %s after %d pushes", p.State, g.pushCount())
	}
	if failed := lastFailure(t, f, k); failed.Phase != v1.PhasePush || !strings.Contains(failed.Message, "blocked") {
		t.Errorf("failure = %+v", failed)
	}
	noValueAnywhere(t, f, g, k, logs)
}

// A failure of a sealing is redacted before it is logged, kept or reported,
// whatever part of it gitops did not redact.
func TestASealingFailureQuotingAValueIsRedacted(t *testing.T) {
	f, g, logs := guardFixture(t)
	g.seal = func(gitops.Intent) (gitops.Sealed, error) {
		return gitops.Sealed{}, errors.New("gitops: git add failed (128): error: open(\"leak-" + ghValue + "\"): Permission denied")
	}
	k := f.endedSecretRun()
	f.turn()
	if p := f.publication(k); p.State != state.PubFailed {
		t.Fatalf("publication = %s", p.State)
	}
	if failed := lastFailure(t, f, k); !strings.Contains(failed.Message, "[REDACTED:github.personal]") {
		t.Errorf("failure = %+v", failed)
	}
	noValueAnywhere(t, f, g, k, logs)
}

// TestARepublicationAfterTheReleaseFailsClosed: once the values are gone (the
// publication ended and released them), nothing can be checked against them,
// so neither `cf run publish` nor the preservation of an orphan sends anything.
func TestARepublicationAfterTheReleaseFailsClosed(t *testing.T) {
	released := func(t *testing.T) (*fixture, *fakeGit, *syncBuffer, state.Key) {
		f, g, logs := guardFixture(t)
		k := f.endedSecretRun()
		f.turn()
		f.turn()
		f.confirm(k, viewOf(k, domain.PublicationPublished))
		f.releaseTick()
		if f.delivered(k) || f.publication(k).State != state.PubPublished {
			t.Fatal("the secrets were not released after the publication")
		}
		return f, g, logs, k
	}
	t.Run("cf run publish", func(t *testing.T) {
		f, g, logs, k := released(t)
		seals, pushes := g.sealCount(), g.pushCount()
		f.publishOrder(k)
		for f.turn() {
		}
		if p := f.publication(k); p.State != state.PubFailed || g.sealCount() != seals || g.pushCount() != pushes {
			t.Fatalf("publication = %s, %d seals, %d pushes", p.State, g.sealCount(), g.pushCount())
		}
		if failed := lastFailure(t, f, k); !strings.Contains(failed.Message, "no longer on this Worker") {
			t.Errorf("failure = %+v", failed)
		}
		noValueAnywhere(t, f, g, k, logs)
	})
	t.Run("the orphan", func(t *testing.T) {
		f, g, logs, k := released(t)
		pushes := g.pushCount()
		before := f.publication(k)
		must(t, f.db.WithTx(t.Context(), func(tx *state.Tx) error { return state.FailPublication(t.Context(), tx, before, "gave up", t0) }))
		f.answers(staleVerdicts)
		f.e.spawn = func(state.Key) {}
		must(t, f.e.reconcile(t.Context(), f.e.reconcileGen.Load()))
		if p := f.publication(k); p.State != state.PubSealed {
			t.Fatalf("the orphan was not requeued: %s", p.State)
		}
		for f.turn() {
		}
		if p := f.publication(k); p.State != state.PubFailed || g.pushCount() != pushes {
			t.Fatalf("publication = %s after %d pushes", p.State, g.pushCount())
		}
		noValueAnywhere(t, f, g, k, logs)
	})
}

func TestAResultOfAnOrphanIsCheckedBeforeItIsKept(t *testing.T) {
	f, g, logs := guardFixture(t)
	k := f.endedSecretRun()
	f.turn()
	f.ctrl.mu.Lock()
	f.ctrl.relayToken = func(string, uint64) (v1.RelayTokenResponse, error) {
		return v1.RelayTokenResponse{}, domain.NewError(domain.CodeStaleLease, "this is not the live lease of the Run", nil)
	}
	f.ctrl.mu.Unlock()
	f.turn()
	f.answers(staleVerdicts)
	f.e.spawn = func(state.Key) {}
	must(t, f.e.reconcile(t.Context(), f.e.reconcileGen.Load()))
	f.passBackoff()
	g.guard = blockOnValues
	for f.turn() {
	}
	if p := f.publication(k); p.State != state.PubFailed || g.pushCount() != 0 {
		t.Fatalf("publication = %s after %d pushes", p.State, g.pushCount())
	}
	noValueAnywhere(t, f, g, k, logs)
}

func TestARunWithoutSecretsIsPublishedWithNothingToCheck(t *testing.T) {
	f, g := newGitFixture(t)
	k := f.sealedRun()
	f.turn()
	if p := f.publication(k); p.State != state.PubPushed {
		t.Fatalf("publication = %s", p.State)
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	for _, fb := range g.forbidden {
		if fb.Since != "" || fb.Values != nil {
			t.Errorf("a Run without secrets was checked against %+v", fb)
		}
	}
	if len(g.forbidden) != 2 {
		t.Errorf("%d checks, want the sealing and the push", len(g.forbidden))
	}
}

// rebuiltForStop is a Run of which this Worker lost its record and kept its
// markers and its worktree, ended, and a stop of the Controller that names it,
// carrying spec (none from a Controller before #23): the record is rebuilt to
// stop it, and its result is to be sealed and published.
func (f *fixture) rebuiltForStop(n uint64, spec []byte) state.Key {
	f.t.Helper()
	k := state.Key{RunID: runID(n), LeaseEpoch: 1}
	w := newWrapperProcess(f.t, "exit 0")
	<-w.ended
	must(f.t, os.MkdirAll(f.runDir(k), 0o700))
	must(f.t, os.MkdirAll(f.e.worktree(k), 0o700))
	f.writeWrapper(k, w.w)
	f.writeStarted(k, w.w, t0)
	f.writeExit(k, execwrap.Exit{ExitCode: intp(0), ExitedAt: t0.Add(time.Second), Cause: execwrap.CauseExit})
	f.e.spawn = func(state.Key) {}
	blocked, err := f.e.persist(f.t.Context(), []v1.WorkerCommand{{Seq: 1, Type: domain.CommandStop, Stop: &v1.StopCommand{
		RunID: k.RunID, LeaseID: "lease_" + k.RunID, Epoch: k.LeaseEpoch, Reason: domain.StopCancel, RunShort: "run-1",
		TaskID: "task_1", ProjectID: "project_1", Branch: "cf/t/run-1", Spec: spec,
	}}})
	if err != nil || blocked {
		f.t.Fatalf("persist: blocked=%v err=%v", blocked, err)
	}
	if r := f.run(k); !r.Adopted {
		f.t.Fatalf("the record was not rebuilt: %+v", r)
	}
	waitFor(f.t, "the Run to be settled", func() bool { return f.e.step(f.t.Context(), k) })
	for f.turn() {
	}
	return k
}

// A Run whose record was rebuilt for a stop, its state.db lost, is published
// unchecked only when the stop says it declared no secrets: without its spec
// the Worker cannot tell, and with secrets it no longer holds (the janitor
// released them with the record gone) nothing can be checked. Neither is sent.
func TestARunRebuiltForAStopIsPublishedUncheckedOnlyIfItDeclaredNoSecrets(t *testing.T) {
	for _, tc := range []struct {
		name    string
		spec    []byte
		blocked string
	}{
		{"without its spec", nil, "cannot tell whether it declared secrets"},
		{"with secrets released", secretSpec("echo hi", "headless", map[string]string{"GH": "github.personal"}), "no longer on this Worker"},
		{"without secrets", specJSON("echo hi", "command", "headless"), ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f, g := newGitFixture(t)
			k := f.rebuiltForStop(120, tc.spec)
			if tc.blocked == "" {
				if p := f.publication(k); p.State != state.PubPushed || g.pushCount() != 1 {
					t.Fatalf("publication = %s, %d pushes", p.State, g.pushCount())
				}
				return
			}
			if p := f.publication(k); p.State != state.PubFailed || g.sealCount() != 0 || g.pushCount() != 0 {
				t.Fatalf("publication = %s, %d seals, %d pushes", p.State, g.sealCount(), g.pushCount())
			}
			if failed := lastFailure(t, f, k); !strings.Contains(failed.Message, tc.blocked) {
				t.Errorf("failure = %+v", failed)
			}
		})
	}
}
