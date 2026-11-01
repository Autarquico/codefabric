//go:build linux || darwin

package executor

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Autarquico/codefabric/internal/adapters/harness"
	"github.com/Autarquico/codefabric/internal/adapters/harness/command"
	"github.com/Autarquico/codefabric/internal/domain"
	"github.com/Autarquico/codefabric/internal/worker/state"
)

// fakeGlobal is the global configuration of the Worker: a lock per harness, and
// whether the Workflow is what it holds.
type fakeGlobal struct {
	mu      sync.Mutex
	locks   map[string]*sync.Mutex
	taken   map[string]int
	waiting chan string // a Lock is about to block or take the lock
	holds   error
	checked int
}

func newFakeGlobal() *fakeGlobal {
	return &fakeGlobal{locks: map[string]*sync.Mutex{}, taken: map[string]int{}, waiting: make(chan string, 8)}
}

func (g *fakeGlobal) Lock(id string) func() {
	g.mu.Lock()
	m, ok := g.locks[id]
	if !ok {
		m = &sync.Mutex{}
		g.locks[id] = m
	}
	g.taken[id]++
	g.mu.Unlock()
	g.waiting <- id
	m.Lock()
	return m.Unlock
}

func (g *fakeGlobal) Holds(context.Context, string, harness.NativeSnapshot) error {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.checked++
	return g.holds
}

func commandSpec(w *fakeWorkflows) []byte {
	m := map[string]any{
		"schema": 2, "prompt": "true", "mode": "headless",
		"requirements": map[string]any{"harness": command.ID},
	}
	if w != nil {
		m["workflow"] = "team@" + w.version.Digest
	}
	raw, _ := json.Marshal(m)
	return raw
}

func globalFixture(t *testing.T, w *fakeWorkflows, g *fakeGlobal) *fixture {
	t.Helper()
	return newFixture(t, func(o *Options) {
		o.Harnesses = registry(t, command.Adapter{}, isolated)
		o.Workflows, o.GlobalConfig = w, g
	})
}

func TestAWorkflowRunOfAHarnessWithoutIsolationIsAcceptedWhenTheWorkflowIsApplied(t *testing.T) {
	w := newFakeWorkflows(t, map[string]string{"settings.json": `{}`})
	g := newFakeGlobal()
	g.waiting = make(chan string, 64)
	f := globalFixture(t, w, g)
	k := f.acceptedSpec(commandSpec(w))
	if g.checked == 0 {
		t.Error("the global configuration was not checked")
	}
	// It runs with the global configuration: nothing is materialised for it and
	// no effective digest is reported.
	if f.e.step(t.Context(), k) {
		t.Fatalf("the Run ended: %+v", f.reports(k))
	}
	if _, err := os.Stat(filepath.Join(f.runDir(k), "config")); err == nil {
		t.Error("a configuration was materialised for a Run that uses the global one")
	}
	if digest, ok := startedDigest(f, k); !ok || digest != "" {
		t.Errorf("started carries %q (reported=%v), want none", digest, ok)
	}
}

func TestAWorkflowRunOfAHarnessWithoutIsolationIsRefusedWhenTheWorkflowIsNotApplied(t *testing.T) {
	w := newFakeWorkflows(t, map[string]string{"settings.json": `{}`})
	g := newFakeGlobal()
	g.waiting = make(chan string, 64)
	g.holds = errors.New("the component \"settings\" was edited on the host")
	f := globalFixture(t, w, g)
	run := state.Run{Key: state.Key{RunID: runID(1), LeaseEpoch: 1}, Spec: commandSpec(w), CPU: 1, MemoryMB: 1}
	v := f.e.evaluate(t.Context(), run, Limits{MaxConcurrentRuns: 4, CPU: 8, MemoryMB: 1024}, state.Load{})
	if v.reject != domain.CodeHarnessUnavailable || !strings.Contains(v.message, "edited on the host") || !strings.Contains(v.message, w.version.Digest) {
		t.Errorf("verdict = %+v", v)
	}
}

func TestAcceptingAWorkflowRunOfAHarnessWithoutIsolationWaitsForTheLockOfThatHarness(t *testing.T) {
	w := newFakeWorkflows(t, map[string]string{"settings.json": `{}`})
	g := newFakeGlobal()
	f := globalFixture(t, w, g)
	unlock := g.Lock(command.ID) // an apply in progress
	<-g.waiting
	k := f.assign(commandSpec(w))
	done := make(chan struct{})
	go func() { defer close(done); f.e.decideOnce(t.Context()) }()
	if id := <-g.waiting; id != command.ID {
		t.Fatalf("locked %q", id)
	}
	select {
	case <-done:
		t.Fatal("the Run was answered while the harness was locked")
	case <-time.After(50 * time.Millisecond):
	}
	if got := f.run(k).Phase; got != state.PhaseAssigned {
		t.Fatalf("phase = %s while locked, want assigned", got)
	}
	unlock()
	<-done
	if got := f.run(k).Phase; got != state.PhaseAccepted {
		t.Errorf("phase = %s after the apply, want accepted", got)
	}
}

func TestAcceptingARunOfAnIsolatedHarnessTakesNoLock(t *testing.T) {
	g := newFakeGlobal()
	f := globalFixture(t, nil, g)
	k := f.acceptedSpec(fakeSpec("do it", "")) // the fake harness has isolation
	if f.e.step(t.Context(), k) {
		t.Fatalf("the Run ended: %+v", f.reports(k))
	}
	if len(g.taken) != 0 {
		t.Errorf("locks taken for an isolated harness: %v", g.taken)
	}
}

func TestPreparingARunOfAHarnessWithoutIsolationTakesNoLock(t *testing.T) {
	g := newFakeGlobal()
	f := globalFixture(t, nil, g)
	k := f.acceptedSpec(commandSpec(nil))
	unlock := g.Lock(command.ID) // an apply, or another Run launching
	defer unlock()
	<-g.waiting
	done := make(chan error, 1)
	go func() { done <- f.e.prepare(t.Context(), f.run(k)) }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("prepare: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("prepare waited for the lock of the harness: a slow prepare would stall every Run of it")
	}
}

func TestADecisionOfAnotherHarnessIsNotHeldUpByTheLockOfAHarnessWithoutIsolation(t *testing.T) {
	g := newFakeGlobal()
	f := globalFixture(t, nil, g)
	unlock := g.Lock(command.ID)
	defer unlock()
	<-g.waiting
	k := f.assign(fakeSpec("do it", ""))
	done := make(chan struct{})
	go func() { defer close(done); f.e.decideOnce(t.Context()) }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("deciding an isolated Run waited for another harness")
	}
	if got := f.run(k).Phase; got != state.PhaseAccepted {
		t.Errorf("phase = %s, want accepted", got)
	}
}

func TestARunIsRefusedAtLaunchWhenTheGlobalConfigurationChangedAfterTheAccept(t *testing.T) {
	w := newFakeWorkflows(t, map[string]string{"settings.json": `{}`})
	g := newFakeGlobal()
	g.waiting = make(chan string, 64)
	f := globalFixture(t, w, g)
	k := f.acceptedSpec(commandSpec(w))
	g.mu.Lock()
	g.holds = errors.New("an apply came in between")
	g.mu.Unlock()
	if !f.e.step(t.Context(), k) {
		t.Fatalf("the Run was not settled: phase %s", f.run(k).Phase)
	}
	var msg string
	for _, en := range f.reports(k) {
		msg += en.Message + "|"
	}
	if !strings.Contains(msg, "an apply came in between") {
		t.Errorf("reports %q do not say why", msg)
	}
	if got := f.run(k).Phase; got.Live() {
		t.Errorf("phase = %s, want a Run that never ran", got)
	}
}
