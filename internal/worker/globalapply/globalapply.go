// Package globalapply is the Worker-local engine that applies a Workflow to
// the native configuration of a harness in the HOME of the Worker (a
// harness.TargetGlobal), with the rules of §8.12 and of D1-D4 of the phase 2
// close plan:
//
//   - components limits what is written and deleted; the rest keeps its
//     content, ownership and baseline;
//   - a baseline per component (what was applied) makes drift three-way:
//     a managed component edited on the host is not overwritten without Force,
//     a local one never is, and the same Workflow with no drift is a no-op
//     (Force repairs);
//   - secret markers are rendered from the local secret store before any
//     write; a missing one fails the whole apply untouched;
//   - everything an apply touches is backed up under the data directory with
//     a journal, and [Recover] restores a cut-off apply.
//
// It does no network and starts no goroutine: the daemon feeds it the
// snapshot it already downloaded and verified.
package globalapply

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"path/filepath"
	"slices"
	"sync"

	"github.com/Autarquico/codefabric/internal/adapters/harness"
)

// Action is what an apply does to a component.
type Action string

// The actions.
const (
	ActionWrite     Action = "write"
	ActionPreserve  Action = "preserve"
	ActionDelete    Action = "delete"
	ActionSkipDrift Action = "skip-drift"
)

// Origin is where the content a component has on the host comes from.
type Origin string

// The origins.
const (
	OriginWorkflow Origin = "workflow"
	OriginLocal    Origin = "local"
	OriginAbsent   Origin = "absent"
)

// ErrPreviewChanged is an apply whose Expected preview is not what the host
// and the Workflow give now: someone changed the host after the dry run.
var ErrPreviewChanged = errors.New("globalapply: the host changed since the preview")

// maxValueBytes bounds the unrendered content a plan shows per file.
const maxValueBytes = 16 << 10

// Engine applies to one harness of one host. Use one per harness and share it:
// it serialises its operations.
type Engine struct {
	Adapter harness.Adapter
	Host    harness.Host
	// DataDir is the data directory of the Worker; the baseline and the backup
	// live under DataDir/globalapply/<harness>.
	DataDir string
	// Secrets resolves the markers of the files. Without it, a file with
	// markers fails the apply.
	Secrets SecretResolver

	mu    sync.Mutex
	hooks hooks
}

// hooks are test seams for crashes.
type hooks struct {
	afterApply func()
}

// Request is one apply, dry run or explain.
type Request struct {
	// Snapshot is the desired configuration, unrendered.
	Snapshot harness.NativeSnapshot
	// WorkflowID and Version name what Snapshot is; they only label the
	// baseline and the explanation.
	WorkflowID string
	Version    string
	// Components limits the operation; empty is all.
	Components []string
	// Force overwrites managed components with drift and repairs those that
	// are already applied.
	Force bool
	// DryRun makes Apply return the plan without touching anything.
	DryRun bool
	// ExpectedPreview, when set, is the Digest of the Plan the person saw: the
	// apply fails with ErrPreviewChanged if it is not the one computed now.
	ExpectedPreview string
}

// ComponentPlan is what an apply does to a component, and why.
type ComponentPlan struct {
	Name      string            `json:"name"`
	Origin    Origin            `json:"origin"`
	Version   string            `json:"version,omitempty"`
	Ownership harness.Ownership `json:"ownership,omitempty"`
	// Paths are the absolute paths of the host the component lives in.
	Paths  []string `json:"paths"`
	Action Action   `json:"action"`
	Reason string   `json:"reason"`
	// Drift: a managed component was edited on the host since it was applied;
	// a local one differs from what the Workflow carries for it.
	Drift bool `json:"drift"`
	// Refs are the secret refs of the desired content, never their values.
	Refs []string `json:"refs,omitempty"`
	// Values is the content the Workflow carries by file, unrendered (a secret
	// is its marker), cut at 16 KiB. Only components of the Workflow have it:
	// what is on the host and not in the Workflow never leaves it.
	Values map[string]string `json:"values,omitempty"`

	desired, actual, applied string
	change                   bool // the adapter must be told about it
}

// Plan is the outcome of comparing baseline, host and Workflow.
type Plan struct {
	Components []ComponentPlan `json:"components"`
	// Digest identifies the state the plan was made from; a dry run reports it
	// and the apply that follows passes it back as Request.ExpectedPreview.
	Digest string `json:"digest"`
	// NoOp: nothing to write or delete.
	NoOp bool `json:"noop"`
}

// Result is what Apply did.
type Result struct {
	Plan Plan
	// Applied is false for a dry run and a no-op.
	Applied bool
}

func (e *Engine) harnessDir() string {
	return filepath.Join(e.DataDir, stateDirName, e.Adapter.ID())
}

func (e *Engine) inspector() (harness.GlobalInspector, error) {
	in, ok := e.Adapter.(harness.GlobalInspector)
	if !ok {
		return nil, fmt.Errorf("globalapply: harness %s cannot apply globally: %w", e.Adapter.ID(), harness.ErrUnsupported)
	}
	return in, nil
}

// Plan compares the baseline, the host and req.Snapshot. It writes nothing.
func (e *Engine) Plan(ctx context.Context, req Request) (Plan, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.plan(ctx, req)
}

func (e *Engine) plan(ctx context.Context, req Request) (Plan, error) {
	in, err := e.inspector()
	if err != nil {
		return Plan{}, err
	}
	snap := req.Snapshot
	if len(snap.Components) == 0 && snap.Harness == "" {
		snap.Harness, snap.SchemaVersion = e.Adapter.ID(), 0
	}
	if snap.Harness != e.Adapter.ID() {
		return Plan{}, fmt.Errorf("globalapply: snapshot of %s for harness %s", snap.Harness, e.Adapter.ID())
	}
	if _, err := in.GlobalPaths(e.Host, req.Components); err != nil { // validates the names
		return Plan{}, err
	}
	sel := map[string]bool{}
	for _, n := range req.Components {
		sel[n] = true
	}
	selected := func(n string) bool { return len(sel) == 0 || sel[n] }

	bl, err := readBaseline(e.harnessDir())
	if err != nil {
		return Plan{}, err
	}
	actual, err := in.GlobalFingerprint(ctx, e.Host, nil)
	if err != nil {
		return Plan{}, err
	}
	diff, err := e.Adapter.DiffConfig(ctx, e.Host, snap)
	if err != nil {
		return Plan{}, err
	}
	kinds := map[string]harness.ChangeKind{}
	for _, ch := range diff.Changes {
		kinds[ch.Name] = ch.Kind
	}
	onHost, err := e.Adapter.SnapshotConfig(ctx, e.Host, harness.SnapshotOptions{})
	if err != nil {
		return Plan{}, err
	}

	var plan Plan
	seen := map[string]bool{}
	for _, c := range snap.Components {
		seen[c.Name] = true
		cp := ComponentPlan{Name: c.Name, Ownership: c.Ownership, desired: c.Digest, actual: actual[c.Name]}
		cp.Refs, cp.Values = describe(c)
		b, hasBase := bl.Components[c.Name]
		e.place(&cp, in, b, hasBase, kinds[c.Name])
		e.decide(&cp, c, b, hasBase, kinds[c.Name], selected(c.Name), req)
		plan.Components = append(plan.Components, cp)
	}
	for _, name := range slices.Sorted(keys(bl.Components)) {
		if seen[name] {
			continue
		}
		b := bl.Components[name]
		cp := ComponentPlan{Name: name, Ownership: b.Ownership, actual: actual[name]}
		e.place(&cp, in, b, true, "")
		e.decideRemoval(&cp, b, selected(name), req)
		plan.Components = append(plan.Components, cp)
		seen[name] = true
	}
	// Components the adapter recorded as applied but this baseline does not
	// know (applied before there was one): the diff says they are removed.
	for _, ch := range diff.Changes {
		if ch.Kind != harness.Removed || seen[ch.Name] {
			continue
		}
		cp := ComponentPlan{Name: ch.Name, Ownership: harness.Managed, actual: actual[ch.Name]}
		e.place(&cp, in, baselineEntry{}, false, "")
		e.decideRemoval(&cp, baselineEntry{}, selected(ch.Name), req)
		plan.Components = append(plan.Components, cp)
		seen[ch.Name] = true
	}
	digest := sha256.New()
	for _, c := range plan.Components {
		_, _ = fmt.Fprintf(digest, "%s|%s|%s|%s|%s|%t\n", c.Name, c.Action, c.desired, c.actual, c.applied, c.Drift)
	}
	plan.Digest = "sha256:" + hex.EncodeToString(digest.Sum(nil))
	plan.NoOp = true
	for _, c := range plan.Components {
		if c.change {
			plan.NoOp = false
		}
	}
	// What is on the host and nobody here manages: shown, never part of the
	// digest, never touched.
	for _, h := range onHost.Components { // name, ownership and paths only: its content stays on the host
		if !seen[h.Name] {
			cp := ComponentPlan{Name: h.Name, Origin: OriginLocal, Ownership: harness.Local, Action: ActionPreserve, Reason: "on the host and not in the Workflow"}
			cp.Paths, _ = in.GlobalPaths(e.Host, []string{h.Name})
			if len(cp.Paths) > 0 {
				cp.Paths = cp.Paths[1:] // not the ownership record
			}
			plan.Components = append(plan.Components, cp)
		}
	}
	return plan, nil
}

// place sets the origin, version and paths of cp.
func (e *Engine) place(cp *ComponentPlan, in harness.GlobalInspector, b baselineEntry, hasBase bool, kind harness.ChangeKind) {
	cp.Paths, _ = in.GlobalPaths(e.Host, []string{cp.Name})
	if len(cp.Paths) > 0 {
		cp.Paths = cp.Paths[1:]
	}
	switch {
	case hasBase:
		cp.Origin, cp.Version, cp.applied = OriginWorkflow, b.WorkflowVersion, b.Applied
		cp.Drift = cp.actual != b.Applied
	case cp.actual != "" || (kind != "" && kind != harness.Added):
		cp.Origin = OriginLocal
	default:
		cp.Origin = OriginAbsent
	}
}

func (e *Engine) decide(cp *ComponentPlan, c harness.Component, b baselineEntry, hasBase bool, kind harness.ChangeKind, selected bool, req Request) {
	switch {
	case !selected:
		cp.Action, cp.Reason = ActionPreserve, "not selected"
		cp.Drift = false
	case c.Ownership == harness.Local:
		cp.Action, cp.Reason = ActionPreserve, "local: never written"
		// Drift of a local component: what the host has differs from what the
		// Workflow carries for it. It is reported, never written.
		cp.Drift = cp.actual != "" && kind != "" && kind != harness.Added
		if cp.Drift {
			cp.Reason = "local: differs from the Workflow, never written"
		}
		if hasBase { // ceded to the person: the adapter forgets it owned it
			cp.change = true
			cp.Reason = "local: ceded to the person"
		}
	case !hasBase && kind == "":
		cp.Action, cp.Reason = ActionPreserve, "already as the Workflow has it"
		cp.Drift = false
		if req.Force {
			cp.Action, cp.Reason, cp.change = ActionWrite, "forced", true
		}
	case !hasBase && kind != harness.Added && !req.Force:
		// Something of the host the Workflow would replace, with no baseline to
		// say it was ours: it counts as edited.
		cp.Action, cp.Reason, cp.Drift = ActionSkipDrift, "on the host with other content and never applied here", true
	case !hasBase && kind != harness.Added:
		cp.Action, cp.Reason, cp.change = ActionWrite, "forced over what the host had", true
	case !hasBase:
		cp.Action, cp.Reason, cp.change = ActionWrite, "first apply", true
	case cp.Drift && !req.Force:
		cp.Action, cp.Reason = ActionSkipDrift, "edited on the host since it was applied"
	case cp.Drift:
		cp.Action, cp.Reason, cp.change = ActionWrite, "forced over drift", true
	case b.Desired != c.Digest:
		cp.Action, cp.Reason, cp.change = ActionWrite, "the Workflow changed", true
	case req.Force:
		cp.Action, cp.Reason, cp.change = ActionWrite, "forced repair", true
	default:
		cp.Action, cp.Reason = ActionPreserve, "already applied"
	}
}

func (e *Engine) decideRemoval(cp *ComponentPlan, b baselineEntry, selected bool, req Request) {
	switch {
	case !selected:
		cp.Action, cp.Reason = ActionPreserve, "not selected"
		cp.Drift = false
	case cp.actual == "":
		// Already gone: only the baseline is forgotten.
		cp.Action, cp.Reason, cp.change = ActionDelete, "already absent from the host", true
		cp.Drift = false
	case cp.Drift && !req.Force:
		cp.Action, cp.Reason = ActionSkipDrift, "the Workflow dropped it but it was edited on the host"
	default:
		cp.Action, cp.Reason, cp.change = ActionDelete, "the Workflow no longer has it", true
	}
	cp.Origin = OriginWorkflow
	if cp.Version == "" {
		cp.Version = b.WorkflowVersion
	}
	if cp.actual == "" {
		cp.Origin = OriginAbsent
	}
}

func describe(c harness.Component) (refs []string, values map[string]string) {
	values = map[string]string{}
	for _, f := range c.Files {
		refs = append(refs, f.SecretRefs...)
		v := string(f.Content)
		if len(v) > maxValueBytes {
			v = v[:maxValueBytes] + "…"
		}
		values[f.Path] = v
	}
	slices.Sort(refs)
	return slices.Compact(refs), values
}

// Apply plans, and unless req.DryRun or the plan is a no-op, backs up what it
// will touch, renders the secrets, writes through the adapter and records the
// baseline. On any error it restores the host and returns it; a crash is left
// for [Recover].
func (e *Engine) Apply(ctx context.Context, req Request) (Result, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if _, err := restoreBackup(e.harnessDir()); err != nil {
		return Result{}, err
	}
	plan, err := e.plan(ctx, req)
	if err != nil {
		return Result{}, err
	}
	if req.ExpectedPreview != "" && req.ExpectedPreview != plan.Digest {
		return Result{Plan: plan}, ErrPreviewChanged
	}
	if req.DryRun || plan.NoOp {
		return Result{Plan: plan}, nil
	}
	in, _ := e.inspector()

	// What the adapter is told: the components that change, rendered. A secret
	// that cannot be rendered stops here, before anything is written.
	var names []string
	snap := harness.NativeSnapshot{Harness: req.Snapshot.Harness, SchemaVersion: req.Snapshot.SchemaVersion}
	byName := map[string]harness.Component{}
	for _, c := range req.Snapshot.Components {
		byName[c.Name] = c
	}
	for _, cp := range plan.Components {
		if !cp.change {
			continue
		}
		names = append(names, cp.Name)
		if cp.Action == ActionDelete {
			continue
		}
		resolve := e.Secrets
		if cp.Action == ActionPreserve {
			// Ceded to the person: told to the adapter so it forgets it owned
			// it, never written. Its markers become empty values.
			resolve = func(string) ([]byte, error) { return nil, nil }
		}
		r, err := renderComponent(e.Adapter, resolve, byName[cp.Name])
		if err != nil {
			return Result{Plan: plan}, err
		}
		snap.Components = append(snap.Components, r)
	}
	if snap.Harness == "" {
		snap.Harness = e.Adapter.ID()
	}
	paths, err := in.GlobalPaths(e.Host, names)
	if err != nil {
		return Result{Plan: plan}, err
	}
	paths = append(paths, baselinePath(e.harnessDir()))
	if err := beginBackup(e.harnessDir(), paths); err != nil {
		_, _ = restoreBackup(e.harnessDir())
		return Result{Plan: plan}, err
	}
	if err := e.write(ctx, in, plan, snap, names, req); err != nil {
		if _, rerr := restoreBackup(e.harnessDir()); rerr != nil {
			return Result{Plan: plan}, errors.Join(err, rerr)
		}
		return Result{Plan: plan}, err
	}
	if err := commitBackup(e.harnessDir()); err != nil {
		return Result{Plan: plan, Applied: true}, err
	}
	return Result{Plan: plan, Applied: true}, nil
}

func (e *Engine) write(ctx context.Context, in harness.GlobalInspector, plan Plan, snap harness.NativeSnapshot, names []string, req Request) error {
	if _, err := e.Adapter.ApplyConfig(ctx, e.Host, snap, harness.ApplyOptions{Target: harness.TargetGlobal{}, Components: names}); err != nil {
		return err
	}
	if e.hooks.afterApply != nil {
		e.hooks.afterApply()
	}
	after, err := in.GlobalFingerprint(ctx, e.Host, names)
	if err != nil {
		return err
	}
	bl, err := readBaseline(e.harnessDir())
	if err != nil {
		return err
	}
	for _, cp := range plan.Components {
		if !cp.change {
			continue
		}
		switch cp.Action {
		case ActionWrite:
			bl.Components[cp.Name] = baselineEntry{
				WorkflowID: req.WorkflowID, WorkflowVersion: req.Version,
				Ownership: cp.Ownership, Desired: cp.desired, Applied: after[cp.Name],
			}
		default: // delete, or a component ceded to the person
			delete(bl.Components, cp.Name)
		}
	}
	return writeBaseline(e.harnessDir(), bl)
}

func keys[V any](m map[string]V) func(yield func(string) bool) {
	return func(yield func(string) bool) {
		for k := range m {
			if !yield(k) {
				return
			}
		}
	}
}
