// Package scheduler decides which Worker takes each queued Run (§10.1) and
// explains why (§10.6). The decision is a pure function of the Run's request
// and a snapshot of the Workers (filters.go, score.go, decide.go); the Service
// reads the snapshot, writes the lease and drives the loop.
package scheduler

import (
	"cmp"
	"slices"
	"time"

	v1 "github.com/Autarquico/codefabric/internal/api/v1"
	"github.com/Autarquico/codefabric/internal/app/runs"
	"github.com/Autarquico/codefabric/internal/domain"
)

// Worker is a candidate as the scheduler sees it. Reserved and Reservations
// are the open reservations (§10.5), never the real usage; MissedAt is when
// the Worker last rejected a lease of this Run or let one expire; KnowsProject says that it
// already accepted a Run of the Run's Project (the affinity of §10.1).
type Worker struct {
	ID                domain.ID
	Name              string
	State             domain.WorkerState
	Availability      domain.AvailabilityPolicy
	Capabilities      []string
	Allocatable       v1.Capacity
	Reserved          v1.Capacity
	Reservations      int
	MaxConcurrentRuns int
	MissedAt          *time.Time
	KnowsProject      bool
}

// Verdict is what the scheduler found of one Worker: the filters it fails, in
// the fixed order of filters, or the score that ranks it.
type Verdict struct {
	ID     domain.ID
	Name   string
	State  domain.WorkerState
	Failed []Failure
	Score  *Score
}

// Eligible says that the Worker passes every filter.
func (v Verdict) Eligible() bool { return len(v.Failed) == 0 }

// Decision is the verdict on every Worker, sorted by name, and the Worker
// chosen among the eligible ones, nil if there is none.
type Decision struct {
	Verdicts []Verdict
	Chosen   *Verdict
}

// Decide ranks the Workers for a Run. It is deterministic: the same inputs
// give the same Decision whatever the order of workers, because the highest
// score wins and a tie goes to the lowest name (§10.6). Names are unique among
// live Workers.
func Decide(req runs.Placement, workers []Worker, now time.Time) Decision {
	sorted := slices.SortedFunc(slices.Values(workers), func(a, b Worker) int { return cmp.Compare(a.Name, b.Name) })
	d := Decision{Verdicts: make([]Verdict, len(sorted))}
	for i, w := range sorted {
		v := Verdict{ID: w.ID, Name: w.Name, State: w.State, Failed: failures(req, w, now)}
		if v.Eligible() {
			s := score(req, w)
			v.Score = &s
		}
		d.Verdicts[i] = v
	}
	for i := range d.Verdicts {
		v := &d.Verdicts[i]
		// Verdicts are in name order, so a strictly greater score is the only way to displace the first.
		if v.Eligible() && (d.Chosen == nil || v.Score.Total > d.Chosen.Score.Total) {
			d.Chosen = v
		}
	}
	return d
}
