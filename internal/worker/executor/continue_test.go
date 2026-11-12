package executor

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"log/slog"
	"strings"
	"sync"
	"testing"

	"github.com/Autarquico/codefabric/internal/adapters/harness"
	v1 "github.com/Autarquico/codefabric/internal/api/v1"
	"github.com/Autarquico/codefabric/internal/domain"
	"github.com/Autarquico/codefabric/internal/testutil/fakeharness"
	"github.com/Autarquico/codefabric/internal/worker/gitops"
	"github.com/Autarquico/codefabric/internal/worker/state"
)

const (
	parentSession = "sess-of-the-parent"
	nativeArchive = "archive:" + parentSession
)

// resumable is the fakeharness with a native session that Resume restores from
// the archive of a checkpoint, and that records what it was asked.
type resumable struct {
	fakeharness.Adapter
	mu      sync.Mutex
	starts  []harness.RunSpec
	resumes []resumed
}

type resumed struct {
	run harness.RunSpec
	cp  harness.Checkpoint
}

func (r *resumable) Start(ctx context.Context, host harness.Host, run harness.RunSpec) (harness.Launch, error) {
	r.mu.Lock()
	r.starts = append(r.starts, run)
	r.mu.Unlock()
	l, err := r.Adapter.Start(ctx, host, run)
	l.SessionID = "sess-new"
	return l, err
}

func (r *resumable) Resume(ctx context.Context, host harness.Host, run harness.RunSpec, cp harness.Checkpoint) (harness.Launch, error) {
	r.mu.Lock()
	r.resumes = append(r.resumes, resumed{run, cp})
	r.mu.Unlock()
	session, ok := strings.CutPrefix(string(cp.Data), "archive:")
	if !ok {
		return harness.Launch{}, errors.New("not an archive")
	}
	l, err := r.Adapter.Resume(ctx, host, run, harness.Checkpoint{Harness: cp.Harness, Format: fakeharness.CheckpointFormat, Data: []byte(session)})
	l.SessionID = session
	return l, err
}

func (*resumable) CheckpointFormat() string { return "fake-archive/v1" }

// resumeCtrl serves the native state of a checkpoint.
type resumeCtrl struct {
	*cpCtrl
	native  []byte
	openErr error
	opens   []string
}

func (c *resumeCtrl) OpenNativeState(_ context.Context, run, checkpoint string, _ uint64) (io.ReadCloser, int64, error) {
	c.opens = append(c.opens, run+"/"+checkpoint)
	if c.openErr != nil {
		return nil, 0, c.openErr
	}
	return io.NopCloser(bytes.NewReader(c.native)), int64(len(c.native)), nil
}

type resumeRig struct {
	*fixture
	ctrl *resumeCtrl
	git  *fakeGit
	h    *resumable
	logs *syncBuffer
}

func newResumeRig(t *testing.T) *resumeRig {
	t.Helper()
	r := &resumeRig{
		git:  &fakeGit{head: headOID},
		logs: &syncBuffer{},
		h: &resumable{Adapter: fakeharness.Adapter{
			Binary: "/fake/harness", Caps: harness.Capabilities{Modes: []domain.Mode{domain.ModeHeadless}, Resume: true, NativeCheckpoint: true},
		}},
	}
	r.fixture = newFixture(t, func(o *Options) {
		r.ctrl = &resumeCtrl{cpCtrl: &cpCtrl{fakeController: o.Controller.(*fakeController)}, native: []byte(nativeArchive)}
		o.Controller, o.Git = r.ctrl, r.git
		o.Harnesses = registry(t, r.h)
		o.RelayTLS = RelayTLS{Cert: "/c/cert.pem", Key: "/c/key.pem", CA: "/c/ca.pem"}
		o.Log = slog.New(slog.NewTextHandler(r.logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
	})
	return r
}

func digestOf(data []byte) string {
	sum := sha256.Sum256(data)
	return "sha256:" + hex.EncodeToString(sum[:])
}

func (r *resumeRig) native() *v1.NativeStateRef {
	return &v1.NativeStateRef{Adapter: fakeharness.DefaultID, AdapterVersion: "9.9", Digest: digestOf(r.ctrl.native)}
}

var testSummary = v1.CheckpointSummary{Progress: "3 commits", Next: "run the tests", Decisions: "use sqlite", Commands: "go test ./..."}

// continuing accepts the assignment of a Run that continues the checkpoint and
// steps it through its prepare.
func (r *resumeRig) continuing(c v1.Continuation) (state.Key, bool) {
	r.t.Helper()
	c.ParentRunID, c.CheckpointID, c.Ref = "run_01J0000000000000000000PARENT", testCheckpoint, domain.CheckpointRef(testCheckpoint)
	c.Summary = testSummary
	k := r.assign(fakeSpec("fix the build", ""), func(a *v1.AssignCommand) {
		a.SourceCommit, a.Continues = headOID, &c
	})
	if r.e.decideOnce(r.t.Context()) {
		r.t.Fatal("deciding wanted a retry")
	}
	return k, r.e.step(r.t.Context(), k)
}

func TestScenario_ResumeEnOtroWorker_PrepareTakesTheSourceFromTheRelayAndNotFromOrigin(t *testing.T) {
	r := newResumeRig(t)
	var prepared gitops.PrepareSpec
	r.git.onPrepare = func(s gitops.PrepareSpec, _ string) error { prepared = s; return nil }

	k, _ := r.continuing(v1.Continuation{NativeState: r.native()})

	if prepared.Relay == nil {
		t.Fatalf("Prepare was given no relay source: %+v", prepared)
	}
	if prepared.Relay.Ref != domain.CheckpointRef(testCheckpoint) || prepared.SourceCommit != headOID {
		t.Errorf("relay ref %q, commit %q", prepared.Relay.Ref, prepared.SourceCommit)
	}
	a := prepared.Relay.Auth
	if a.URL != "https://controller.test/v1/relay/project_1.git" || a.Token != "token-"+k.RunID || a.Cert != "/c/cert.pem" || a.CA != "/c/ca.pem" {
		t.Errorf("relay auth = %+v", a)
	}
	if prepared.Auth != nil {
		t.Errorf("the credential of the origin was used: %+v", prepared.Auth)
	}
	if prepared.Branch == "" {
		t.Error("the branch of the new Run is empty: the result has nowhere to go")
	}
}

func TestScenario_ResumeEnOtroWorker_OrdinaryRunsKeepTheOrigin(t *testing.T) {
	r := newResumeRig(t)
	var prepared gitops.PrepareSpec
	r.git.onPrepare = func(s gitops.PrepareSpec, _ string) error { prepared = s; return nil }
	r.e.step(t.Context(), r.acceptedSpec(fakeSpec("plain", "")))
	if prepared.Relay != nil || prepared.Repo == "" {
		t.Errorf("an ordinary Run was prepared with %+v", prepared)
	}
}

func TestScenario_SesionReanudada(t *testing.T) {
	r := newResumeRig(t)
	k, _ := r.continuing(v1.Continuation{NativeState: r.native(), Warnings: nil})

	if got := r.run(k).Phase; got != state.PhaseRunning {
		t.Fatalf("phase = %s, want running", got)
	}
	if len(r.h.starts) != 0 || len(r.h.resumes) != 1 {
		t.Fatalf("starts = %d, resumes = %d", len(r.h.starts), len(r.h.resumes))
	}
	got := r.h.resumes[0]
	if string(got.cp.Data) != nativeArchive || got.cp.Harness != fakeharness.DefaultID || got.cp.Format != "fake-archive/v1" {
		t.Errorf("checkpoint = %s %s %q", got.cp.Harness, got.cp.Format, got.cp.Data)
	}
	if got.run.Worktree != r.e.worktree(k) || got.run.RunID != domain.ID(k.RunID) || got.run.Prompt != harness.ContinuePrompt {
		t.Errorf("run spec = %+v", got.run)
	}
	if want := k.RunID + "/" + testCheckpoint; len(r.ctrl.opens) != 1 || r.ctrl.opens[0] != want {
		t.Errorf("native state opened as %v, want %s", r.ctrl.opens, want)
	}
	// The session the Launch names is the Run's, and its Handle carries it.
	if got := r.run(k).SessionID; got != parentSession {
		t.Errorf("session persisted = %q", got)
	}
	h, err := r.e.handleOf(r.h, r.run(k), parseSpec(r.run(k).Spec))
	if err != nil || h.SessionID != parentSession {
		t.Errorf("handle = %+v, %v", h, err)
	}
}

func TestScenario_MajorDistinto_ContinuesFromSourceAndSummary(t *testing.T) {
	r := newResumeRig(t)
	why := "the native state was written by fakeharness 1.0, another major version than the Worker's: the Run continues from the source and the summary"
	k, _ := r.continuing(v1.Continuation{Warnings: []string{why}})

	if got := r.run(k).Phase; got != state.PhaseRunning {
		t.Fatalf("phase = %s, want running", got)
	}
	if len(r.h.resumes) != 0 || len(r.h.starts) != 1 || len(r.ctrl.opens) != 0 {
		t.Fatalf("starts = %d, resumes = %d, downloads = %v", len(r.h.starts), len(r.h.resumes), r.ctrl.opens)
	}
	prompt := r.h.starts[0].Prompt
	for _, want := range []string{"fix the build", "3 commits", "run the tests", "use sqlite", "go test ./..."} {
		if !strings.Contains(prompt, want) {
			t.Errorf("the prompt lacks %q:\n%s", want, prompt)
		}
	}
	if !strings.Contains(r.logs.String(), "another major version") {
		t.Errorf("the warning was not logged:\n%s", r.logs.String())
	}
	if got := r.run(k).SessionID; got != "sess-new" {
		t.Errorf("session = %q: it is a new one", got)
	}
}

func TestScenario_NativeStateOff_ContinuesFromSourceAndSummary(t *testing.T) {
	r := newResumeRig(t)
	k, _ := r.continuing(v1.Continuation{Warnings: []string{"the checkpoint holds no native state: the Run continues from the source and the summary"}})
	if got := r.run(k).Phase; got != state.PhaseRunning || len(r.h.resumes) != 0 || len(r.h.starts) != 1 || len(r.ctrl.opens) != 0 {
		t.Fatalf("phase = %s, starts = %d, resumes = %d, downloads = %v", got, len(r.h.starts), len(r.h.resumes), r.ctrl.opens)
	}
	if !strings.Contains(r.h.starts[0].Prompt, "3 commits") {
		t.Errorf("the prompt lacks the summary: %s", r.h.starts[0].Prompt)
	}
}

func TestResume_NativeStateThatCannotBeUsedFallsBackToTheSummary(t *testing.T) {
	cases := map[string]func(r *resumeRig) *v1.NativeStateRef{
		"the digest differs": func(r *resumeRig) *v1.NativeStateRef {
			n := r.native()
			n.Digest = digestOf([]byte("another"))
			return n
		},
		"another adapter": func(r *resumeRig) *v1.NativeStateRef {
			n := r.native()
			n.Adapter = "other"
			return n
		},
		"the Controller has none": func(r *resumeRig) *v1.NativeStateRef {
			r.ctrl.openErr = domain.NewError(domain.CodeNotFound, "this checkpoint has no native state", nil)
			return r.native()
		},
	}
	for name, mk := range cases {
		t.Run(name, func(t *testing.T) {
			r := newResumeRig(t)
			k, _ := r.continuing(v1.Continuation{NativeState: mk(r)})
			if got := r.run(k).Phase; got != state.PhaseRunning || len(r.h.resumes) != 0 || len(r.h.starts) != 1 {
				t.Fatalf("phase = %s, starts = %d, resumes = %d", got, len(r.h.starts), len(r.h.resumes))
			}
			if !strings.Contains(r.h.starts[0].Prompt, "3 commits") || !strings.Contains(r.logs.String(), "continues without the native state") {
				t.Errorf("prompt %q, log:\n%s", r.h.starts[0].Prompt, r.logs.String())
			}
		})
	}
}

func TestResume_ADownloadThatFailsIsTriedAgainAndNothingStarts(t *testing.T) {
	r := newResumeRig(t)
	r.ctrl.openErr = domain.NewError(domain.CodeControllerUnreachable, "no route", nil)
	k, _ := r.continuing(v1.Continuation{NativeState: r.native()})
	if got := r.run(k).Phase; got != state.PhaseAccepted {
		t.Errorf("phase = %s, want accepted: the prepare is tried again", got)
	}
	if len(r.h.starts)+len(r.h.resumes) != 0 {
		t.Errorf("the harness was launched: starts %d, resumes %d", len(r.h.starts), len(r.h.resumes))
	}
}

func TestResume_AContinuationThatNamesAnotherRefIsRefused(t *testing.T) {
	r := newResumeRig(t)
	r.git.onPrepare = func(gitops.PrepareSpec, string) error {
		t.Error("Prepare ran for a continuation with a ref that is not its checkpoint's")
		return nil
	}
	k := r.assign(fakeSpec("x", ""), func(a *v1.AssignCommand) {
		a.Continues = &v1.Continuation{CheckpointID: testCheckpoint, Ref: "refs/heads/main"}
	})
	if r.e.decideOnce(t.Context()) {
		t.Fatal("deciding wanted a retry")
	}
	r.e.step(t.Context(), k)
	if got := r.run(k).Phase; got == state.PhaseRunning {
		t.Errorf("phase = %s", got)
	}
}
