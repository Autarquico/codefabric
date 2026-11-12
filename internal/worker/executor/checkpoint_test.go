//go:build linux || darwin

package executor

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Autarquico/codefabric/internal/adapters/harness"
	v1 "github.com/Autarquico/codefabric/internal/api/v1"
	"github.com/Autarquico/codefabric/internal/config"
	"github.com/Autarquico/codefabric/internal/domain"
	"github.com/Autarquico/codefabric/internal/testutil/fakeharness"
	"github.com/Autarquico/codefabric/internal/worker/execwrap"
	"github.com/Autarquico/codefabric/internal/worker/gitops"
	"github.com/Autarquico/codefabric/internal/worker/state"
)

const testCheckpoint = "cp_01J0000000000000000000CP01"

// cpCtrl is a Controller that takes the captures of checkpoints.
type cpCtrl struct {
	*fakeController
	cmu       sync.Mutex
	tokens    []v1.CheckpointTokenRequest
	completes []v1.CheckpointCompleteRequest
	fails     []v1.CheckpointFailRequest
	blobs     map[string][]byte
	// onComplete and onPut run when the call arrives.
	onComplete func()
	onPut      func()
	// tokenErr, if set, is what CheckpointToken answers, and putErr PutNativeState.
	tokenErr error
	putErr   error
}

func (c *cpCtrl) CheckpointToken(_ context.Context, run string, req v1.CheckpointTokenRequest) (v1.RelayTokenResponse, error) {
	c.cmu.Lock()
	defer c.cmu.Unlock()
	c.tokens = append(c.tokens, req)
	if c.tokenErr != nil {
		return v1.RelayTokenResponse{}, c.tokenErr
	}
	return v1.RelayTokenResponse{Token: "cp-token-" + run, Path: "/v1/relay/project_1.git", Write: true, Ref: domain.CheckpointRef(domain.ID(req.CheckpointID))}, nil
}

func (c *cpCtrl) CompleteCheckpoint(_ context.Context, _, id string, req v1.CheckpointCompleteRequest) (v1.CheckpointReported, error) {
	if c.onComplete != nil {
		c.onComplete()
	}
	c.cmu.Lock()
	defer c.cmu.Unlock()
	c.completes = append(c.completes, req)
	return v1.CheckpointReported{CheckpointID: id, RunState: "suspended"}, nil
}

func (c *cpCtrl) FailCheckpoint(_ context.Context, _, id string, req v1.CheckpointFailRequest) (v1.CheckpointReported, error) {
	c.cmu.Lock()
	defer c.cmu.Unlock()
	c.fails = append(c.fails, req)
	return v1.CheckpointReported{CheckpointID: id}, nil
}

func (c *cpCtrl) PutNativeState(_ context.Context, _, _ string, _ uint64, digest string, r io.Reader, size int64) (v1.NativeStateUploaded, error) {
	if c.putErr != nil {
		return v1.NativeStateUploaded{}, c.putErr
	}
	if c.onPut != nil {
		c.onPut()
	}
	data, err := io.ReadAll(r)
	if err != nil {
		return v1.NativeStateUploaded{}, err
	}
	sum := sha256.Sum256(data)
	if "sha256:"+hex.EncodeToString(sum[:]) != digest || int64(len(data)) != size {
		return v1.NativeStateUploaded{}, domain.NewError(domain.CodeInvalidRequest, "the content does not hash to the digest", nil)
	}
	c.cmu.Lock()
	defer c.cmu.Unlock()
	if c.blobs == nil {
		c.blobs = map[string][]byte{}
	}
	c.blobs[digest] = data
	return v1.NativeStateUploaded{Digest: digest, Size: size}, nil
}

func (c *cpCtrl) OpenNativeState(context.Context, string, string, uint64) (io.ReadCloser, int64, error) {
	return nil, 0, domain.NewError(domain.CodeNotFound, "this checkpoint has no native state", nil)
}

// porterHarness is the fakeharness with a native session in a directory of the test.
type porterHarness struct {
	fakeharness.Adapter
	transcript string
	session    string
	located    *[]harness.SessionRef
	// beforeExport, if set, runs at the start of Export.
	beforeExport *func()
}

func (p porterHarness) Start(ctx context.Context, host harness.Host, run harness.RunSpec) (harness.Launch, error) {
	l, err := p.Adapter.Start(ctx, host, run)
	l.SessionID = p.session
	return l, err
}

func (p porterHarness) Locate(_ context.Context, ref harness.SessionRef) (harness.SessionFiles, error) {
	*p.located = append(*p.located, ref)
	return harness.SessionFiles{Transcript: p.transcript}, nil
}

func (p porterHarness) Export(_ context.Context, ref harness.SessionRef, _ harness.ExportOptions, w io.Writer) (harness.SessionManifest, error) {
	if p.beforeExport != nil && *p.beforeExport != nil {
		(*p.beforeExport)() // the harness writes on, between the check of the files and the export
	}
	data, err := os.ReadFile(p.transcript)
	if err != nil {
		return harness.SessionManifest{}, err
	}
	manifest := harness.SessionManifest{Adapter: p.ID(), Version: "9.9", SessionID: ref.SessionID}
	return manifest, harness.WriteSessionArchive(w, manifest, data, nil)
}

func (p porterHarness) Import(context.Context, harness.SessionImport, io.Reader) (harness.SessionImportResult, error) {
	return harness.SessionImportResult{}, harness.ErrUnsupported
}

type cpRig struct {
	*fixture
	ctrl    *cpCtrl
	git     *fakeGit
	logs    *syncBuffer
	located []harness.SessionRef
	// transcript is the file of the native session of the Runs.
	transcript string
	// withoutNativeState orders the capture of a Project that did not opt in.
	withoutNativeState bool
	// beforeExport runs when the adapter starts to export the session.
	beforeExport func()
}

func newCpRig(t *testing.T, mutate ...func(*Options)) *cpRig {
	t.Helper()
	r := &cpRig{git: &fakeGit{head: headOID}, logs: &syncBuffer{}, transcript: filepath.Join(t.TempDir(), "session.jsonl")}
	if err := os.WriteFile(r.transcript, []byte("{\"line\":1}\n{\"line\":2}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	ph := porterHarness{
		Adapter:    fakeharness.Adapter{Binary: "/fake/harness", Caps: harness.Capabilities{Modes: []domain.Mode{domain.ModeHeadless}, NativeCheckpoint: true}},
		transcript: r.transcript, session: "sess-1", located: &r.located, beforeExport: &r.beforeExport,
	}
	r.fixture = newFixture(t, append([]func(*Options){func(o *Options) {
		r.ctrl = &cpCtrl{fakeController: o.Controller.(*fakeController)}
		o.Controller, o.Git = r.ctrl, r.git
		o.Harnesses = registry(t, ph)
		o.Secrets = newFakeStore(map[string]string{"github.personal": ghValue})
		o.RedactPipeArgv = []string{"/cf", "worker", "redact-pipe"}
		o.CheckpointGrace = 10 * time.Second
		o.RelayTLS = RelayTLS{Cert: "/c/cert.pem", Key: "/c/key.pem", CA: "/c/ca.pem"}
		o.Log = slog.New(slog.NewTextHandler(r.logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
	}}, mutate...)...)
	return r
}

// running is a Run of the harness with a native session, started, whose
// wrapper is a process that writes to its worktree without stopping until it is
// asked to end (unless trap says otherwise).
func (r *cpRig) running(spec []byte, trap string) (state.Key, *helper) {
	r.t.Helper()
	k := r.acceptedSpec(spec)
	if r.e.step(r.t.Context(), k) {
		r.t.Fatal("the Run ended at its first step")
	}
	if got := r.run(k).Phase; got != state.PhaseRunning {
		r.t.Fatalf("phase = %s, want running", got)
	}
	busy := filepath.Join(r.t.TempDir(), "busy")
	w := newTrappingWrapperProcess(r.t, trap, "while :; do echo tick >> "+busy+"; sleep 0.01; done")
	r.writeWrapper(k, w.w)
	r.writeStarted(k, w.w, t0)
	return k, w
}

func (r *cpRig) order(k state.Key, exclude ...string) {
	r.t.Helper()
	r.orderOf(k, domain.CheckpointDefer, testCheckpoint, exclude...)
}

// orderOf is the order of the checkpoint id, for the trigger.
func (r *cpRig) orderOf(k state.Key, trigger domain.CheckpointTrigger, id string, exclude ...string) {
	r.t.Helper()
	r.nextID++
	cmd := v1.WorkerCommand{Seq: r.nextID, Type: domain.CommandCheckpoint, Checkpoint: &v1.CheckpointCommand{
		RunID: k.RunID, LeaseID: r.run(k).LeaseID, Epoch: k.LeaseEpoch, CheckpointID: id, Trigger: trigger,
		Ref: domain.CheckpointRef(domain.ID(id)), RunShort: "run-1", TaskID: "task_1", ProjectID: "project_1", Exclude: exclude,
		NativeState: !r.withoutNativeState,
	}}
	if blocked, err := r.e.persist(r.t.Context(), []v1.WorkerCommand{cmd}); err != nil || blocked {
		r.t.Fatalf("persist: blocked=%v err=%v", blocked, err)
	}
}

// settled waits for the harness to end, leaves the exit its wrapper would, and
// steps the Run until it is settled.
func (r *cpRig) settled(k state.Key, w *helper) {
	r.t.Helper()
	select {
	case <-w.ended:
	case <-time.After(10 * time.Second):
		r.t.Fatal("the harness was not asked to end")
	}
	r.writeExit(k, execwrap.Exit{ExitCode: intp(0), ExitedAt: t0.Add(time.Second), Cause: execwrap.CauseStop})
	waitFor(r.t, "the Run to be settled", func() bool { return r.e.step(r.t.Context(), k) })
}

func TestScenario_CheckpointCapture_CoherentCapture_Worker(t *testing.T) {
	r := newCpRig(t)
	k, w := r.running(fakeSpec("do it", ""), "trap 'exit 0' TERM")
	aliveAtSnapshot, phaseAtComplete := true, state.Phase("")
	r.git.snapshot = func(gitops.SnapshotIntent, gitops.Forbidden) (gitops.Sealed, error) {
		aliveAtSnapshot = w.alive()
		return gitops.Sealed{Commit: "4444444444444444444444444444444444444444"}, nil
	}
	r.ctrl.onComplete = func() { phaseAtComplete = r.run(k).Phase }

	r.order(k)
	if got := r.run(k).Phase; got != state.PhaseCheckpointing {
		t.Fatalf("phase = %s, want checkpointing", got)
	}
	r.settled(k, w)

	if aliveAtSnapshot {
		t.Error("the worktree was captured while the harness still wrote")
	}
	if phaseAtComplete != state.PhaseCheckpointing {
		t.Errorf("the capture was reported in phase %q: the Run was settled before it", phaseAtComplete)
	}
	if len(r.ctrl.completes) != 1 || len(r.ctrl.fails) != 0 || r.ctrl.completes[0].Commit != "4444444444444444444444444444444444444444" {
		t.Fatalf("completes = %+v, fails = %+v", r.ctrl.completes, r.ctrl.fails)
	}
	if len(r.git.pushes) != 1 || r.git.pushes[0].Branch != domain.CheckpointRef(testCheckpoint) || r.git.pushes[0].Auth.Token != "cp-token-"+k.RunID {
		t.Errorf("pushes = %+v", r.git.pushes)
	}
	// The exit that follows is reported as any other, after the capture, and the capture is closed.
	if got := kinds(r.reports(k)); !slices.Equal(got, []v1.RunReportKind{v1.ReportStarted, v1.ReportExited, v1.ReportStopped}) {
		t.Errorf("reports = %v", got)
	}
	if c, err := state.CaptureOf(t.Context(), r.db.Reader(), k); err != nil || c.Outcome != state.CaptureCompleted {
		t.Errorf("capture = %+v, %v", c, err)
	}
	if got := r.run(k).Phase; got != state.PhaseStopped {
		t.Errorf("phase = %s", got)
	}
}

func TestScenario_CheckpointCapture_Exclusions_Worker(t *testing.T) {
	for name, tc := range map[string]struct{ order, want []string }{
		"the exclusions of the order": {[]string{"**/.env*", "**/*.pem", "secrets/**"}, []string{"**/.env*", "**/*.pem", "secrets/**"}},
		"the default without any":     {nil, config.DefaultCheckpointExclude()},
	} {
		t.Run(name, func(t *testing.T) {
			r := newCpRig(t)
			k, w := r.running(fakeSpec("do it", ""), "trap 'exit 0' TERM")
			r.git.snapshot = func(gitops.SnapshotIntent, gitops.Forbidden) (gitops.Sealed, error) {
				return gitops.Sealed{Commit: headOID, Excluded: []string{".env", "k.pem"}}, nil
			}
			r.order(k, tc.order...)
			r.settled(k, w)
			if len(r.git.snapshots) != 1 || !slices.Equal(r.git.snapshots[0].Exclude, tc.want) {
				t.Errorf("snapshots = %+v, want exclude %v", r.git.snapshots, tc.want)
			}
			if len(r.ctrl.completes) != 1 || !slices.Equal(r.ctrl.completes[0].Excluded, []string{".env", "k.pem"}) {
				t.Errorf("completes = %+v", r.ctrl.completes)
			}
		})
	}
}

func TestScenario_CheckpointCapture_SessionAfterSettle_Worker(t *testing.T) {
	r := newCpRig(t)
	k, w := r.running(fakeSpec("do it", ""), "trap 'exit 0' TERM")
	if got := r.run(k).SessionID; got != "sess-1" {
		t.Fatalf("session persisted = %q", got)
	}
	phaseAtPut := state.Phase("")
	r.ctrl.onPut = func() { phaseAtPut = r.run(k).Phase }
	r.order(k)
	r.settled(k, w)

	if phaseAtPut != state.PhaseCheckpointing {
		t.Errorf("the session was uploaded in phase %q, after the Run settled", phaseAtPut)
	}
	if len(r.located) != 1 || r.located[0].SessionID != "sess-1" || r.located[0].Cwd != r.e.worktree(k) {
		t.Errorf("located = %+v", r.located)
	}
	if len(r.ctrl.completes) != 1 || r.ctrl.completes[0].NativeState == nil {
		t.Fatalf("completes = %+v", r.ctrl.completes)
	}
	ns := r.ctrl.completes[0].NativeState
	if ns.Adapter != fakeharness.DefaultID || ns.AdapterVersion != "9.9" || !strings.Contains(unarchived(t, r.ctrl.blobs[ns.Digest]), "{\"line\":1}") {
		t.Errorf("native state = %+v, blobs = %d", ns, len(r.ctrl.blobs))
	}
}

// unarchived is the text of the files of a tar.gz.
func unarchived(t *testing.T, archive []byte) string {
	t.Helper()
	gz, err := gzip.NewReader(bytes.NewReader(archive))
	if err != nil {
		t.Fatal(err)
	}
	var out strings.Builder
	tr := tar.NewReader(gz)
	for {
		if _, err := tr.Next(); err != nil {
			return out.String()
		}
		if _, err := io.Copy(&out, tr); err != nil {
			t.Fatal(err)
		}
	}
}

// What is scanned is what is uploaded: a secret the harness writes to the session after
// the files were looked at, before they are exported, does not travel.
func TestScenario_CheckpointCapture_SecretWrittenDuringExport_Worker(t *testing.T) {
	r := newCpRig(t)
	spec := secretSpecFor(fakeharness.DefaultID, "do it", "headless", map[string]string{"GH": "github.personal"})
	k, w := r.running(spec, "trap 'exit 0' TERM")
	r.beforeExport = func() {
		f, err := os.OpenFile(r.transcript, os.O_APPEND|os.O_WRONLY, 0o600)
		if err != nil {
			t.Error(err)
			return
		}
		defer f.Close()
		_, _ = f.WriteString("{\"tool\":\"" + ghValue + "\"}\n")
	}
	r.order(k)
	r.settled(k, w)

	for _, blob := range r.ctrl.blobs {
		if strings.Contains(unarchived(t, blob), ghValue) {
			t.Fatal("a secret written during the export was uploaded")
		}
	}
	if len(r.ctrl.completes) != 1 || r.ctrl.completes[0].NativeState != nil {
		t.Errorf("completes = %+v: the checkpoint is source and summary", r.ctrl.completes)
	}
}

func TestScenario_CheckpointCapture_SecretInSession_Worker(t *testing.T) {
	r := newCpRig(t)
	spec := secretSpecFor(fakeharness.DefaultID, "do it", "headless", map[string]string{"GH": "github.personal"})
	k, w := r.running(spec, "trap 'exit 0' TERM")
	if err := os.WriteFile(r.transcript, []byte("{\"line\":1}\n{\"tool\":\""+ghValue+"\"}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	r.order(k)
	r.settled(k, w)

	if len(r.ctrl.blobs) != 0 {
		t.Error("a session that holds a secret was uploaded")
	}
	if len(r.ctrl.completes) != 1 || r.ctrl.completes[0].NativeState != nil {
		t.Errorf("completes = %+v: the checkpoint is source and summary", r.ctrl.completes)
	}
	logs := r.logs.String()
	if !strings.Contains(logs, "sess-1.jsonl lines 2") || strings.Contains(logs, ghValue) {
		t.Errorf("the warning names the lines and never the value:\n%s", logs)
	}
	warnings := r.ctrl.completes[0].Warnings
	if len(warnings) != 1 || !strings.HasPrefix(warnings[0], "Secret in the session") ||
		!strings.Contains(warnings[0], "sess-1.jsonl lines 2") || strings.Contains(warnings[0], ghValue) {
		t.Errorf("the report warns of the lines, never of the value: %q", warnings)
	}
}

func TestScenario_CheckpointCapture_NativeStateOff_Worker(t *testing.T) {
	r := newCpRig(t)
	r.withoutNativeState = true
	k, w := r.running(fakeSpec("do it", ""), "trap 'exit 0' TERM")
	r.order(k)
	r.settled(k, w)

	if len(r.located) != 0 || len(r.ctrl.blobs) != 0 {
		t.Errorf("a Project without native_state had its session read (%d) or uploaded (%d)", len(r.located), len(r.ctrl.blobs))
	}
	if len(r.ctrl.completes) != 1 || r.ctrl.completes[0].NativeState != nil || len(r.ctrl.completes[0].Warnings) != 0 {
		t.Errorf("completes = %+v: the checkpoint is source and summary, with nothing to warn of", r.ctrl.completes)
	}
}

func TestScenario_CheckpointCapture_FailureWithLiveHarness_Worker(t *testing.T) {
	r := newCpRig(t)
	// Running by its record, but with no wrapper to stop: nothing can be quiesced.
	k := r.acceptedSpec(fakeSpec("do it", ""))
	r.e.step(t.Context(), k)
	r.order(k)

	if got := r.run(k).Phase; got != state.PhaseRunning {
		t.Errorf("phase = %s: nothing was touched", got)
	}
	if len(r.ctrl.fails) != 1 || !r.ctrl.fails[0].HarnessAlive || r.ctrl.fails[0].LeaseEpoch != k.LeaseEpoch {
		t.Fatalf("fails = %+v", r.ctrl.fails)
	}
	if _, err := state.CaptureOf(t.Context(), r.db.Reader(), k); err == nil {
		t.Error("a capture was stored")
	}
	if _, err := state.StopOrderOf(t.Context(), r.db.Reader(), k); err == nil {
		t.Error("a stop was ordered")
	}
}

func TestCheckpointCapture_FailureAfterQuiesceEndsItWithTheHarnessGone(t *testing.T) {
	r := newCpRig(t)
	k, w := r.running(fakeSpec("do it", ""), "trap 'exit 0' TERM")
	r.git.snapshot = func(gitops.SnapshotIntent, gitops.Forbidden) (gitops.Sealed, error) {
		return gitops.Sealed{}, os.ErrPermission
	}
	r.order(k)
	r.settled(k, w)
	if len(r.ctrl.fails) != 1 || r.ctrl.fails[0].HarnessAlive || len(r.ctrl.completes) != 0 {
		t.Errorf("fails = %+v, completes = %+v", r.ctrl.fails, r.ctrl.completes)
	}
}

func TestCheckpointCapture_AHarnessThatIgnoresTheSignalIsKilledAfterCheckpointGrace(t *testing.T) {
	r := newCpRig(t)
	k, w := r.running(fakeSpec("do it", ""), "trap '' TERM")
	r.order(k)
	r.e.step(t.Context(), k)
	if !w.alive() {
		t.Fatal("the harness ended before the grace")
	}
	r.clock.Advance(10*time.Second + stopMargin + time.Second)
	r.e.step(t.Context(), k)
	r.settled(k, w)
	if len(r.ctrl.completes) != 1 {
		t.Errorf("completes = %+v", r.ctrl.completes)
	}
}

func TestCheckpointCapture_AStopOfTheControllerAbortsTheCapture(t *testing.T) {
	r := newCpRig(t)
	k, w := r.running(fakeSpec("do it", ""), "trap 'exit 0' TERM")
	r.order(k)
	r.stopOrder(k, domain.StopCancel)
	if got := r.run(k).Phase; got != state.PhaseStopping {
		t.Fatalf("phase = %s", got)
	}
	r.settled(k, w)
	if len(r.ctrl.completes) != 0 || len(r.ctrl.fails) != 0 || len(r.git.snapshots) != 0 {
		t.Errorf("a capture the Controller ended was made: completes=%+v fails=%+v", r.ctrl.completes, r.ctrl.fails)
	}
	if c, err := state.CaptureOf(t.Context(), r.db.Reader(), k); err != nil || c.Outcome != state.CaptureAborted {
		t.Errorf("capture = %+v, %v", c, err)
	}
}

func TestCheckpointCapture_AnOrderDeliveredAgainIsOneCapture(t *testing.T) {
	r := newCpRig(t)
	k, w := r.running(fakeSpec("do it", ""), "trap 'exit 0' TERM")
	r.order(k)
	r.order(k)
	r.settled(k, w)
	if len(r.ctrl.completes) != 1 {
		t.Errorf("completes = %d", len(r.ctrl.completes))
	}
}
