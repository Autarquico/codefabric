//go:build linux || darwin

package executor

import (
	"context"
	"slices"
	"testing"

	"github.com/Autarquico/codefabric/internal/worker/state"
)

// holdWorkflowFixture is holdFixture for a Run that froze a Workflow: its
// configuration is materialised, its harness runs, and its `started` is on hold.
func holdWorkflowFixture(t *testing.T) (*fixture, state.Key, string) {
	t.Helper()
	w := newFakeWorkflows(t, map[string]string{"settings.json": `{"theme":"dark"}`})
	a := structuredAdapter{lineAdapter{Adapter: isolated, seen: &linesSeen{}}}
	f := newFixture(t, func(o *Options) {
		o.Harnesses, o.Git, o.Workflows = registry(t, a), &fakeGit{head: headOID}, w
		o.RelayTLS = RelayTLS{Cert: "/c/cert.pem", Key: "/c/key.pem", CA: "/c/ca.pem"}
	})
	k := f.acceptedSpec(w.spec("do it"))
	if f.e.step(t.Context(), k) {
		t.Fatalf("the Run ended: %+v", f.reports(k))
	}
	digest, ok, err := state.ConfigDigestOf(t.Context(), f.db.Reader(), k)
	if err != nil || !ok {
		t.Fatalf("no digest stored (%v, %v)", ok, err)
	}
	h := newTrappingWrapperProcess(t, "trap 'exit 0' TERM", "while :; do sleep 0.05; done")
	f.writeWrapper(k, h.w)
	f.writeStarted(k, h.w, t0)
	if got := f.run(k); got.Phase != state.PhaseRunning || len(f.reports(k)) != 0 {
		t.Fatalf("phase %s, reports %+v: the start must be held", got.Phase, f.reports(k))
	}
	return f, k, digest
}

func TestTheDigestOfTheMaterialisationSurvivesARestartWhileTheStartIsHeld(t *testing.T) {
	f, k, digest := holdWorkflowFixture(t)
	f.restart("01J0000000000000000000000E2")
	f.e.step(t.Context(), k)
	f.appendEvents(k, "model m-1\n")
	f.e.step(t.Context(), k)
	got := startedReports(f, k)
	if len(got) != 1 || got[0].Model != "m-1" || got[0].ConfigDigest != digest {
		t.Fatalf("started = %+v, want the digest %s", got, digest)
	}
}

func TestTheDigestOfTheMaterialisationSurvivesARestartBeforeAHeldStartIsDue(t *testing.T) {
	f, k, digest := holdWorkflowFixture(t)
	f.restart("01J0000000000000000000000E2")
	f.clock.Advance(startHold)
	f.e.step(t.Context(), k)
	got := startedReports(f, k)
	if len(got) != 1 || got[0].ConfigDigest != digest {
		t.Fatalf("started = %+v, want the digest %s", got, digest)
	}
}

func TestTheDigestOfTheMaterialisationSurvivesARestartBeforeTheHarnessStarts(t *testing.T) {
	w := newFakeWorkflows(t, map[string]string{"settings.json": `{"theme":"dark"}`})
	f := workflowFixture(t, w)
	k := f.assign(w.spec("do it"))
	f.e.decideOnce(t.Context())
	f.e.step(t.Context(), k)
	want, ok, err := state.ConfigDigestOf(t.Context(), f.db.Reader(), k)
	if err != nil || !ok {
		t.Fatalf("no digest stored (%v, %v)", ok, err)
	}
	f.restart("01J0000000000000000000000E2")
	f.e.step(t.Context(), k)
	if digest, reported := startedDigest(f, k); !reported || digest != want {
		t.Errorf("started carries %q (reported=%v), want %s", digest, reported, want)
	}
}

// A digest that cannot be read is not one that was never stored: the first is an
// error, so that the report is tried again, the second starts without a digest.
func TestAStorageErrorReadingTheDigestIsNotAStartWithoutOne(t *testing.T) {
	w := newFakeWorkflows(t, map[string]string{"settings.json": `{"theme":"dark"}`})
	f := workflowFixture(t, w)
	k := f.acceptedSpec(w.spec("do it"))

	if digest, err := f.e.materializedDigest(t.Context(), f.db.Reader(), k); err != nil || digest != "" {
		t.Errorf("never stored: %q, %v; want none and no error", digest, err)
	}
	f.e.step(t.Context(), k)
	if digest, err := f.e.materializedDigest(t.Context(), f.db.Reader(), k); err != nil || digest == "" {
		t.Errorf("stored: %q, %v", digest, err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if digest, err := f.e.materializedDigest(ctx, f.db.Reader(), k); err == nil || digest != "" {
		t.Errorf("storage error: %q, %v; want it returned", digest, err)
	}
}

// The secrets only the configuration of the frozen Workflow uses are asked for
// by the Worker, though the spec of the Run declares none.
func TestTheSecretsOfAWorkflowConfigurationAreGrantedThoughTheSpecDeclaresNone(t *testing.T) {
	w := newFakeWorkflows(t, map[string]string{"settings.json": `{"token":"{{cf-secret:team/token}}"}`})
	f := secretFixture(t, newFakeStore(map[string]string{"team/token": "plain-value"}), func(o *Options) {
		o.Harnesses, o.Workflows = registry(t, escapingHarness()), w
	})
	k := f.acceptedSpec(w.spec("do it"))
	if f.e.step(t.Context(), k) {
		t.Fatalf("the Run ended: %+v", f.reports(k))
	}
	grants := f.ctrl.grantRequests()
	if len(grants) != 1 || !slices.Equal(grants[0].Refs, []string{"team/token"}) {
		t.Errorf("grants = %+v", grants)
	}
	if digest, ok := startedDigest(f, k); !ok || digest == "" {
		t.Errorf("started carries %q (reported=%v)", digest, ok)
	}
}
