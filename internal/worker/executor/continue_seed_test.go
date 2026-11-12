package executor

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"strings"
	"testing"

	"github.com/Autarquico/codefabric/internal/adapters/harness"
	v1 "github.com/Autarquico/codefabric/internal/api/v1"
	"github.com/Autarquico/codefabric/internal/domain"
	"github.com/Autarquico/codefabric/internal/testutil/fakeharness"
	"github.com/Autarquico/codefabric/internal/worker/state"
)

const (
	seedSession = "sess-of-the-client"
	seedArchive = "archive:" + seedSession
)

// seedCtrl serves, besides the native state of a checkpoint, the session a client seeded
// the Run with.
type seedCtrl struct {
	*resumeCtrl
	seed    []byte
	openErr error
	opens   []string
}

func (c *seedCtrl) OpenNativeSeed(_ context.Context, run string, _ uint64) (io.ReadCloser, int64, error) {
	c.opens = append(c.opens, run)
	if c.openErr != nil {
		return nil, 0, c.openErr
	}
	return io.NopCloser(bytes.NewReader(c.seed)), int64(len(c.seed)), nil
}

// newSeedRig is newResumeRig with a Controller that also serves seeds.
func newSeedRig(t *testing.T) (*resumeRig, *seedCtrl) {
	t.Helper()
	r := &resumeRig{
		git:  &fakeGit{head: headOID},
		logs: &syncBuffer{},
		h: &resumable{Adapter: fakeharness.Adapter{
			Binary: "/fake/harness", Caps: harness.Capabilities{Modes: []domain.Mode{domain.ModeHeadless}, Resume: true, NativeCheckpoint: true},
		}},
	}
	var sc *seedCtrl
	r.fixture = newFixture(t, func(o *Options) {
		r.ctrl = &resumeCtrl{cpCtrl: &cpCtrl{fakeController: o.Controller.(*fakeController)}, native: []byte(nativeArchive)}
		sc = &seedCtrl{resumeCtrl: r.ctrl, seed: []byte(seedArchive)}
		o.Controller, o.Git = sc, r.git
		o.Harnesses = registry(t, r.h)
		o.RelayTLS = RelayTLS{Cert: "/c/cert.pem", Key: "/c/key.pem", CA: "/c/ca.pem"}
		o.Log = slog.New(slog.NewTextHandler(r.logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
	})
	return r, sc
}

func seedRef(data []byte) *v1.NativeStateRef {
	return &v1.NativeStateRef{Adapter: fakeharness.DefaultID, AdapterVersion: "9.9", Digest: digestOf(data)}
}

// seededRun accepts the assignment of a Run that has a seed and no checkpoint, which is
// what `cf do --session` makes, and steps it through its prepare.
func seededRun(r *resumeRig, c v1.Continuation) state.Key {
	r.t.Helper()
	k := r.assign(fakeSpec("fix the build", ""), func(a *v1.AssignCommand) {
		a.SourceCommit, a.Continues = headOID, &c
	})
	if r.e.decideOnce(r.t.Context()) {
		r.t.Fatal("deciding wanted a retry")
	}
	r.e.step(r.t.Context(), k)
	return k
}

func TestSeed_ARunWithoutCheckpointResumesTheSessionOfItsClient(t *testing.T) {
	r, sc := newSeedRig(t)
	k := seededRun(r, v1.Continuation{Seed: seedRef(sc.seed)})

	if got := r.run(k).Phase; got != state.PhaseRunning {
		t.Fatalf("phase = %s, want running", got)
	}
	if len(r.h.starts) != 0 || len(r.h.resumes) != 1 {
		t.Fatalf("starts = %d, resumes = %d", len(r.h.starts), len(r.h.resumes))
	}
	got := r.h.resumes[0]
	if string(got.cp.Data) != seedArchive || got.run.Prompt != harness.ContinuePrompt {
		t.Errorf("resumed %q with %q", got.cp.Data, got.run.Prompt)
	}
	if len(sc.opens) != 1 || sc.opens[0] != k.RunID || len(r.ctrl.opens) != 0 {
		t.Errorf("seed opened %v, native state opened %v", sc.opens, r.ctrl.opens)
	}
	if got := r.run(k).SessionID; got != seedSession {
		t.Errorf("session persisted = %q", got)
	}
}

func TestSeed_ReplacesTheNativeStateOfTheCheckpoint(t *testing.T) {
	r, sc := newSeedRig(t)
	k, _ := r.continuing(v1.Continuation{Seed: seedRef(sc.seed), NativeState: r.native()})

	if got := r.run(k).Phase; got != state.PhaseRunning || len(r.h.resumes) != 1 || len(r.h.starts) != 0 {
		t.Fatalf("phase = %s, starts = %d, resumes = %d", got, len(r.h.starts), len(r.h.resumes))
	}
	if string(r.h.resumes[0].cp.Data) != seedArchive || len(r.ctrl.opens) != 0 {
		t.Errorf("resumed %q, native state opened %v", r.h.resumes[0].cp.Data, r.ctrl.opens)
	}
}

func TestSeed_AnUnusableSeedStartsFromTheSourceAndSaysWhy(t *testing.T) {
	cases := map[string]func(sc *seedCtrl) *v1.NativeStateRef{
		"the digest differs": func(sc *seedCtrl) *v1.NativeStateRef {
			ref := seedRef(sc.seed)
			ref.Digest = digestOf([]byte("another"))
			return ref
		},
		"another adapter": func(sc *seedCtrl) *v1.NativeStateRef {
			ref := seedRef(sc.seed)
			ref.Adapter = "other"
			return ref
		},
		"the Controller has none": func(sc *seedCtrl) *v1.NativeStateRef {
			sc.openErr = domain.NewError(domain.CodeNotFound, "no seed", nil)
			return seedRef(sc.seed)
		},
	}
	for name, mk := range cases {
		t.Run(name, func(t *testing.T) {
			r, sc := newSeedRig(t)
			k := seededRun(r, v1.Continuation{Seed: mk(sc)})
			if got := r.run(k).Phase; got != state.PhaseRunning || len(r.h.resumes) != 0 || len(r.h.starts) != 1 {
				t.Fatalf("phase = %s, starts = %d, resumes = %d", got, len(r.h.starts), len(r.h.resumes))
			}
			if r.h.starts[0].Prompt != "fix the build" {
				t.Errorf("a Run with no checkpoint has no summary to add: %q", r.h.starts[0].Prompt)
			}
			if !strings.Contains(r.logs.String(), "starts without the session it was seeded with") {
				t.Errorf("the reason was not logged:\n%s", r.logs.String())
			}
		})
	}
}

func TestSeed_AnUnusableSeedWithACheckpointFallsBackToItsNativeState(t *testing.T) {
	r, sc := newSeedRig(t)
	sc.openErr = domain.NewError(domain.CodeNotFound, "no seed", nil)
	k, _ := r.continuing(v1.Continuation{Seed: seedRef(sc.seed), NativeState: r.native()})
	if got := r.run(k).Phase; got != state.PhaseRunning || len(r.h.resumes) != 1 || string(r.h.resumes[0].cp.Data) != nativeArchive {
		t.Fatalf("phase = %s, resumes = %d", got, len(r.h.resumes))
	}
}

func TestSeed_ADownloadThatFailsIsTriedAgainAndNothingStarts(t *testing.T) {
	r, sc := newSeedRig(t)
	sc.openErr = domain.NewError(domain.CodeControllerUnreachable, "no route", nil)
	k := seededRun(r, v1.Continuation{Seed: seedRef(sc.seed)})
	if got := r.run(k).Phase; got != state.PhaseAccepted {
		t.Errorf("phase = %s, want accepted: the prepare is tried again", got)
	}
	if len(r.h.starts)+len(r.h.resumes) != 0 {
		t.Errorf("the harness was launched: starts %d, resumes %d", len(r.h.starts), len(r.h.resumes))
	}
}

func TestSeed_AWorkerThatCannotDownloadItStartsWithoutIt(t *testing.T) {
	r := newResumeRig(t) // its Controller has no OpenNativeSeed
	k := seededRun(r, v1.Continuation{Seed: seedRef([]byte(seedArchive))})
	if got := r.run(k).Phase; got != state.PhaseRunning || len(r.h.starts) != 1 || len(r.h.resumes) != 0 {
		t.Fatalf("phase = %s, starts = %d, resumes = %d", got, len(r.h.starts), len(r.h.resumes))
	}
}
