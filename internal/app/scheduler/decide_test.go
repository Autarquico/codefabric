package scheduler_test

import (
	"math/rand/v2"
	"slices"
	"testing"
	"time"

	v1 "github.com/Autarquico/codefabric/internal/api/v1"
	"github.com/Autarquico/codefabric/internal/app/scheduler"
	"github.com/Autarquico/codefabric/internal/domain"
)

var now = time.Date(2026, 10, 10, 12, 0, 0, 0, time.UTC)

// worker is an online Worker that contributes 8 CPUs and 16 GB and has
// nothing reserved, with the changes in mutate.
func worker(name string, mutate ...func(*scheduler.Worker)) scheduler.Worker {
	w := scheduler.Worker{
		ID: domain.ID("worker_" + name), Name: name, State: domain.WorkerOnline, Availability: domain.AvailabilityAlways,
		Allocatable: v1.Capacity{CPU: 8, MemoryMB: 16384, DiskMB: 100000}, MaxConcurrentRuns: 4,
	}
	for _, m := range mutate {
		m(&w)
	}
	return w
}

func request(mutate ...func(*scheduler.Request)) scheduler.Request {
	r := scheduler.Request{Resources: v1.Capacity{CPU: 2, MemoryMB: 4096}}
	for _, m := range mutate {
		m(&r)
	}
	return r
}

func chosen(t *testing.T, d scheduler.Decision) string {
	t.Helper()
	if d.Chosen == nil {
		return ""
	}
	return d.Chosen.Name
}

func verdict(t *testing.T, d scheduler.Decision, name string) scheduler.Verdict {
	t.Helper()
	for _, v := range d.Verdicts {
		if v.Name == name {
			return v
		}
	}
	t.Fatalf("no verdict for %s", name)
	return scheduler.Verdict{}
}

func TestScenario_SchedulerLeasesReservations_Determinismo(t *testing.T) {
	// Two Workers tie on every part of the score.
	workers := []scheduler.Worker{worker("alpha"), worker("beta")}
	rng := rand.New(rand.NewPCG(1, 2))

	for i := range 1000 {
		rng.Shuffle(len(workers), func(a, b int) { workers[a], workers[b] = workers[b], workers[a] })
		d := scheduler.Decide(request(), workers, now)
		if got := chosen(t, d); got != "alpha" {
			t.Fatalf("decision %d chose %q: a tie goes to the lowest name", i, got)
		}
	}
}

func TestDecide_DoesNotDependOnTheOrderOfTheWorkersAndDoesNotChangeThem(t *testing.T) {
	workers := []scheduler.Worker{
		worker("gamma", func(w *scheduler.Worker) { w.Reserved = v1.Capacity{CPU: 4}; w.Reservations = 1 }),
		worker("alpha", func(w *scheduler.Worker) { w.State = domain.WorkerDraining }),
		worker("beta"),
	}
	before := slices.Clone(workers)
	want := scheduler.Decide(request(), workers, now)
	for _, perm := range [][]int{{2, 1, 0}, {1, 2, 0}, {0, 2, 1}} {
		shuffled := []scheduler.Worker{workers[perm[0]], workers[perm[1]], workers[perm[2]]}
		got := scheduler.Decide(request(), shuffled, now)
		if chosen(t, got) != "beta" || len(got.Verdicts) != 3 {
			t.Fatalf("permutation %v chose %q", perm, chosen(t, got))
		}
		for i := range want.Verdicts {
			if got.Verdicts[i].Name != want.Verdicts[i].Name || len(got.Verdicts[i].Failed) != len(want.Verdicts[i].Failed) {
				t.Fatalf("verdict %d differs: %+v vs %+v", i, got.Verdicts[i], want.Verdicts[i])
			}
		}
	}
	if !slices.EqualFunc(workers, before, func(a, b scheduler.Worker) bool { return a.Name == b.Name }) {
		t.Error("Decide reordered the slice it was given")
	}
	names := []string{want.Verdicts[0].Name, want.Verdicts[1].Name, want.Verdicts[2].Name}
	if !slices.IsSorted(names) {
		t.Errorf("verdicts are not by name: %v", names)
	}
}

// Each part of the score changes the winner when the others are even.
func TestScore_EachComponentDecides(t *testing.T) {
	tests := []struct {
		name string
		req  scheduler.Request
		a, b scheduler.Worker // a sorts before b: it would win a tie
		want string
	}{
		{
			name: "preference outranks everything",
			req:  request(func(r *scheduler.Request) { r.Prefer = []string{"zulu"} }),
			a:    worker("alpha", func(w *scheduler.Worker) { w.KnowsProject = true }),
			b:    worker("zulu", func(w *scheduler.Worker) { w.Reserved = v1.Capacity{CPU: 4}; w.Reservations = 3 }),
			want: "zulu",
		},
		{
			name: "earlier in the prefer list wins",
			req:  request(func(r *scheduler.Request) { r.Prefer = []string{"zulu", "alpha"} }),
			a:    worker("alpha"),
			b:    worker("zulu"),
			want: "zulu",
		},
		{
			name: "more free resources",
			req:  request(),
			a:    worker("alpha", func(w *scheduler.Worker) { w.Reserved = v1.Capacity{CPU: 4, MemoryMB: 8192}; w.Reservations = 0 }),
			b:    worker("zulu"),
			want: "zulu",
		},
		{
			name: "less load",
			req:  request(),
			a:    worker("alpha", func(w *scheduler.Worker) { w.Reservations = 2 }),
			b:    worker("zulu", func(w *scheduler.Worker) { w.Reservations = 1 }),
			want: "zulu",
		},
		{
			name: "affinity with the Project",
			req:  request(),
			a:    worker("alpha"),
			b:    worker("zulu", func(w *scheduler.Worker) { w.KnowsProject = true }),
			want: "zulu",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			d := scheduler.Decide(tc.req, []scheduler.Worker{tc.b, tc.a}, now)
			if got := chosen(t, d); got != tc.want {
				a, b := verdict(t, d, "alpha").Score, verdict(t, d, "zulu").Score
				t.Errorf("chose %q, want %q (alpha %+v, zulu %+v)", got, tc.want, a, b)
			}
		})
	}
}

func TestScore_IsTheSumOfItsParts(t *testing.T) {
	w := worker("alpha", func(w *scheduler.Worker) {
		w.Reserved = v1.Capacity{CPU: 2, MemoryMB: 4096}
		w.Reservations = 1
		w.KnowsProject = true
	})
	d := scheduler.Decide(request(func(r *scheduler.Request) { r.Prefer = []string{"beta", "alpha"} }), []scheduler.Worker{w}, now)
	s := *d.Chosen.Score
	// 8 CPUs with 2+2 taken leave 4/8; 16 GB with 4+4 taken leave 8/16.
	want := scheduler.Score{Prefer: 1_000_000, Free: 500 + 500, Load: -250, Affinity: 100}
	want.Total = want.Prefer + want.Free + want.Load + want.Affinity
	if s != want {
		t.Errorf("score = %+v, want %+v", s, want)
	}
}

func TestDecide_NoEligibleWorkerChoosesNone(t *testing.T) {
	d := scheduler.Decide(request(), nil, now)
	if d.Chosen != nil || len(d.Verdicts) != 0 {
		t.Errorf("decision for no Workers = %+v", d)
	}
	d = scheduler.Decide(request(), []scheduler.Worker{worker("alpha", func(w *scheduler.Worker) { w.State = domain.WorkerOffline })}, now)
	if d.Chosen != nil || d.Verdicts[0].Score != nil {
		t.Errorf("an ineligible Worker was ranked: %+v", d)
	}
}

// Every filter, alone: it names itself, carries its code and keeps the other
// Workers out of the way.
func TestFilters_EachOneRejectsWithItsReason(t *testing.T) {
	recent := now.Add(-3 * time.Second)
	tests := []struct {
		filter string
		code   domain.Code
		req    scheduler.Request
		w      scheduler.Worker
	}{
		{scheduler.FilterState, domain.CodeNoEligibleWorker, request(), worker("w", func(w *scheduler.Worker) { w.State = domain.WorkerDraining })},
		{scheduler.FilterState, domain.CodeWorkerOffline, request(), worker("w", func(w *scheduler.Worker) { w.State = domain.WorkerOffline })},
		{scheduler.FilterState, domain.CodeNoEligibleWorker, request(), worker("w", func(w *scheduler.Worker) { w.State = domain.WorkerDegraded })},
		{scheduler.FilterState, domain.CodeNoEligibleWorker, request(), worker("w", func(w *scheduler.Worker) { w.State = domain.WorkerDrained })},
		{scheduler.FilterState, domain.CodeNoEligibleWorker, request(), worker("w", func(w *scheduler.Worker) { w.State = domain.WorkerJoining })},
		{scheduler.FilterAvailability, domain.CodeNoEligibleWorker, request(), worker("w", func(w *scheduler.Worker) { w.Availability = domain.AvailabilityManual })},
		{scheduler.FilterAvailability, domain.CodeNoEligibleWorker, request(func(r *scheduler.Request) { r.Require = []string{"w"} }),
			worker("w", func(w *scheduler.Worker) { w.Availability = domain.AvailabilityIdle })},
		{scheduler.FilterRequire, domain.CodeNoEligibleWorker, request(func(r *scheduler.Request) { r.Require = []string{"other"} }), worker("w")},
		{scheduler.FilterAvoid, domain.CodeNoEligibleWorker, request(func(r *scheduler.Request) { r.Avoid = []string{"w"} }), worker("w")},
		{scheduler.FilterCapabilities, domain.CodeNoEligibleWorker, request(func(r *scheduler.Request) { r.Capabilities = []string{"docker", "gpu"} }),
			worker("w", func(w *scheduler.Worker) { w.Capabilities = []string{"docker"} })},
		{scheduler.FilterHarness, domain.CodeHarnessUnavailable, request(func(r *scheduler.Request) { r.Harness = "claude" }), worker("w")},
		{scheduler.FilterResources, domain.CodeNoEligibleWorker, request(func(r *scheduler.Request) { r.Resources.CPU = 9 }), worker("w")},
		{scheduler.FilterConcurrency, domain.CodeWorkerCapacity, request(), worker("w", func(w *scheduler.Worker) { w.Reservations = 4 })},
		{scheduler.FilterCapacity, domain.CodeWorkerCapacity, request(), worker("w", func(w *scheduler.Worker) { w.Reserved = v1.Capacity{CPU: 7}; w.Reservations = 1 })},
		{scheduler.FilterCapacity, domain.CodeWorkerCapacity, request(), worker("w", func(w *scheduler.Worker) { w.Reserved = v1.Capacity{MemoryMB: 14000}; w.Reservations = 1 })},
		{scheduler.FilterCapacity, domain.CodeWorkerCapacity, request(func(r *scheduler.Request) { r.Resources.DiskMB = 50 }),
			worker("w", func(w *scheduler.Worker) { w.Reserved = v1.Capacity{DiskMB: 99980}; w.Reservations = 1 })},
		{scheduler.FilterRecentReject, domain.CodeWorkerCapacity, request(), worker("w", func(w *scheduler.Worker) { w.RejectedAt = &recent })},
	}
	for _, tc := range tests {
		t.Run(tc.filter+"/"+string(tc.code)+"/"+string(tc.w.State), func(t *testing.T) {
			d := scheduler.Decide(tc.req, []scheduler.Worker{tc.w}, now)
			v := d.Verdicts[0]
			if d.Chosen != nil || v.Eligible() || len(v.Failed) == 0 {
				t.Fatalf("the Worker passed: %+v", v)
			}
			if f := v.Failed[0]; f.Filter != tc.filter || f.Code != tc.code || f.Message == "" {
				t.Errorf("first failure = %+v, want filter %s with %s", f, tc.filter, tc.code)
			}
		})
	}
}

func TestFilters_EveryFailureIsListedInTheFixedOrder(t *testing.T) {
	recent := now.Add(-time.Second)
	w := worker("w", func(w *scheduler.Worker) {
		w.State = domain.WorkerDraining
		w.Availability = domain.AvailabilityManual
		w.Reservations = 4
		w.Reserved = v1.Capacity{CPU: 8}
		w.RejectedAt = &recent
	})
	req := request(func(r *scheduler.Request) {
		r.Require = []string{"other"}
		r.Avoid = []string{"w"}
		r.Capabilities = []string{"gpu"}
		r.Harness = "claude"
		r.Resources.CPU = 9
	})

	var got []string
	for _, f := range scheduler.Decide(req, []scheduler.Worker{w}, now).Verdicts[0].Failed {
		got = append(got, f.Filter)
	}
	want := []string{"state", "availability", "require", "avoid", "capabilities", "harness", "resources", "concurrency", "capacity", "recent_reject"}
	if !slices.Equal(got, want) {
		t.Errorf("failures = %v, want %v", got, want)
	}
}

func TestFilters_Boundaries(t *testing.T) {
	t.Run("a Run that exactly fits the free resources is eligible", func(t *testing.T) {
		w := worker("w", func(w *scheduler.Worker) { w.Reserved = v1.Capacity{CPU: 6, MemoryMB: 12288}; w.Reservations = 1 })
		if d := scheduler.Decide(request(), []scheduler.Worker{w}, now); d.Chosen == nil {
			t.Errorf("rejected: %+v", d.Verdicts[0].Failed)
		}
	})
	t.Run("one slot below max_concurrent_runs is eligible", func(t *testing.T) {
		w := worker("w", func(w *scheduler.Worker) { w.Reservations = 3 })
		if d := scheduler.Decide(request(), []scheduler.Worker{w}, now); d.Chosen == nil {
			t.Errorf("rejected: %+v", d.Verdicts[0].Failed)
		}
	})
	t.Run("a rejection is forgotten once the backoff has passed", func(t *testing.T) {
		at := now.Add(-domain.LeaseRejectBackoff)
		w := worker("w", func(w *scheduler.Worker) { w.RejectedAt = &at })
		if d := scheduler.Decide(request(), []scheduler.Worker{w}, now); d.Chosen == nil {
			t.Errorf("rejected: %+v", d.Verdicts[0].Failed)
		}
		inside := now.Add(-domain.LeaseRejectBackoff + time.Nanosecond)
		w.RejectedAt = &inside
		if d := scheduler.Decide(request(), []scheduler.Worker{w}, now); d.Chosen != nil {
			t.Error("a rejection a nanosecond inside the backoff did not count")
		}
	})
	t.Run("a manual Worker takes the Runs that require it", func(t *testing.T) {
		w := worker("w", func(w *scheduler.Worker) { w.Availability = domain.AvailabilityManual })
		d := scheduler.Decide(request(func(r *scheduler.Request) { r.Require = []string{"w", "x"} }), []scheduler.Worker{w}, now)
		if d.Chosen == nil {
			t.Errorf("rejected: %+v", d.Verdicts[0].Failed)
		}
	})
	t.Run("a harness is the capability harness:<id>", func(t *testing.T) {
		w := worker("w", func(w *scheduler.Worker) { w.Capabilities = []string{scheduler.HarnessCapability("claude")} })
		d := scheduler.Decide(request(func(r *scheduler.Request) { r.Harness = "claude" }), []scheduler.Worker{w}, now)
		if d.Chosen == nil {
			t.Errorf("rejected: %+v", d.Verdicts[0].Failed)
		}
	})
}
