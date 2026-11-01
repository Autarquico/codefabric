package workers_test

import (
	"context"
	"errors"
	"log/slog"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	v1 "github.com/Autarquico/codefabric/internal/api/v1"
	"github.com/Autarquico/codefabric/internal/app/workers"
	"github.com/Autarquico/codefabric/internal/controller/store"
	"github.com/Autarquico/codefabric/internal/domain"
)

func TestScenario_WorkerRegistryHeartbeat_Offline(t *testing.T) {
	e := newEnv(t, controllerVersion)
	w := e.join("box")
	epoch := newEpoch()
	if got := e.beat(w, request(epoch)); got != domain.WorkerOnline {
		t.Fatalf("first heartbeat: %s", got)
	}

	// 45 s of silence is exactly the TTL: the Worker is still alive.
	e.clock.Advance(offlineTTL)
	e.sweep()
	if got := e.state(w); got != domain.WorkerOnline {
		t.Fatalf("at the TTL: %s", got)
	}
	e.clock.Advance(time.Second)
	e.sweep()
	if got := e.state(w); got != domain.WorkerOffline {
		t.Fatalf("past the TTL: %s", got)
	}
	lost := e.events(domain.EventWorkerLost)
	if len(lost) != 1 || lost[0].WorkerID != w.Principal.ID || lost[0].Level != "warn" || lost[0].Actor.Kind != domain.ActorSystem {
		t.Fatalf("WorkerLost = %+v", lost)
	}

	// Sweeping a Worker that is already offline changes and emits nothing.
	e.clock.Advance(time.Hour)
	e.sweep()
	e.sweep()
	if n := len(e.events(domain.EventWorkerLost)); n != 1 {
		t.Errorf("%d WorkerLost events after more sweeps", n)
	}

	// The heartbeat brings it back to where it was.
	if got := e.beat(w, request(epoch)); got != domain.WorkerOnline {
		t.Fatalf("after the return: %s", got)
	}
	back := e.events(domain.EventWorkerReconnected)
	if len(back) != 1 || back[0].Data["restarted"] != false || back[0].Actor.Kind != domain.ActorWorker {
		t.Fatalf("WorkerReconnected = %+v", back)
	}
	if n := len(e.events(domain.EventWorkerRestarted)); n != 0 {
		t.Errorf("%d WorkerRestarted for a daemon that never restarted", n)
	}
}

func TestOffline_ARestartedDaemonThatComesBackIsReconnectedNotRestarted(t *testing.T) {
	e := newEnv(t, controllerVersion)
	w := e.join("box")
	e.beat(w, request(newEpoch()))
	e.clock.Advance(offlineTTL + time.Second)
	e.sweep()

	e.beat(w, request(newEpoch()))

	back := e.events(domain.EventWorkerReconnected)
	if len(back) != 1 || back[0].Data["restarted"] != true {
		t.Fatalf("WorkerReconnected = %+v, want one that says the epoch changed", back)
	}
	if n := len(e.events(domain.EventWorkerRestarted)); n != 0 {
		t.Errorf("%d WorkerRestarted: coming back from offline is a reconnection", n)
	}
}

func TestOffline_ReconnectedSaysTheStateTheVerdictLeavesTheWorkerIn(t *testing.T) {
	e := newEnv(t, controllerVersion)
	w := e.join("box")
	epoch := newEpoch()
	e.beat(w, request(epoch))
	e.clock.Advance(offlineTTL + time.Second)
	e.sweep()

	if got := e.beat(w, request(epoch, withVersion("v0.1.0"))); got != domain.WorkerDegraded {
		t.Fatalf("back as %s, want degraded", got)
	}

	back := e.events(domain.EventWorkerReconnected)
	if len(back) != 1 || back[0].Data["state"] != string(domain.WorkerDegraded) {
		t.Fatalf("WorkerReconnected = %+v, want one that says degraded", back)
	}
}

func TestOffline_ADegradedWorkerComesBackDegraded(t *testing.T) {
	e := newEnv(t, controllerVersion)
	w := e.join("box")
	epoch := newEpoch()
	e.beat(w, request(epoch, withVersion("v0.1.0")))
	e.clock.Advance(offlineTTL + time.Second)
	e.sweep()
	if got := e.state(w); got != domain.WorkerOffline {
		t.Fatal(got)
	}

	if got := e.beat(w, request(epoch, withVersion("v0.1.0"))); got != domain.WorkerDegraded {
		t.Errorf("back as %s, want degraded", got)
	}
}

func TestOffline_ADrainingWorkerComesBackDraining(t *testing.T) {
	e := newEnv(t, controllerVersion)
	w := e.join("box")
	epoch := newEpoch()
	e.beat(w, request(epoch))
	run := e.seedRun(string(w.Principal.ID), domain.RunRunning)
	_, err := e.svc.Drain(t.Context(), e.owner, "box", v1.ActionRequest{})
	must(t, err)
	e.clock.Advance(offlineTTL + time.Second)
	e.sweep()
	if got := e.state(w); got != domain.WorkerOffline {
		t.Fatal(got)
	}
	if got := e.beat(w, request(epoch)); got != domain.WorkerDraining {
		t.Errorf("back as %s, want draining", got)
	}
	e.finishRun(run)
	if got := e.beat(w, request(epoch)); got != domain.WorkerDrained {
		t.Errorf("after its last Run ended: %s", got)
	}
}

func TestSweep_AJoinThatNeverHeartbeatedGoesOfflineCountingFromCreation(t *testing.T) {
	e := newEnv(t, controllerVersion)
	w := e.join("box")

	e.clock.Advance(offlineTTL)
	e.sweep()
	if got := e.state(w); got != domain.WorkerJoining {
		t.Fatalf("at the TTL: %s", got)
	}
	e.clock.Advance(time.Second)
	e.sweep()
	if got := e.state(w); got != domain.WorkerOffline {
		t.Fatalf("past the TTL: %s", got)
	}
	lost := e.events(domain.EventWorkerLost)
	if len(lost) != 1 || lost[0].Data["was"] != string(domain.WorkerJoining) {
		t.Fatalf("WorkerLost = %+v", lost)
	}

	// Its daemon finally starts: that is its registration, not a reconnection.
	if got := e.beat(w, request(newEpoch())); got != domain.WorkerOnline {
		t.Errorf("first heartbeat after the silence: %s", got)
	}
	if got := e.events(domain.EventWorkerReconnected, domain.EventWorkerRestarted); len(got) != 0 {
		t.Errorf("registering emitted %+v", got)
	}
}

func TestSweep_AControllerThatJustStartedWaitsATTLBeforeLosingAnyone(t *testing.T) {
	e := newEnv(t, controllerVersion)
	w := e.join("box")
	e.beat(w, request(newEpoch()))

	// The Controller was down for an hour; the Worker's last heartbeat is old.
	e.clock.Advance(time.Hour)
	restarted := workers.New(e.db, e.clock, workers.Config{
		ControllerVersion: controllerVersion, OfflineTTL: offlineTTL, StartedAt: e.clock.Now(),
	})
	sweep := func() { must(t, restarted.Sweep(t.Context())) }

	sweep()
	e.clock.Advance(offlineTTL)
	sweep()
	if got := e.state(w); got != domain.WorkerOnline || len(e.events(domain.EventWorkerLost)) != 0 {
		t.Fatalf("a TTL after the Controller started: %s", got)
	}
	e.clock.Advance(time.Second)
	sweep()
	if got := e.state(w); got != domain.WorkerOffline {
		t.Errorf("past the TTL since the Controller started: %s", got)
	}
	if lost := e.events(domain.EventWorkerLost); len(lost) != 1 {
		t.Errorf("WorkerLost = %+v", lost)
	}
}

func TestSweep_AWorkerThatHeartbeatedAfterTheStartIsMeasuredFromItsHeartbeat(t *testing.T) {
	e := newEnv(t, controllerVersion)
	w := e.join("box")
	started := e.clock.Now()
	e.clock.Advance(30 * time.Second)
	e.beat(w, request(newEpoch()))
	restarted := workers.New(e.db, e.clock, workers.Config{ControllerVersion: controllerVersion, OfflineTTL: offlineTTL, StartedAt: started})

	e.clock.Advance(offlineTTL + time.Second)
	must(t, restarted.Sweep(t.Context()))
	if got := e.state(w); got != domain.WorkerOffline {
		t.Errorf("silent for more than a TTL since its heartbeat: %s", got)
	}
}

func TestSweep_OnlyLooksAtLiveWorkers(t *testing.T) {
	e := newEnv(t, controllerVersion)
	gone := e.join("gone")
	e.beat(gone, request(newEpoch()))
	_, err := e.svc.Remove(t.Context(), e.owner, "gone", v1.ActionRequest{})
	must(t, err)
	live := e.join("live")
	e.beat(live, request(newEpoch()))

	e.clock.Advance(time.Hour)
	e.sweep()

	lost := e.events(domain.EventWorkerLost)
	if len(lost) != 1 || lost[0].WorkerID != live.Principal.ID {
		t.Errorf("WorkerLost = %+v, want only the live Worker's", lost)
	}
	if got := e.state(gone); got != domain.WorkerRemoved {
		t.Errorf("the removed Worker is %s", got)
	}
}

func TestScenario_WorkerRegistryHeartbeat_VersionIncompatible(t *testing.T) {
	e := newEnv(t, controllerVersion)
	for _, tt := range []struct {
		version    string
		want       domain.WorkerState
		compatible bool
	}{
		{"v0.3.1", domain.WorkerOnline, true},    // N
		{"v0.3.0", domain.WorkerOnline, true},    // N, another patch
		{"v0.2.7", domain.WorkerOnline, true},    // N-1
		{"v0.1.9", domain.WorkerDegraded, false}, // N-2
		{"v0.4.0", domain.WorkerDegraded, false}, // newer than the Controller
		{"dev", domain.WorkerDegraded, false},    // a development build against a release
		{"", domain.WorkerDegraded, false},
	} {
		t.Run("worker "+tt.version, func(t *testing.T) {
			w := e.join("w-"+tt.version+"x", v1.WorkerSettings{})
			if got := e.beat(w, request(newEpoch(), withVersion(tt.version))); got != tt.want {
				t.Fatalf("registered as %s, want %s", got, tt.want)
			}
			view := e.worker(string(w.Principal.ID))
			if view.State != tt.want || view.Compatible != tt.compatible || view.DaemonVersion != tt.version {
				t.Errorf("view = %+v", view)
			}
		})
	}
}

func TestVersionPolicy_DevWorkersMatchDevControllers(t *testing.T) {
	e := newEnv(t, "dev")
	dev := e.join("dev-box")
	if got := e.beat(dev, request(newEpoch(), withVersion("dev"))); got != domain.WorkerOnline {
		t.Errorf("a dev Worker on a dev Controller: %s", got)
	}
	released := e.join("released-box")
	if got := e.beat(released, request(newEpoch(), withVersion("v0.3.0"))); got != domain.WorkerDegraded {
		t.Errorf("a released Worker on a dev Controller: %s", got)
	}
}

func TestVersionPolicy_IsReappliedOnEveryHeartbeat(t *testing.T) {
	e := newEnv(t, controllerVersion)
	w := e.join("box")
	epoch := newEpoch()
	e.beat(w, request(epoch, withVersion("v0.1.0")))
	if got := e.state(w); got != domain.WorkerDegraded {
		t.Fatal(got)
	}
	if got := e.beat(w, request(epoch, withVersion("v0.3.1"))); got != domain.WorkerOnline {
		t.Errorf("upgraded: %s", got)
	}
	if got := e.beat(w, request(epoch, withVersion("v0.1.0"))); got != domain.WorkerDegraded {
		t.Errorf("downgraded: %s", got)
	}
}

func TestVersionPolicy_NeitherSideIsSemverMeansTheSameString(t *testing.T) {
	e := newEnv(t, "3f2a9c1")
	same := e.join("same")
	other := e.join("other")
	if got := e.beat(same, request(newEpoch(), withVersion("3f2a9c1"))); got != domain.WorkerOnline {
		t.Errorf("the same commit: %s", got)
	}
	if got := e.beat(other, request(newEpoch(), withVersion("8b7e0d4"))); got != domain.WorkerDegraded {
		t.Errorf("another commit: %s", got)
	}
}

func TestHealth_ADaemonThatReportsItselfDegradedIsDegraded(t *testing.T) {
	e := newEnv(t, controllerVersion)
	w := e.join("box")
	epoch := newEpoch()
	sick := func(r *v1.HeartbeatRequest) { r.Health = v1.Health{Status: v1.HealthDegraded, Message: "disk full"} }

	if got := e.beat(w, request(epoch, sick)); got != domain.WorkerDegraded {
		t.Errorf("registered sick: %s", got)
	}
	if got := e.worker("box").Health; !reflect.DeepEqual(got, v1.Health{Status: v1.HealthDegraded, Message: "disk full"}) {
		t.Errorf("health = %+v", got)
	}
	if got := e.beat(w, request(epoch)); got != domain.WorkerOnline {
		t.Errorf("recovered: %s", got)
	}
}

func TestHeartbeat_EpochEvents(t *testing.T) {
	e := newEnv(t, controllerVersion)
	w := e.join("box")
	first := newEpoch()

	e.beat(w, request(first))
	e.beat(w, request(first))
	if got := e.events(domain.EventWorkerReconnected, domain.EventWorkerRestarted); len(got) != 0 {
		t.Fatalf("registering and beating again emitted %+v", got)
	}

	second := newEpoch()
	e.beat(w, request(second))
	e.beat(w, request(second))
	restarted := e.events(domain.EventWorkerRestarted)
	if len(restarted) != 1 || restarted[0].Data["epoch"] != second || restarted[0].Data["previous_epoch"] != first {
		t.Fatalf("WorkerRestarted = %+v", restarted)
	}
	if got := e.worker("box").Epoch; got != second {
		t.Errorf("epoch = %s, want %s", got, second)
	}
}

func TestHeartbeat_RestartsAreGroupedByTheHeartbeatInterval(t *testing.T) {
	e := newEnv(t, controllerVersion)
	w := e.join("box")
	e.beat(w, request(newEpoch()))

	for range 300 {
		e.beat(w, request(newEpoch()))
	}
	last := newEpoch()
	e.beat(w, request(last))
	if n := len(e.events(domain.EventWorkerRestarted)); n != 1 {
		t.Fatalf("%d WorkerRestarted for 301 restarts inside one interval, want 1", n)
	}
	if got := e.worker("box").Epoch; got != last {
		t.Errorf("epoch = %s, want the latest, %s", got, last)
	}

	e.clock.Advance(domain.Defaults().WorkerHeartbeatInterval)
	e.beat(w, request(newEpoch()))
	if n := len(e.events(domain.EventWorkerRestarted)); n != 2 {
		t.Errorf("%d WorkerRestarted after the interval passed, want 2", n)
	}
}

func TestHeartbeat_AWorkerCanOnlyUpdateItself(t *testing.T) {
	e := newEnv(t, controllerVersion)
	a, b := e.join("a"), e.join("b")

	e.beat(a, request(newEpoch(), func(r *v1.HeartbeatRequest) { r.Hostname = "host-a" }))

	if got := e.worker("a"); got.State != domain.WorkerOnline || got.Hostname != "host-a" || got.LastHeartbeatAt == nil {
		t.Errorf("A = %+v", got)
	}
	if got := e.worker("b"); got.State != domain.WorkerJoining || got.Hostname != "" || got.Epoch != "" || got.LastHeartbeatAt != nil {
		t.Errorf("B was touched by A's heartbeat: %+v", got)
	}
	if e.state(b) != domain.WorkerJoining {
		t.Error("B moved")
	}

	// A client's session reaches no Worker at all.
	_, err := e.svc.Heartbeat(t.Context(), e.owner, request(newEpoch()))
	wantCode(t, err, domain.CodeNotFound)
}

func TestHeartbeat_Validation(t *testing.T) {
	e := newEnv(t, controllerVersion)
	w := e.join("box")
	long := make([]byte, 300)
	for i := range long {
		long[i] = 'x'
	}
	for name, mutate := range map[string]func(*v1.HeartbeatRequest){
		"epoch is not a ULID":     func(r *v1.HeartbeatRequest) { r.Epoch = "restart-1" },
		"no cpu":                  func(r *v1.HeartbeatRequest) { r.Total.CPU = 0 },
		"no memory":               func(r *v1.HeartbeatRequest) { r.Total.MemoryMB = 0 },
		"negative disk":           func(r *v1.HeartbeatRequest) { r.Total.DiskMB = -1 },
		"unknown health":          func(r *v1.HeartbeatRequest) { r.Health.Status = "great" },
		"hidden characters":       func(r *v1.HeartbeatRequest) { r.Hostname = "box\u202e" },
		"overlong version":        func(r *v1.HeartbeatRequest) { r.DaemonVersion = string(long) },
		"empty capability":        func(r *v1.HeartbeatRequest) { r.Capabilities = []string{""} },
		"capability with a space": func(r *v1.HeartbeatRequest) { r.Capabilities = []string{"docker compose"} },
		"too many capabilities":   func(r *v1.HeartbeatRequest) { r.Capabilities = make([]string, 65) },
		"unnamed component":       func(r *v1.HeartbeatRequest) { r.Health.Components = []v1.ComponentHealth{{Status: v1.ComponentOK}} },
		"unknown component state": func(r *v1.HeartbeatRequest) {
			r.Health.Components = []v1.ComponentHealth{{Name: "disk", Status: "bad"}}
		},
		"too many components": func(r *v1.HeartbeatRequest) { r.Health.Components = make([]v1.ComponentHealth, 65) },
		"too many cpus":       func(r *v1.HeartbeatRequest) { r.Total.CPU = 4097 },
		"too much memory":     func(r *v1.HeartbeatRequest) { r.Total.MemoryMB = 1<<40 + 1 },
		"too much disk":       func(r *v1.HeartbeatRequest) { r.Total.DiskMB = 1<<40 + 1 },
	} {
		t.Run(name, func(t *testing.T) {
			_, err := e.svc.Heartbeat(t.Context(), w, request(newEpoch(), mutate))
			wantCode(t, err, domain.CodeInvalidRequest)
		})
	}
	if got := e.state(w); got != domain.WorkerJoining {
		t.Errorf("an invalid heartbeat registered the Worker: %s", got)
	}
}

func TestResources_TheContributionCapsWhatTheMachineHas(t *testing.T) {
	e := newEnv(t, controllerVersion)
	capped := e.join("capped", v1.WorkerSettings{CPUMax: 4, MemoryMaxMB: 8192, DiskMaxMB: 1 << 30, MaxConcurrentRuns: 2, Availability: domain.AvailabilityIdle})
	open := e.join("open")

	e.beat(capped, request(newEpoch()))
	e.beat(open, request(newEpoch()))

	got := e.worker("capped")
	total := v1.Capacity{CPU: 16, MemoryMB: 65536, DiskMB: 500000}
	allocatable := v1.Capacity{CPU: 4, MemoryMB: 8192, DiskMB: 500000} // a cap above what the machine has changes nothing
	if got.Resources != (v1.WorkerResources{Total: total, Allocatable: allocatable, Free: allocatable}) {
		t.Errorf("resources = %+v", got.Resources)
	}
	if got.Contribution.CPUMax != 4 || got.Contribution.MemoryMaxMB != 8192 || got.MaxConcurrentRuns != 2 || got.Availability != domain.AvailabilityIdle {
		t.Errorf("the contribution was not persisted: %+v", got)
	}
	if got := e.worker("open"); got.Resources.Allocatable != total || got.MaxConcurrentRuns != 4 || got.Availability != domain.AvailabilityAlways {
		t.Errorf("a Worker with no limits contributes everything, with the defaults: %+v", got)
	}
}

func TestScenario_WorkerRegistryHeartbeat_Baja(t *testing.T) {
	e := newEnv(t, controllerVersion)
	w := e.join("box")
	id := string(w.Principal.ID)
	epoch := newEpoch()
	e.beat(w, request(epoch))

	// With active Runs it is refused, and nothing changes.
	run := e.seedRun(id, domain.RunRunning)
	e.seedRun(id, domain.RunCompleted) // a finished Run does not count
	_, err := e.svc.Remove(t.Context(), e.owner, "box", v1.ActionRequest{})
	wantCode(t, err, domain.CodeWorkerBusy)
	if got := e.worker("box"); got.State != domain.WorkerOnline || got.ActiveRuns != 1 {
		t.Errorf("after the refusal: %+v", got)
	}
	if n := len(e.events(domain.EventWorkerRemoved)); n != 0 {
		t.Errorf("%d WorkerRemoved events for a refusal", n)
	}
	if got := e.beat(w, request(epoch)); got != domain.WorkerOnline {
		t.Errorf("the identity stopped working after a refused removal: %s", got)
	}

	// Drained and empty, it goes, and its certificate with it.
	_, err = e.svc.Drain(t.Context(), e.owner, "box", v1.ActionRequest{})
	must(t, err)
	e.finishRun(run)
	e.beat(w, request(epoch))
	if got := e.state(w); got != domain.WorkerDrained {
		t.Fatalf("before removing: %s", got)
	}
	removed, err := e.svc.Remove(t.Context(), e.owner, "box", v1.ActionRequest{})
	must(t, err)
	if removed.State != domain.WorkerRemoved || removed.RemovedAt == nil {
		t.Errorf("removed = %+v", removed)
	}
	if n := e.count(`SELECT COUNT(*) FROM identities WHERE id = ? AND revoked_at IS NOT NULL`, id); n != 1 {
		t.Error("the identity of the removed Worker was not revoked")
	}
	if _, err := e.svc.Heartbeat(t.Context(), w, request(epoch)); err == nil {
		t.Error("a removed Worker's certificate can still heartbeat")
	} else {
		wantCode(t, err, domain.CodeUnauthenticated)
	}
	if got := e.events(domain.EventWorkerRemoved); len(got) != 1 || got[0].WorkerID != w.Principal.ID {
		t.Errorf("WorkerRemoved = %+v", got)
	}
	if n := e.count(`SELECT COUNT(*) FROM audit_entries WHERE action = 'worker.removed'`); n != 1 {
		t.Errorf("%d audit entries for the removal", n)
	}

	// Removing it again is not an error, and does not repeat anything.
	if _, err := e.svc.Remove(t.Context(), e.owner, "worker-1", v1.ActionRequest{}); err != nil {
		t.Errorf("second removal: %v", err)
	}
	if n := len(e.events(domain.EventWorkerRemoved)); n != 1 {
		t.Errorf("%d WorkerRemoved events after removing twice", n)
	}
}

func TestRemove_AJoiningWorkerCanBeRemovedAndItsNameIsFreed(t *testing.T) {
	e := newEnv(t, controllerVersion)
	e.join("box")
	_, err := e.svc.Remove(t.Context(), e.owner, "box", v1.ActionRequest{})
	must(t, err)

	e.join("box")
	again, err := e.svc.List(t.Context(), false)
	must(t, err)
	if len(again) != 1 || again[0].ID == "" {
		t.Fatalf("workers = %+v", again)
	}
	all, err := e.svc.List(t.Context(), true)
	must(t, err)
	if len(all) != 2 || all[0].State != domain.WorkerRemoved || all[1].State != domain.WorkerJoining {
		t.Errorf("all = %+v", all)
	}
	if _, err := e.svc.Get(t.Context(), "worker-1"); err != nil {
		t.Errorf("the removed Worker answers to its short ID: %v", err)
	}
	if got := e.worker("box"); got.Short != "worker-2" {
		t.Errorf("the name resolves to %s, want the live Worker", got.Short)
	}
}

func TestDrain_ADrainingWorkerBecomesDrainedWhenItsRunsEnd(t *testing.T) {
	e := newEnv(t, controllerVersion)
	w := e.join("box")
	id := string(w.Principal.ID)
	epoch := newEpoch()
	e.beat(w, request(epoch))
	r1, r2 := e.seedRun(id, domain.RunRunning), e.seedRun(id, domain.RunQueued)

	drained, err := e.svc.Drain(t.Context(), e.owner, "box", v1.ActionRequest{})
	must(t, err)
	if drained.State != domain.WorkerDraining {
		t.Fatalf("after drain: %s", drained.State)
	}
	again, err := e.svc.Drain(t.Context(), e.owner, "box", v1.ActionRequest{})
	if err != nil || again.State != domain.WorkerDraining {
		t.Errorf("draining twice: %+v, %v", again, err)
	}
	if n := len(e.events(domain.EventWorkerDraining)); n != 1 {
		t.Errorf("%d WorkerDraining events after draining twice", n)
	}

	e.beat(w, request(epoch))
	e.finishRun(r1)
	e.beat(w, request(epoch))
	if got := e.state(w); got != domain.WorkerDraining {
		t.Fatalf("with a Run left: %s", got)
	}
	e.finishRun(r2)
	if got := e.beat(w, request(epoch)); got != domain.WorkerDrained {
		t.Fatalf("with none left: %s", got)
	}
	if n := len(e.events(domain.EventWorkerDrained)); n != 1 {
		t.Errorf("%d WorkerDrained events", n)
	}
	e.beat(w, request(epoch))
	if n := len(e.events(domain.EventWorkerDrained)); n != 1 {
		t.Errorf("%d WorkerDrained events after another heartbeat", n)
	}
}

func TestDrain_TheSweeperDrainsAWorkerWhoseRunsEndedBetweenHeartbeats(t *testing.T) {
	e := newEnv(t, controllerVersion)
	w := e.join("box")
	epoch := newEpoch()
	e.beat(w, request(epoch))
	run := e.seedRun(string(w.Principal.ID), domain.RunRunning)
	_, err := e.svc.Drain(t.Context(), e.owner, "box", v1.ActionRequest{})
	must(t, err)

	e.sweep()
	if got := e.state(w); got != domain.WorkerDraining {
		t.Fatalf("with a Run: %s", got)
	}
	e.finishRun(run)
	e.sweep()
	if got := e.state(w); got != domain.WorkerDrained {
		t.Errorf("without Runs: %s", got)
	}
	e.sweep()
	if n := len(e.events(domain.EventWorkerDrained)); n != 1 {
		t.Errorf("%d WorkerDrained events", n)
	}
}

func TestDrain_AnEmptyWorkerIsDrainedByTheNextHeartbeat(t *testing.T) {
	e := newEnv(t, controllerVersion)
	w := e.join("box")
	epoch := newEpoch()
	e.beat(w, request(epoch))
	_, err := e.svc.Drain(t.Context(), e.owner, "box", v1.ActionRequest{})
	must(t, err)

	if got := e.beat(w, request(epoch)); got != domain.WorkerDrained {
		t.Errorf("after the heartbeat: %s", got)
	}
}

func TestUndrain(t *testing.T) {
	e := newEnv(t, controllerVersion)
	w := e.join("box")
	epoch := newEpoch()
	e.beat(w, request(epoch))
	run := e.seedRun(string(w.Principal.ID), domain.RunRunning)
	undrain := func() (v1.Worker, error) {
		return e.svc.Undrain(t.Context(), e.owner, "box", v1.ActionRequest{})
	}

	if got, err := undrain(); err != nil || got.State != domain.WorkerOnline {
		t.Fatalf("undraining an online Worker: %+v, %v", got, err)
	}
	_, err := e.svc.Drain(t.Context(), e.owner, "box", v1.ActionRequest{})
	must(t, err)
	_, err = undrain()
	wantCode(t, err, domain.CodeInvalidTransition) // §9.5: only a drained Worker is undrained

	e.finishRun(run)
	e.beat(w, request(epoch))
	if got, err := undrain(); err != nil || got.State != domain.WorkerOnline {
		t.Fatalf("undraining a drained Worker: %+v, %v", got, err)
	}

	e.clock.Advance(offlineTTL + time.Second)
	e.sweep()
	_, err = undrain()
	wantCode(t, err, domain.CodeWorkerOffline)
}

func TestUndrain_ReappliesTheVerdictOfTheVersionAndTheHealth(t *testing.T) {
	for name, tc := range map[string]struct {
		mutate []func(*v1.HeartbeatRequest)
		want   domain.WorkerState
	}{
		"healthy and compatible": {nil, domain.WorkerOnline},
		"an incompatible version": {
			[]func(*v1.HeartbeatRequest){withVersion("v0.1.0")}, domain.WorkerDegraded,
		},
		"a degraded health": {
			[]func(*v1.HeartbeatRequest){func(r *v1.HeartbeatRequest) { r.Health = v1.Health{Status: v1.HealthDegraded, Message: "disk full"} }},
			domain.WorkerDegraded,
		},
	} {
		t.Run(name, func(t *testing.T) {
			e := newEnv(t, controllerVersion)
			w := e.join("box")
			epoch := newEpoch()
			e.beat(w, request(epoch, tc.mutate...))
			_, err := e.svc.Drain(t.Context(), e.owner, "box", v1.ActionRequest{})
			must(t, err)
			e.beat(w, request(epoch, tc.mutate...)) // no Runs: drained
			if got := e.state(w); got != domain.WorkerDrained {
				t.Fatalf("before undraining: %s", got)
			}

			got, err := e.svc.Undrain(t.Context(), e.owner, "box", v1.ActionRequest{})
			if err != nil || got.State != tc.want {
				t.Errorf("undrained: %+v, %v; want %s", got, err, tc.want)
			}
		})
	}
}

func TestNew_ATTLUnderASecondTakesTheDefault(t *testing.T) {
	e := newEnv(t, controllerVersion)
	w := e.join("box")
	e.beat(w, request(newEpoch()))
	tiny := workers.New(e.db, e.clock, workers.Config{ControllerVersion: controllerVersion, OfflineTTL: time.Millisecond})

	e.clock.Advance(time.Minute)
	must(t, tiny.Sweep(t.Context()))
	if got := e.state(w); got != domain.WorkerOffline {
		t.Fatalf("a minute of silence: %s", got)
	}
	// A TTL of a millisecond would have lost the Worker after its first second.
	fresh := e.join("fresh")
	e.beat(fresh, request(newEpoch()))
	e.clock.Advance(10 * time.Second)
	must(t, tiny.Sweep(t.Context()))
	if got := e.state(fresh); got != domain.WorkerOnline {
		t.Errorf("10 s of silence with a 1 ms TTL: %s, want the default TTL to apply", got)
	}
}

func TestAdministration_AnOfflineWorkerCannotBeDrained(t *testing.T) {
	e := newEnv(t, controllerVersion)
	w := e.join("box")
	e.beat(w, request(newEpoch()))
	e.clock.Advance(offlineTTL + time.Second)
	e.sweep()

	_, err := e.svc.Drain(t.Context(), e.owner, "box", v1.ActionRequest{})
	wantCode(t, err, domain.CodeWorkerOffline)
}

func TestAdministration_IsForClientsAndIsAudited(t *testing.T) {
	e := newEnv(t, controllerVersion)
	w := e.join("box")
	e.beat(w, request(newEpoch()))

	for name, call := range map[string]func() error{
		"drain":   func() error { _, err := e.svc.Drain(t.Context(), w, "box", v1.ActionRequest{}); return err },
		"undrain": func() error { _, err := e.svc.Undrain(t.Context(), w, "box", v1.ActionRequest{}); return err },
		"remove":  func() error { _, err := e.svc.Remove(t.Context(), w, "box", v1.ActionRequest{}); return err },
	} {
		if err := call(); err == nil {
			t.Errorf("a Worker could %s a Worker", name)
		} else {
			wantCode(t, err, domain.CodePolicyDenied)
		}
	}
	_, err := e.svc.Drain(t.Context(), e.owner, "box", v1.ActionRequest{})
	must(t, err)
	if n := e.count(`SELECT COUNT(*) FROM audit_entries WHERE action = 'worker.drained' AND actor_id = ?`, string(e.owner.Principal.ID)); n != 1 {
		t.Errorf("%d audit entries for the drain by the owner", n)
	}
}

func TestReferences(t *testing.T) {
	e := newEnv(t, controllerVersion)
	w := e.join("box")
	id := string(w.Principal.ID)
	for _, ref := range []string{"box", "worker-1", id, id[7:14]} {
		if got := e.worker(ref); got.ID != id {
			t.Errorf("%q resolves to %s", ref, got.ID)
		}
	}
	_, err := e.svc.Get(t.Context(), "nowhere")
	wantCode(t, err, domain.CodeNotFound)
	_, err = e.svc.Get(t.Context(), "")
	wantCode(t, err, domain.CodeNotFound)
}

func TestScenario_WorkerRegistryHeartbeat_Epoch(t *testing.T) {
	e := newEnv(t, controllerVersion)
	w := e.join("box")
	first, second := newEpoch(), newEpoch()

	e.beat(w, request(first))
	if got := e.worker("box").Epoch; got != first {
		t.Fatalf("epoch = %s, want %s", got, first)
	}
	e.beat(w, request(second)) // the daemon restarted
	if got := e.worker("box").Epoch; got != second {
		t.Errorf("epoch = %s after the restart, want %s", got, second)
	}
	if got := e.events(domain.EventWorkerRestarted); len(got) != 1 {
		t.Errorf("WorkerRestarted = %+v", got)
	}
}

func TestEnroll_AFailureAfterInsertingTheWorkerLeavesTheTokenUnspent(t *testing.T) {
	e := newEnv(t, controllerVersion)
	boom := errors.New("boom")
	failing := e.newAuthority(func(ctx context.Context, tx *store.Tx, id domain.ID, name string, s v1.WorkerSettings, now time.Time) error {
		if err := workers.Enroll(ctx, tx, id, name, s, now); err != nil {
			return err
		}
		return boom // after the Worker row and its event are written
	})
	token := e.token()

	_, err := failing.Join(t.Context(), token, newCSR(t), "box", &v1.WorkerSettings{})
	if !errors.Is(err, boom) {
		t.Fatalf("join: %v", err)
	}
	for table, want := range map[string]int{"workers": 0, "identities": 1} { // only the owner
		if n := e.count(`SELECT COUNT(*) FROM ` + table); n != want {
			t.Errorf("%s has %d rows after the failed join, want %d", table, n, want)
		}
	}
	if n := len(e.events(domain.EventWorkerJoined)); n != 0 {
		t.Errorf("%d WorkerJoined events survived the rollback", n)
	}
	if n := e.count(`SELECT COUNT(*) FROM join_tokens WHERE used_at IS NOT NULL`); n != 0 {
		t.Errorf("%d tokens consumed", n)
	}
	if n := e.count(`SELECT COALESCE(MAX(last), 0) FROM sequences WHERE kind = 'worker'`); n != 0 {
		t.Errorf("the short ID sequence moved to %d", n)
	}

	// The same token still works.
	if _, err := e.tryJoin(token, "box"); err != nil {
		t.Errorf("redeeming the token after the failure: %v", err)
	}
	if n := len(e.events(domain.EventWorkerJoined)); n != 1 {
		t.Errorf("%d WorkerJoined events", n)
	}
}

func TestEnroll_Refusals(t *testing.T) {
	e := newEnv(t, controllerVersion)
	e.join("taken")
	for name, tc := range map[string]struct {
		workerName string
		settings   v1.WorkerSettings
	}{
		"a name in use":          {"taken", v1.WorkerSettings{}},
		"a name that is no slug": {"Not A Slug", v1.WorkerSettings{}},
		"a name that is an ID":   {"worker_01JA0000000000000000000000", v1.WorkerSettings{}},
		"negative cpu":           {"a", v1.WorkerSettings{CPUMax: -1}},
		"negative concurrency":   {"b", v1.WorkerSettings{MaxConcurrentRuns: -1}},
		"unknown availability":   {"c", v1.WorkerSettings{Availability: "sometimes"}},
		"too many cpus":          {"d", v1.WorkerSettings{CPUMax: 4097}},
		"too much memory":        {"e", v1.WorkerSettings{MemoryMaxMB: 1<<40 + 1}},
		"too much disk":          {"f", v1.WorkerSettings{DiskMaxMB: 1<<40 + 1}},
		"too much concurrency":   {"g", v1.WorkerSettings{MaxConcurrentRuns: 1025}},
	} {
		t.Run(name, func(t *testing.T) {
			token := e.token()
			_, err := e.tryJoin(token, tc.workerName, tc.settings)
			wantCode(t, err, domain.CodeInvalidRequest)
			// The refusal did not spend the token.
			if _, err := e.tryJoin(token, "fresh-"+strings.ToLower(tc.workerName[:1])); err != nil {
				t.Errorf("redeeming after the refusal: %v", err)
			}
		})
	}
}

func TestJoin_AWorkerInvitationRequiresWorkerSettingsAndAClientOneForbidsThem(t *testing.T) {
	e := newEnv(t, controllerVersion)
	workerToken := e.token()
	_, err := e.authority.Join(t.Context(), workerToken, newCSR(t), "box", nil)
	wantCode(t, err, domain.CodeInvalidRequest)

	clientInvite, _, err := e.authority.CreateToken(t.Context(), e.owner, domain.KindClient, time.Hour)
	must(t, err)
	_, err = e.authority.Join(t.Context(), clientInvite.Token, newCSR(t), "laptop", &v1.WorkerSettings{})
	wantCode(t, err, domain.CodeInvalidRequest)

	// Both tokens are still good for what they are.
	if _, err := e.tryJoin(workerToken, "box"); err != nil {
		t.Errorf("worker token: %v", err)
	}
	if _, err := e.authority.Join(t.Context(), clientInvite.Token, newCSR(t), "laptop", nil); err != nil {
		t.Errorf("client token: %v", err)
	}
}

func TestJoin_AnAuthorityWithoutWorkersRefusesWorkerInvitations(t *testing.T) {
	e := newEnv(t, controllerVersion)
	bare := e.newAuthority(nil)
	token := e.token()
	_, err := bare.Join(t.Context(), token, newCSR(t), "box", &v1.WorkerSettings{})
	wantCode(t, err, domain.CodeNotImplemented)
	if _, err := e.tryJoin(token, "box"); err != nil {
		t.Errorf("the refusal spent the token: %v", err)
	}
}

func TestJoin_RecordsTheJoinedWorker(t *testing.T) {
	e := newEnv(t, controllerVersion)
	w := e.join("box", v1.WorkerSettings{CPUMax: 2})

	got := e.worker("box")
	if got.ID != string(w.Principal.ID) || got.State != domain.WorkerJoining || got.Short != "worker-1" || got.Contribution.CPUMax != 2 {
		t.Errorf("worker = %+v", got)
	}
	if kind, _, err := domain.ParseID(got.ID); err != nil || kind != domain.KindWorker {
		t.Errorf("id %q", got.ID)
	}
	joined := e.events(domain.EventWorkerJoined)
	if len(joined) != 1 || joined[0].WorkerID != w.Principal.ID || joined[0].Actor.ID != got.ID {
		t.Errorf("WorkerJoined = %+v", joined)
	}
	if !slices.Equal(got.Capabilities, []string{}) {
		t.Errorf("capabilities = %#v, want an empty list, not null", got.Capabilities)
	}
}

// startSweeper runs RunSweeper until the test ends and returns what ends it.
func (e *env) startSweeper() (stop func()) {
	e.t.Helper()
	ctx, cancel := context.WithCancel(e.t.Context())
	done := make(chan struct{})
	go func() {
		defer close(done)
		e.svc.RunSweeper(ctx, slog.New(slog.DiscardHandler))
	}()
	waitCtx, cancelWait := context.WithTimeout(e.t.Context(), 5*time.Second)
	defer cancelWait()
	if err := e.clock.BlockUntil(waitCtx, 1); err != nil {
		e.t.Fatal(err)
	}
	return func() { cancel(); <-done }
}

func TestRunSweeper_SweepsEveryThirdOfTheTTL(t *testing.T) {
	e := newEnv(t, controllerVersion)
	w := e.join("box")
	e.beat(w, request(newEpoch()))
	e.clock.Advance(offlineTTL + time.Second) // silent past the TTL, and nobody has swept
	stop := e.startSweeper()
	defer stop()

	e.clock.Advance(offlineTTL/3 - time.Nanosecond)
	if got := e.state(w); got != domain.WorkerOnline {
		t.Fatalf("before the first third of the TTL: %s", got)
	}
	e.clock.Advance(time.Nanosecond)
	deadline := time.Now().Add(5 * time.Second) // waits for the sweep, not for the time to pass
	for e.state(w) != domain.WorkerOffline {
		if time.Now().After(deadline) {
			t.Fatalf("a third of the TTL later the Worker is still %s", e.state(w))
		}
		time.Sleep(time.Millisecond)
	}
}

func TestRunSweeper_EndsWithItsContext(t *testing.T) {
	e := newEnv(t, controllerVersion)
	stop := e.startSweeper()
	stop() // returns only if RunSweeper does
}

func TestHeartbeat_AnIncompatibleDaemonWithComponentsGetsTheVersionComponentFailed(t *testing.T) {
	e := newEnv(t, controllerVersion)
	w := e.join("box")
	components := func(r *v1.HeartbeatRequest) {
		r.Health.Components = []v1.ComponentHealth{{Name: "disk", Status: v1.ComponentOK}, {Name: "version", Status: v1.ComponentOK}}
	}
	if got := e.beat(w, request(newEpoch(), withVersion("v0.1.0"), components)); got != domain.WorkerDegraded {
		t.Fatalf("state = %s", got)
	}
	var version []v1.ComponentHealth
	for _, c := range e.worker("box").Health.Components {
		if c.Name == "version" {
			version = append(version, c)
		}
	}
	if len(version) != 1 || version[0].Status != v1.ComponentFail {
		t.Errorf("version components = %+v", version)
	}

	// A compatible daemon keeps what it said; an old one, with none, stays without.
	e.beat(w, request(newEpoch(), components))
	if got := e.worker("box").Health.Components; len(got) != 2 || got[1].Status != v1.ComponentOK {
		t.Errorf("components = %+v", got)
	}
	e.beat(w, request(newEpoch(), withVersion("v0.1.0")))
	if got := e.worker("box").Health.Components; len(got) != 0 {
		t.Errorf("an old daemon got components: %+v", got)
	}
}
