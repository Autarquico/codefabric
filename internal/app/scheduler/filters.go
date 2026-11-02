package scheduler

import (
	"fmt"
	"slices"
	"strings"
	"time"

	v1 "github.com/Autarquico/codefabric/internal/api/v1"
	"github.com/Autarquico/codefabric/internal/domain"
)

// The filters of §10.1 that apply to a Run today, in the order they are
// evaluated. A Worker's verdict lists every one it fails, so the first is the
// main reason. Environment and isolation arrive with the milestones that define them.
const (
	FilterState        = "state"
	FilterHealth       = "health"
	FilterAvailability = "availability"
	FilterRequire      = "require"
	FilterAvoid        = "avoid"
	FilterCapabilities = "capabilities"
	FilterHarness      = "harness"
	FilterConfig       = "config"
	FilterSecrets      = "secrets"
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
	check func(Request, Worker, time.Time) *Failure
}

var filters = []filter{
	{FilterState, checkState},
	{FilterHealth, checkHealth},
	{FilterAvailability, checkAvailability},
	{FilterRequire, checkRequire},
	{FilterAvoid, checkAvoid},
	{FilterCapabilities, checkCapabilities},
	{FilterHarness, checkHarness},
	{FilterConfig, checkConfig},
	{FilterSecrets, checkSecrets},
	{FilterResources, checkResources},
	{FilterConcurrency, checkConcurrency},
	{FilterCapacity, checkCapacity},
	{FilterRecentMiss, checkRecentMiss},
	{FilterReconcile, checkReconcile},
}

// checkReconcile keeps a Worker whose live work is not reconciled since the
// Controller started from getting more: it may not know what it holds (§27.1, §27.3).
func checkReconcile(_ Request, w Worker, _ time.Time) *Failure {
	if !w.Unreconciled {
		return nil
	}
	return &Failure{
		Code:    domain.CodeNoEligibleWorker,
		Message: "the Worker has not reconciled its live Runs since the Controller started",
	}
}

func failures(req Request, w Worker, now time.Time) []Failure {
	var failed []Failure
	for _, f := range filters {
		if failure := f.check(req, w, now); failure != nil {
			failure.Filter = f.name
			failed = append(failed, *failure)
		}
	}
	return failed
}

// checkState lets an online Worker through, and a degraded one whose doctor
// reported a failing component: checkHealth then decides whether that failure
// concerns the Run (§9.5). Degraded without a reported failure (an old daemon,
// a version out of policy it cannot name) takes no Runs.
func checkState(_ Request, w Worker, _ time.Time) *Failure {
	if w.State == domain.WorkerOnline || w.State == domain.WorkerDegraded && failingComponent(w.Components, func(string) bool { return true }) != nil {
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

// checkHealth keeps a Run off a Worker with a failing component that concerns
// it: any global one (controller, runtime, git, disk, version, daemon, setup) or one of the
// Run's harness (harness:<id>, auth:<id>, workflow:<id>). Those of other
// harnesses are no reason.
func checkHealth(req Request, w Worker, _ time.Time) *Failure {
	c := failingComponent(w.Components, func(name string) bool { return concerns(name, req.Harness) })
	if c == nil {
		return nil
	}
	return &Failure{
		Code: domain.CodeNoEligibleWorker, Message: c.Name + ": " + c.Message,
		Details: map[string]any{"component": c.Name, "action": c.Action},
	}
}

func failingComponent(components []v1.ComponentHealth, concerning func(string) bool) *v1.ComponentHealth {
	for i, c := range components {
		if c.Status == v1.ComponentFail && concerning(c.Name) {
			return &components[i]
		}
	}
	return nil
}

// concerns says whether the component named affects the Runs of harness: the
// per-harness ones only those of their harness, every other one all Runs.
func concerns(name, harness string) bool {
	for _, prefix := range []string{"harness:", "auth:", "workflow:"} {
		if id, ok := strings.CutPrefix(name, prefix); ok {
			return id == harness
		}
	}
	return true
}

// checkAvailability: a manual Worker takes only the Runs that name it, and
// idle needs a signal of inactivity that does not exist yet, so no Run can
// know the Worker is idle.
func checkAvailability(req Request, w Worker, _ time.Time) *Failure {
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

func checkRequire(req Request, w Worker, _ time.Time) *Failure {
	if len(req.Require) == 0 || slices.Contains(req.Require, w.Name) {
		return nil
	}
	return &Failure{
		Code: domain.CodeNoEligibleWorker, Message: "the Run requires one of " + strings.Join(req.Require, ", "),
		Details: map[string]any{"require": req.Require},
	}
}

func checkAvoid(req Request, w Worker, _ time.Time) *Failure {
	if !slices.Contains(req.Avoid, w.Name) {
		return nil
	}
	return &Failure{Code: domain.CodeNoEligibleWorker, Message: "the Run avoids this Worker"}
}

func checkCapabilities(req Request, w Worker, _ time.Time) *Failure {
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
func checkHarness(req Request, w Worker, _ time.Time) *Failure {
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

// checkConfig applies the configuration lock of §8.12 to a harness the Worker
// does not isolate, where every Run reads the same global configuration: no
// apply may wait for it, the Runs that hold it must need the same
// configuration, and a Run with a Workflow needs the Worker to have applied
// exactly that digest, whole and unedited. The reason says which one fails.
func checkConfig(req Request, w Worker, _ time.Time) *Failure {
	if req.Harness == "" || slices.Contains(w.Capabilities, domain.HarnessConfigIsolationCapability(req.Harness)) {
		return nil
	}
	c := w.Configs[req.Harness]
	switch {
	case c.ApplyPending:
		return &Failure{
			Code: domain.CodeWorkerBusy, Message: fmt.Sprintf("an apply of the %s configuration waits for the Worker's Runs to end (apply_pending)", req.Harness),
			Details: map[string]any{"reason": "apply_pending", "harness": req.Harness},
		}
	case c.Locked && c.Key != req.WorkflowDigest:
		return &Failure{
			Code: domain.CodeWorkerBusy, Message: fmt.Sprintf("the Worker's %s Runs hold another configuration (config_key mismatch)", req.Harness),
			Details: map[string]any{"reason": "config_key", "harness": req.Harness, "held": c.Key, "wanted": req.WorkflowDigest},
		}
	case req.WorkflowDigest != "" && (c.Applied != req.WorkflowDigest || c.Partial || c.ManagedDrift):
		return &Failure{
			Code: domain.CodeNoEligibleWorker, Message: fmt.Sprintf("the Workflow of the Run is not applied globally for %s on the Worker", req.Harness),
			Details: map[string]any{"reason": "workflow_not_applied", "harness": req.Harness, "applied": c.Applied, "wanted": req.WorkflowDigest, "partial": c.Partial, "drift": c.ManagedDrift},
		}
	}
	return nil
}

// checkSecrets asks that every secret of the Run be allowed by its Project
// and announced by the Worker for that Project (§16.7). A Run without secrets
// passes. A ref the allowlist lacks fails every Worker alike with
// CF_POLICY_DENIED, which outranks what this Worker holds; otherwise a ref
// the Worker lacks, or holds only for other Projects, fails it with
// CF_SECRET_UNAVAILABLE. Details name refs, never values.
func checkSecrets(req Request, w Worker, _ time.Time) *Failure {
	if len(req.Secrets) == 0 {
		return nil
	}
	if len(req.NotAllowed) > 0 {
		return &Failure{
			Code: domain.CodePolicyDenied, Message: "the Project does not allow the secrets " + strings.Join(req.NotAllowed, ", "),
			Details: map[string]any{"not_allowed": req.NotAllowed},
		}
	}
	var missing, outOfScope []string
	for _, ref := range req.Secrets {
		projects, held := w.Secrets[ref]
		switch {
		case !held:
			missing = append(missing, ref)
		case len(projects) > 0 && !slices.Contains(projects, string(req.Project)):
			outOfScope = append(outOfScope, ref)
		}
	}
	if len(missing) == 0 && len(outOfScope) == 0 {
		return nil
	}
	var reasons []string
	details := map[string]any{}
	if len(missing) > 0 {
		reasons = append(reasons, "the Worker does not hold the secrets "+strings.Join(missing, ", "))
		details["missing"] = missing
	}
	if len(outOfScope) > 0 {
		reasons = append(reasons, "the Worker holds the secrets "+strings.Join(outOfScope, ", ")+" only for other Projects")
		details["out_of_scope"] = outOfScope
	}
	return &Failure{Code: domain.CodeSecretUnavailable, Message: strings.Join(reasons, "; "), Details: details}
}

// checkResources fails a Worker that could never take the Run, however empty.
func checkResources(req Request, w Worker, _ time.Time) *Failure {
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
func checkConcurrency(_ Request, w Worker, _ time.Time) *Failure {
	if w.Reservations < w.MaxConcurrentRuns {
		return nil
	}
	return &Failure{
		Code:    domain.CodeWorkerCapacity,
		Message: fmt.Sprintf("worker holds %d of its %d concurrent Runs", w.Reservations, w.MaxConcurrentRuns),
		Details: map[string]any{"reservations": w.Reservations, "max_concurrent_runs": w.MaxConcurrentRuns},
	}
}

func checkCapacity(req Request, w Worker, _ time.Time) *Failure {
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
func checkRecentMiss(_ Request, w Worker, now time.Time) *Failure {
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
