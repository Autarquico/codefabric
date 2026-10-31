// Package applier is the Worker side of applying a Workflow to the global
// configuration of a harness (§8.6): it takes the orders the heartbeat
// delivers, downloads and verifies what they name, applies them with
// [globalapply] and reports the outcome.
//
// One order runs at a time per harness. An order is applied once per
// (id, generation) and never after a newer one arrived for its harness.
package applier

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/Autarquico/codefabric/internal/adapters/harness"
	"github.com/Autarquico/codefabric/internal/adapters/secrets"
	v1 "github.com/Autarquico/codefabric/internal/api/v1"
	"github.com/Autarquico/codefabric/internal/domain"
	"github.com/Autarquico/codefabric/internal/support/clock"
	"github.com/Autarquico/codefabric/internal/worker/globalapply"
	"github.com/Autarquico/codefabric/internal/worker/workflowfetch"
)

// secretTimeout bounds one read of the secret store.
const secretTimeout = 10 * time.Second

// What the Controller accepts of a report (maxReport* in
// internal/app/workflows/applyaccess.go); a bigger one is refused for good.
const (
	maxResultBytes = 256 << 10
	maxErrorBytes  = 4 << 10
	maxDriftItems  = 256
)

// maxInternalFaults is how many deliveries of an order are retried when the
// Controller answers CodeInternal before the order is reported as failed.
const maxInternalFaults = 5

// Client is what the Worker asks of the Controller for an order.
// *client.Client opened with the credentials of the Worker implements it.
type Client interface {
	ApplyManifest(ctx context.Context, id string, generation int) (v1.WorkflowVersion, error)
	OpenApplyBlob(ctx context.Context, id string, generation int, digest string) (io.ReadCloser, int64, error)
	ReportWorkflowApply(ctx context.Context, id string, req v1.ReportWorkflowApplyRequest) (v1.ReportWorkflowApplyResponse, error)
}

// Options parameterise New.
type Options struct {
	Client Client
	// Adapters are the harnesses of this Worker; those that cannot apply
	// globally (harness.GlobalInspector) are ignored.
	Adapters []harness.Adapter
	Host     harness.Host
	// DataDir is the data directory of the Worker.
	DataDir string
	Secrets secrets.Store
	Log     *slog.Logger
	// Clock times the reads of the secret store; defaults to clock.Real.
	Clock clock.Clock
	// Recover replaces globalapply.Recover, for tests.
	Recover func(dataDir string) error
}

// Applier executes the orders of one Worker.
type Applier struct {
	o       Options
	engines map[string]*globalapply.Engine
	log     *slog.Logger
	wg      sync.WaitGroup

	mu     sync.Mutex
	locks  map[string]*sync.Mutex
	latest map[string]int       // newest generation seen, by lane
	known  map[orderKey]*record // orders taken, to apply each once
	faults map[orderKey]int     // Controller internal faults met by an order
}

type orderKey struct {
	id         string
	generation int
}

// record is what the Worker remembers of an order it took: the report that the
// Controller has not acknowledged yet, sent again when the order is delivered
// again.
type record struct {
	lane    string
	pending *v1.ReportWorkflowApplyRequest
	busy    bool // working on it or reporting it
}

// New recovers every harness whose apply a crash cut off, and returns the
// Applier. Recovery runs before any order is taken. A recovery that fails is
// logged, not fatal: the engine heals the journal again before its next apply.
func New(o Options) *Applier {
	a := &Applier{
		o: o, log: o.Log, engines: map[string]*globalapply.Engine{},
		locks: map[string]*sync.Mutex{}, latest: map[string]int{}, known: map[orderKey]*record{}, faults: map[orderKey]int{},
	}
	if a.log == nil {
		a.log = slog.Default()
	}
	if a.o.Clock == nil {
		a.o.Clock = clock.Real{}
	}
	recoverFn := o.Recover
	if recoverFn == nil {
		recoverFn = globalapply.Recover
	}
	if err := recoverFn(o.DataDir); err != nil {
		a.log.Error("recovering an interrupted global apply failed; it is retried before the next one", "err", err)
	}
	for _, ad := range o.Adapters {
		if _, ok := ad.(harness.GlobalInspector); !ok {
			continue
		}
		a.engines[ad.ID()] = &globalapply.Engine{Adapter: ad, Host: o.Host, DataDir: o.DataDir, Secrets: a.resolve}
	}
	return a
}

// resolve is the secret store of the Worker as the engine reads it. It never
// returns a value in an error.
func (a *Applier) resolve(ctx context.Context, ref string) ([]byte, error) {
	if a.o.Secrets == nil {
		return nil, errors.New("this Worker keeps no secrets")
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	timeout := a.o.Clock.NewTimer(secretTimeout)
	defer timeout.Stop()
	go func() {
		select {
		case <-timeout.C():
			cancel()
		case <-ctx.Done():
		}
	}()
	s, err := a.o.Secrets.Get(ctx, ref)
	if err != nil {
		if errors.Is(err, secrets.ErrNotFound) {
			return nil, errors.New("this Worker does not hold it")
		}
		return nil, errors.New("the secret store of this Worker cannot be read")
	}
	return s.Value, nil
}

// lock takes the lock of harnessID and returns the function that releases it.
func (a *Applier) lock(harnessID string) (unlock func()) {
	a.mu.Lock()
	m, ok := a.locks[harnessID]
	if !ok {
		m = &sync.Mutex{}
		a.locks[harnessID] = m
	}
	a.mu.Unlock()
	m.Lock()
	return m.Unlock
}

// Wait returns when the orders in flight are done.
func (a *Applier) Wait() { a.wg.Wait() }

// Handle takes the orders of a heartbeat. It does not block: each new order runs
// in its own goroutine, bound to ctx. An order delivered again is not applied
// again; if its report was not acknowledged, the report is sent again.
func (a *Applier) Handle(ctx context.Context, orders []v1.WorkflowApplyOrder) {
	for _, o := range orders {
		a.mu.Lock()
		if o.Generation > a.latest[lane(o)] {
			a.latest[lane(o)] = o.Generation
			for k, r := range a.known { // older orders of the lane are history
				if r.lane == lane(o) && k.generation < o.Generation && !r.busy {
					delete(a.known, k)
				}
			}
		}
		k := orderKey{o.ID, o.Generation}
		r, seen := a.known[k]
		switch {
		case seen && (r.busy || r.pending == nil):
			a.mu.Unlock()
			continue
		case !seen:
			r = &record{lane: lane(o)}
			a.known[k] = r
		}
		r.busy = true
		a.mu.Unlock()
		a.wg.Add(1)
		go func() {
			defer a.wg.Done()
			a.process(ctx, o, r)
		}()
	}
}

func (a *Applier) process(ctx context.Context, o v1.WorkflowApplyOrder, r *record) {
	a.mu.Lock()
	rep := r.pending
	a.mu.Unlock()
	if rep == nil {
		rep = a.execute(ctx, o)
	}
	if rep == nil { // nothing to report: superseded, or to try again at the next heartbeat
		a.finish(o, r, nil, true)
		return
	}
	resp, err := a.o.Client.ReportWorkflowApply(ctx, o.ID, *rep)
	if isInvalid(err) && ctx.Err() == nil {
		// The Controller refuses this report for good; sending it again would
		// never end. Say only what happened, so the order closes.
		a.log.Warn("the Controller refused a workflow apply report; sending a minimal one", "order", o.ID, "err", err)
		rep = minimal(rep, err)
		resp, err = a.o.Client.ReportWorkflowApply(ctx, o.ID, *rep)
		if isInvalid(err) {
			a.log.Error("the Controller refused the minimal workflow apply report; giving up on the order", "order", o.ID, "err", err)
			a.finish(o, r, nil, false)
			return
		}
	}
	switch {
	case err != nil && ctx.Err() == nil:
		a.log.Warn("reporting a workflow apply failed; it is sent again with the next delivery", "order", o.ID, "err", err)
		a.finish(o, r, rep, false)
	case err != nil:
		a.finish(o, r, rep, false)
	default:
		a.log.Info("workflow apply reported", "order", o.ID, "generation", o.Generation, "outcome", rep.Outcome, "accepted", resp.Accepted)
		a.finish(o, r, nil, false)
	}
}

// finish records that the work on r ended. forget makes the order new again for
// the next delivery.
func (a *Applier) finish(o v1.WorkflowApplyOrder, r *record, pending *v1.ReportWorkflowApplyRequest, forget bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	r.busy, r.pending = false, pending
	if !forget && pending == nil {
		delete(a.faults, orderKey{o.ID, o.Generation})
	}
	if forget {
		delete(a.known, orderKey{o.ID, o.Generation})
	}
}

// stale says whether a newer order than o has been seen in its lane.
func (a *Applier) stale(o v1.WorkflowApplyOrder) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.latest[lane(o)] > o.Generation
}

// lane is where orders supersede each other: per harness, dry runs apart from
// real orders, as the Controller keeps them (a dry run never supersedes an
// apply).
func lane(o v1.WorkflowApplyOrder) string {
	if o.DryRun {
		return o.Harness + "/dry-run"
	}
	return o.Harness
}

// execute does the order, under the lock of its harness, and returns the report
// for it, or nil when there is none to make: the order was superseded (the
// Controller ignores its result), or the failure is the Controller's being
// unreachable (the next delivery tries again).
func (a *Applier) execute(ctx context.Context, o v1.WorkflowApplyOrder) *v1.ReportWorkflowApplyRequest {
	unlock := a.lock(o.Harness)
	defer unlock()
	if a.stale(o) || ctx.Err() != nil {
		return nil
	}
	plan, inspected, drift, err := a.run(ctx, o)
	if err != nil {
		var coded *domain.Error
		if errors.As(err, &coded) && (coded.Code == domain.CodeStaleLease) {
			return nil
		}
		if errors.As(err, &coded) && coded.Code == domain.CodeControllerUnreachable {
			a.log.Warn("workflow apply postponed: the Controller cannot be reached", "order", o.ID, "err", err)
			return nil
		}
		if errors.As(err, &coded) && coded.Code == domain.CodeInternal && a.countFault(o) <= maxInternalFaults {
			a.log.Warn("workflow apply postponed: the Controller failed", "order", o.ID, "err", err)
			return nil
		}
		a.log.Warn("workflow apply failed", "order", o.ID, "harness", o.Harness, "err", err)
		return &v1.ReportWorkflowApplyRequest{Generation: o.Generation, Outcome: v1.ApplyStateFailed, Error: clip(err.Error(), maxErrorBytes)}
	}
	result, err := encodePlan(plan)
	if err != nil {
		return &v1.ReportWorkflowApplyRequest{Generation: o.Generation, Outcome: v1.ApplyStateFailed, Error: "the result cannot be encoded"}
	}
	return &v1.ReportWorkflowApplyRequest{
		Generation: o.Generation, Outcome: v1.ApplyStateApplied,
		Drift: head(drift, maxDriftItems), Inspected: inspected, Skipped: head(skippedOf(plan), maxDriftItems), Result: result,
	}
}

// run downloads what the order names and applies it. The order is checked
// against newer ones right before anything is written.
func (a *Applier) run(ctx context.Context, o v1.WorkflowApplyOrder) (v1.WorkflowApplyPlan, []string, []v1.DriftedComponent, error) {
	eng, ok := a.engines[o.Harness]
	if !ok {
		return v1.WorkflowApplyPlan{}, nil, nil, fmt.Errorf("this Worker cannot apply a Workflow globally to the harness %q", o.Harness)
	}
	v, err := a.o.Client.ApplyManifest(ctx, o.ID, o.Generation)
	if err != nil {
		return v1.WorkflowApplyPlan{}, nil, nil, err
	}
	if v.Digest != o.Digest || v.Manifest == nil {
		return v1.WorkflowApplyPlan{}, nil, nil, errors.New("the Controller sent a Workflow that is not the one the order names")
	}
	if err := v.Manifest.Validate(); err != nil {
		return v1.WorkflowApplyPlan{}, nil, nil, err
	}
	f := workflowfetch.Fetcher{
		DataDir: a.o.DataDir, Log: a.log,
		Open: func(ctx context.Context, digest string) (io.ReadCloser, int64, error) {
			return a.o.Client.OpenApplyBlob(ctx, o.ID, o.Generation, digest)
		},
	}
	snap, err := f.Snapshot(ctx, v.Manifest, o.Harness)
	if err != nil {
		return v1.WorkflowApplyPlan{}, nil, nil, err
	}
	if a.stale(o) || ctx.Err() != nil {
		return v1.WorkflowApplyPlan{}, nil, nil, domain.NewError(domain.CodeStaleLease, "a newer order superseded this one", nil)
	}
	res, err := eng.Apply(ctx, globalapply.Request{
		Snapshot: snap, WorkflowID: o.Workflow, Version: strconv.Itoa(o.Version),
		Components: o.Components, Force: o.Force, DryRun: o.DryRun, ExpectedPreview: o.ExpectedPreview,
	})
	if errors.Is(err, globalapply.ErrPreviewChanged) {
		return v1.WorkflowApplyPlan{}, nil, nil, errors.New("the host or the Workflow changed since the dry run the apply was based on; run the diff again")
	}
	if err != nil {
		return v1.WorkflowApplyPlan{}, nil, nil, err
	}
	plan := planOf(res)
	if len(o.Components) == 0 {
		return plan, nil, driftOf(plan), nil
	}
	// A selective order carries only the selected components (the Controller
	// narrows the manifest), so only those are inspected: the Controller keeps
	// the recorded drift of the rest.
	after, err := eng.Plan(ctx, globalapply.Request{Snapshot: snap, WorkflowID: o.Workflow, Version: strconv.Itoa(o.Version), Components: o.Components})
	if err != nil {
		return v1.WorkflowApplyPlan{}, nil, nil, err
	}
	return plan, slices.Clone(o.Components), driftOf(planOf(globalapply.Result{Plan: after})), nil
}

func planOf(res globalapply.Result) v1.WorkflowApplyPlan {
	out := v1.WorkflowApplyPlan{Components: []v1.WorkflowComponentPlan{}, Digest: res.Plan.Digest, NoOp: res.Plan.NoOp, Applied: res.Applied}
	for _, c := range res.Plan.Components {
		out.Components = append(out.Components, v1.WorkflowComponentPlan{
			Name: c.Name, Origin: string(c.Origin), Version: c.Version, Ownership: string(c.Ownership),
			Paths: nonNil(c.Paths), Action: string(c.Action), Reason: c.Reason, Drift: c.Drift, Refs: c.Refs, Values: c.Values,
		})
	}
	return out
}

// skippedOf is the selected components the apply did not write for drift.
func skippedOf(p v1.WorkflowApplyPlan) []string {
	var out []string
	for _, c := range p.Components {
		if c.Action == string(globalapply.ActionSkipDrift) {
			out = append(out, c.Name)
		}
	}
	return out
}

// driftOf is the components that differ from what was applied (managed) or
// from what the Workflow carries (local).
func driftOf(p v1.WorkflowApplyPlan) []v1.DriftedComponent {
	var out []v1.DriftedComponent
	for _, c := range p.Components {
		if c.Action == string(globalapply.ActionSkipDrift) || (c.Ownership == string(harness.Local) && c.Drift) {
			out = append(out, v1.DriftedComponent{Component: c.Name, Ownership: c.Ownership, Paths: slices.Clone(c.Paths)})
		}
	}
	return out
}

func nonNil(s []string) []string {
	if s == nil {
		return []string{}
	}
	return s
}

// countFault counts one more Controller internal fault for o.
func (a *Applier) countFault(o v1.WorkflowApplyOrder) int {
	a.mu.Lock()
	defer a.mu.Unlock()
	k := orderKey{o.ID, o.Generation}
	a.faults[k]++
	return a.faults[k]
}

// isInvalid says whether the Controller refused the request itself, which
// sending it again does not change.
func isInvalid(err error) bool {
	var coded *domain.Error
	return errors.As(err, &coded) && coded.Code == domain.CodeInvalidRequest
}

// reportedPlan is the Result: the plan, and whether it was cut to fit.
type reportedPlan struct {
	v1.WorkflowApplyPlan
	Truncated bool `json:"truncated,omitempty"`
}

// encodePlan is the plan as the Controller takes it: if it is too big, the
// contents (Values) go first, then components from the end, and the result
// says it was cut.
func encodePlan(p v1.WorkflowApplyPlan) (json.RawMessage, error) {
	out := reportedPlan{WorkflowApplyPlan: p}
	b, err := json.Marshal(out)
	if err != nil || len(b) <= maxResultBytes {
		return b, err
	}
	out.Truncated = true
	out.Components = slices.Clone(p.Components)
	for i := range out.Components {
		out.Components[i].Values = nil
	}
	for {
		if b, err = json.Marshal(out); err != nil || len(b) <= maxResultBytes || len(out.Components) == 0 {
			return b, err
		}
		out.Components = out.Components[:len(out.Components)/2]
	}
}

// minimal is the report that says only how the order ended, for a Controller
// that refused the full one. It keeps one skipped component, if any, so an
// apply that left one unwritten is still recorded as partial.
func minimal(rep *v1.ReportWorkflowApplyRequest, refused error) *v1.ReportWorkflowApplyRequest {
	msg := rep.Error
	if msg == "" {
		msg = "the full report was refused by the Controller: " + refused.Error()
	}
	out := &v1.ReportWorkflowApplyRequest{Generation: rep.Generation, Outcome: rep.Outcome, Error: clip(msg, maxErrorBytes)}
	if len(rep.Skipped) > 0 {
		out.Skipped = rep.Skipped[:1]
	}
	return out
}

// clip cuts s to at most n bytes of valid UTF-8.
func clip(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return strings.ToValidUTF8(s[:n-len("…")], "") + "…"
}

func head[T any](s []T, n int) []T {
	if len(s) > n {
		return s[:n]
	}
	return s
}
