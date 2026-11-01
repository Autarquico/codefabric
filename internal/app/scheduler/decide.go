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
	Allocatable       domain.Capacity
	Reserved          domain.Capacity
	Reservations      int
	MaxConcurrentRuns int
	MissedAt          *time.Time  // when it last rejected a lease of the Run or let one expire
	MissCode          domain.Code // why: the code it rejected with, or CF_NO_ELIGIBLE_WORKER for an expiry
	KnowsProject      bool
	// Unreconciled: the Worker has live work it has not reconciled since the
	// Controller started (§27.1); it gets nothing more until it has.
	Unreconciled bool
	// Secrets are the refs the Worker last announced, each with the Projects
	// it holds the ref for (none: any Project). Only refs, never values.
	Secrets map[string][]string
	// Configs is the global configuration of each harness the Worker does not
	// isolate (§8.12), by harness: its lock and the Workflow it has applied.
	Configs map[string]HarnessConfig
	// Components are the doctor's verdicts of the last heartbeat; none for a
	// daemon that predates it (§9.5).
	Components []v1.ComponentHealth
}

// HarnessConfig is what the Worker holds of the global configuration of one
// harness. Locked says that Runs hold the lock, for the configuration Key (the
// digest of their Workflow, "" for none); ApplyPending that an apply waits for
// it. Applied is the digest of the Workflow applied, Partial that components
// were skipped for drift and ManagedDrift that a managed component was edited.
type HarnessConfig struct {
	Locked       bool
	Key          string
	ApplyPending bool
	Applied      string
	Partial      bool
	ManagedDrift bool
}

// Request is what a Run asks of the Worker that takes it: its placement and,
// when it uses secrets, its Project and the refs of the Run that the
// Project's allowlist lacks.
type Request struct {
	domain.Placement
	Project    domain.ID
	NotAllowed []string
	// WorkflowDigest is the digest of the Workflow the Run froze, "" for none:
	// it is the Run's configuration key (§8.12).
	WorkflowDigest string
}

// NewRequest judges the Run's secrets against the allowlist of its Project,
// which is the same for every Worker.
func NewRequest(placement domain.Placement, project domain.ID, allowlist []string) Request {
	req := Request{Placement: placement, Project: project}
	for _, ref := range placement.Secrets {
		if !slices.Contains(allowlist, ref) {
			req.NotAllowed = append(req.NotAllowed, ref)
		}
	}
	return req
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
func Decide(req Request, workers []Worker, now time.Time) Decision {
	sorted := slices.SortedFunc(slices.Values(workers), func(a, b Worker) int { return cmp.Compare(a.Name, b.Name) })
	d := Decision{Verdicts: make([]Verdict, len(sorted))}
	for i, w := range sorted {
		v := Verdict{ID: w.ID, Name: w.Name, State: w.State, Failed: failures(req, w, now)}
		if v.Eligible() {
			s := score(req.Placement, w)
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
