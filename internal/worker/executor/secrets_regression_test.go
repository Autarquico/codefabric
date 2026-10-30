//go:build linux || darwin

package executor

import (
	"testing"

	v1 "github.com/Autarquico/codefabric/internal/api/v1"
	"github.com/Autarquico/codefabric/internal/worker/state"
)

// Directed regressions of what the properties of the secrets found (T18): each
// on the world of the property, with nothing lost and nothing crashing.

// calmSecretWorld is the world of the property with every secret held and
// allowed, nothing lost, and one Run of a Task with secrets accepted here and
// not prepared yet.
func calmSecretWorld(t *testing.T) (*secretWorld, state.Key, string) {
	t.Helper()
	s := newSecretWorld(t, 1)
	s.calm = true
	for _, ref := range secretRefsOfWorld {
		s.setSecret(ref)
		s.setAllowed(ref, true)
	}
	s.heartbeat()
	if !s.f.e.Reconciled() {
		s.reconcile(false)
	}
	s.createTask()
	_, err := s.ctrl.app.Scheduler.Pass(t.Context())
	must(t, err)
	s.longPoll(false)
	s.f.e.decideOnce(t.Context())
	live, err := state.LiveRuns(t.Context(), s.f.db.Reader())
	must(t, err)
	if len(live) != 1 || live[0].Phase != state.PhaseAccepted {
		t.Fatalf("live = %+v", live)
	}
	return s, live[0].Key, s.tasks[0]
}

// The recovery of a new daemon prepares its Runs before the Controller took its
// first heartbeat, when it holds the previous daemon as the current one: a
// grant asked then is refused as daemon_retired and fences the Run. None is
// asked until the heartbeat is taken, and the Run is prepared right after.
func TestNoGrantIsAskedBeforeTheControllerTookAHeartbeatOfTheDaemon(t *testing.T) {
	s, k, _ := calmSecretWorld(t)
	s.lastGrant = nil
	s.world.restart(false) // its recovery prepares the Run
	if s.lastGrant != nil {
		t.Fatalf("a grant was asked before the first heartbeat: %+v", *s.lastGrant)
	}
	if r := s.f.run(k); r.FencedCode != "" || r.Phase != state.PhaseAccepted {
		t.Fatalf("phase %s, fenced %q", r.Phase, r.FencedCode)
	}
	s.gen++
	s.seq = 0
	s.heartbeat()
	s.reconcile(false)
	s.stepAll()    // resolves the configuration of the repo...
	s.flush(false) // ...which the Controller confirms...
	s.f.clock.Advance(prepareRetryMax)
	s.stepAll() // ...and the Run is prepared
	if g := s.lastGrant; g == nil || !g.granted {
		t.Fatalf("the grant after the heartbeat: %+v", g)
	}
	if r := s.f.run(k); r.FencedCode != "" || r.Phase == state.PhaseAccepted || !s.f.delivered(k) {
		t.Fatalf("phase %s, fenced %q, delivered %v", r.Phase, r.FencedCode, s.f.delivered(k))
	}
}

// A Run fenced by a refused grant is stopped by the Controller between the
// answer of the reconciliation and its use: the verdict no longer applies, and
// its `stopped` waits behind the fence. Another reconciliation settles it, and
// the Controller releases its reservation.
func TestAFencedRunStoppedWhileItsVerdictIsOnItsWayIsSettled(t *testing.T) {
	s, k, task := calmSecretWorld(t)
	_, err := s.ctrl.app.Tasks.Cancel(t.Context(), s.owner, task, "", v1.ActionRequest{})
	must(t, err)
	s.stepAll() // the configuration is resolved
	s.flush(false)
	s.f.clock.Advance(prepareRetryMax)
	s.stepAll() // the grant is refused: the Run is no longer preparable
	if r := s.f.run(k); r.FencedCode == "" {
		t.Fatalf("the refused grant did not fence the Run: %+v", r)
	}
	s.ctrl.mu.Lock()
	s.ctrl.between = func() { s.longPoll(false) } // the stop arrives between the answer and its use
	s.ctrl.mu.Unlock()
	gen := s.f.e.reconcileGen.Load()
	if err := s.f.e.reconcile(t.Context(), gen); err == nil && s.f.e.reconcileGen.Load() == gen {
		s.f.e.reconciledGen.Store(gen)
	}
	if r := s.f.run(k); r.Phase.Live() {
		t.Fatalf("the stop was not carried out: %s", r.Phase)
	}
	for range 5 {
		if !s.f.e.Reconciled() {
			s.reconcile(false)
		}
		s.flush(false)
	}
	if !s.quiet() {
		s.dump()
		t.Fatal("the stopped Run is not settled: its report is held behind the fence")
	}
}
