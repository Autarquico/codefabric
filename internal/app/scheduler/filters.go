package scheduler

import (
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/Autarquico/codefabric/internal/domain"
)

// The filters of §10.1 that apply to a Run today, in the order they are
// evaluated. A Worker's verdict lists every one it fails, so the first is the
// main reason. Environment, Workflow, secrets, isolation and the harness
// configuration lock arrive with the milestones that define them.
const (
	FilterState        = "state"
	FilterAvailability = "availability"
	FilterRequire      = "require"
	FilterAvoid        = "avoid"
	FilterCapabilities = "capabilities"
	FilterHarness      = "harness"
	FilterResources    = "resources"
	FilterConcurrency  = "concurrency"
	FilterCapacity     = "capacity"
	FilterRecentMiss   = "recent_miss"
	FilterReconcile    = "reconcile"
)

// Failure is a filter that a Worker does not pass.
type Failure struct {
	Filter  string
	Code    domain.Code
	Message string
	Details map[string]any
}

type filter struct {
	name  string
	check func(domain.Placement, Worker, time.Time) *Failure
}

var filters = []filter{
	{FilterState, checkState},
	{FilterAvailability, checkAvailability},
	{FilterRequire, checkRequire},
	{FilterAvoid, checkAvoid},
	{FilterCapabilities, checkCapabilities},
	{FilterHarness, checkHarness},
	{FilterResources, checkResources},
	{FilterConcurrency, checkConcurrency},
	{FilterCapacity, checkCapacity},
	{FilterRecentMiss, checkRecentMiss},
	{FilterReconcile, checkReconcile},
}

// checkReconcile keeps a Worker whose live work is not reconciled since the
// Controller started from getting more: it may not know what it holds (§27.1, §27.3).
func checkReconcile(_ domain.Placement, w Worker, _ time.Time) *Failure {
	if !w.Unreconciled {
		return nil
	}
	return &Failure{
		Code:    domain.CodeNoEligibleWorker,
		Message: "the Worker has not reconciled its live Runs since the Controller started",
	}
}

func failures(req domain.Placement, w Worker, now time.Time) []Failure {
	var failed []Failure
	for _, f := range filters {
		if failure := f.check(req, w, now); failure != nil {
			failure.Filter = f.name
			failed = append(failed, *failure)
		}
	}
	return failed
}

func checkState(_ domain.Placement, w Worker, _ time.Time) *Failure {
	if w.State == domain.WorkerOnline {
		return nil
	}
	code := domain.CodeNoEligibleWorker
	if w.State == domain.WorkerOffline {
		code = domain.CodeWorkerOffline
	}
	return &Failure{
		Code: code, Message: fmt.Sprintf("worker is %s: only an online Worker takes Runs", w.State),
		Details: map[string]any{"state": string(w.State)},
	}
}

// checkAvailability: a manual Worker takes only the Runs that name it, and
// idle needs a signal of inactivity that does not exist yet, so no Run can
// know the Worker is idle.
func checkAvailability(req domain.Placement, w Worker, _ time.Time) *Failure {
	switch w.Availability {
	case domain.AvailabilityManual:
		if slices.Contains(req.Require, w.Name) {
			return nil
		}
		return &Failure{
			Code: domain.CodeNoEligibleWorker, Message: "availability is manual: the Worker takes only Runs that require it by name",
			Details: map[string]any{"availability": string(w.Availability)},
		}
	case domain.AvailabilityIdle:
		return &Failure{
			Code: domain.CodeNoEligibleWorker, Message: "availability is idle: Worker idleness is not detected yet",
			Details: map[string]any{"availability": string(w.Availability)},
		}
	}
	return nil
}

func checkRequire(req domain.Placement, w Worker, _ time.Time) *Failure {
	if len(req.Require) == 0 || slices.Contains(req.Require, w.Name) {
		return nil
	}
	return &Failure{
		Code: domain.CodeNoEligibleWorker, Message: "the Run requires one of " + strings.Join(req.Require, ", "),
		Details: map[string]any{"require": req.Require},
	}
}

func checkAvoid(req domain.Placement, w Worker, _ time.Time) *Failure {
	if !slices.Contains(req.Avoid, w.Name) {
		return nil
	}
	return &Failure{Code: domain.CodeNoEligibleWorker, Message: "the Run avoids this Worker"}
}

func checkCapabilities(req domain.Placement, w Worker, _ time.Time) *Failure {
	var missing []string
	for _, c := range req.Capabilities {
		if !slices.Contains(w.Capabilities, c) {
			missing = append(missing, c)
		}
	}
	if len(missing) == 0 {
		return nil
	}
	return &Failure{
		Code: domain.CodeNoEligibleWorker, Message: "missing capabilities: " + strings.Join(missing, ", "),
		Details: map[string]any{"missing": missing},
	}
}

// checkHarness asks the Worker to run the Run's harness in the Run's mode, as
// its capabilities say (§10.1, domain.HarnessModes). A Run that names no
// harness passes. A mode the harness lacks fails here, in eligibility, never
// on the Worker at runtime (§7.4).
func checkHarness(req domain.Placement, w Worker, _ time.Time) *Failure {
	if req.Harness == "" {
		return nil
	}
	modes := domain.HarnessModes(w.Capabilities, req.Harness)
	if slices.Contains(modes, req.Mode) {
		return nil
	}
	message := fmt.Sprintf("harness %q is not available on the Worker", req.Harness)
	if len(modes) > 0 {
		message = fmt.Sprintf("harness %q does not run in %s mode on the Worker", req.Harness, req.Mode)
	}
	return &Failure{
		Code: domain.CodeHarnessUnavailable, Message: message,
		Details: map[string]any{"harness": req.Harness, "mode": req.Mode, "modes": modes},
	}
}

// checkResources fails a Worker that could never take the Run, however empty.
func checkResources(req domain.Placement, w Worker, _ time.Time) *Failure {
	if over := exceeded(req.Resources, w.Allocatable); len(over) > 0 {
		return &Failure{
			Code: domain.CodeNoEligibleWorker, Message: "the Run asks for more than the Worker contributes: " + strings.Join(over, ", "),
			Details: map[string]any{"exceeds": over},
		}
	}
	return nil
}

// checkConcurrency counts open reservations, not active Runs: a reservation
// outlives the Run's process until it is stopped and sealed (§10.5).
func checkConcurrency(_ domain.Placement, w Worker, _ time.Time) *Failure {
	if w.Reservations < w.MaxConcurrentRuns {
		return nil
	}
	return &Failure{
		Code:    domain.CodeWorkerCapacity,
		Message: fmt.Sprintf("worker holds %d of its %d concurrent Runs", w.Reservations, w.MaxConcurrentRuns),
		Details: map[string]any{"reservations": w.Reservations, "max_concurrent_runs": w.MaxConcurrentRuns},
	}
}

func checkCapacity(req domain.Placement, w Worker, _ time.Time) *Failure {
	taken := sum(w.Reserved, req.Resources)
	if over := exceeded(taken, w.Allocatable); len(over) > 0 {
		return &Failure{
			Code: domain.CodeWorkerCapacity, Message: "not enough free resources: " + strings.Join(over, ", "),
			Details: map[string]any{"exceeds": over},
		}
	}
	return nil
}

// checkRecentMiss leaves the Run alone, as far as this Worker goes, for the
// backoff after it rejected a lease or let one expire. The code is that of the
// rejection (capacity, say), or CF_NO_ELIGIBLE_WORKER for an expiry.
func checkRecentMiss(_ domain.Placement, w Worker, now time.Time) *Failure {
	if w.MissedAt == nil || now.Sub(*w.MissedAt) >= domain.LeaseMissBackoff {
		return nil
	}
	code := w.MissCode
	if code == "" {
		code = domain.CodeNoEligibleWorker
	}
	return &Failure{
		Code:    code,
		Message: fmt.Sprintf("worker did not take this Run %s ago (it rejected it or let it expire); it is left alone for %s", now.Sub(*w.MissedAt).Round(time.Second), domain.LeaseMissBackoff),
		Details: map[string]any{"missed_at": w.MissedAt.UTC().Format(time.RFC3339Nano)},
	}
}

// exceeded names the resources where want is above have.
func exceeded(want, have domain.Capacity) []string {
	var over []string
	for _, r := range []struct {
		name       string
		want, have int
	}{
		{"cpu", want.CPU, have.CPU},
		{"memory_mb", want.MemoryMB, have.MemoryMB},
		{"disk_mb", want.DiskMB, have.DiskMB},
	} {
		if r.want > r.have {
			over = append(over, fmt.Sprintf("%s %d > %d", r.name, r.want, r.have))
		}
	}
	return over
}

// sum adds two amounts of compute.
func sum(a, b domain.Capacity) domain.Capacity {
	return domain.Capacity{CPU: a.CPU + b.CPU, MemoryMB: a.MemoryMB + b.MemoryMB, DiskMB: a.DiskMB + b.DiskMB}
}
