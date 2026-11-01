package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"

	v1 "github.com/Autarquico/codefabric/internal/api/v1"
	"github.com/Autarquico/codefabric/internal/domain"
)

// workflowApplyStore is the part of *client.Client the apply commands use.
type workflowApplyStore interface {
	ApplyWorkflow(ctx context.Context, req v1.ApplyWorkflowRequest) (v1.ApplyWorkflowResponse, error)
	RollbackWorkflow(ctx context.Context, req v1.RollbackWorkflowRequest) (v1.ApplyWorkflowResponse, error)
	SyncWorkflow(ctx context.Context, req v1.SyncWorkflowRequest) (v1.SyncWorkflowResponse, error)
	WorkflowApply(ctx context.Context, id string) (v1.WorkflowApply, error)
	WorkflowState(ctx context.Context, worker, harness string) ([]v1.WorkflowState, error)
}

// defaultApplyPoll is how often --wait asks the Controller about the order.
const defaultApplyPoll = 500 * time.Millisecond

const (
	flagWorker     = "worker"
	flagComponents = "components"
	flagDryRun     = "dry-run"
	flagForce      = "force"
	flagDetach     = "detach"
	flagPreview    = "preview"
	flagTimeout    = "timeout"
	flagValues     = "values"
	flagNoWait     = "no-wait"

	defaultApplyTimeout = 2 * time.Minute
	actionSkipDrift     = "skip-drift"
)

// applyFlags are the choices the apply-family commands share.
type applyFlags struct {
	worker, harness string
	components      []string
	dryRun, force   bool
	detach          bool
	noWait          bool
	preview         string
	timeout         time.Duration
	values          bool
}

func (f *applyFlags) bindTarget(cmd *cobra.Command, withWorker bool) {
	fl := cmd.Flags()
	if withWorker {
		fl.StringVar(&f.worker, flagWorker, "", "Worker to apply to (name or ID); required")
	}
	fl.StringVar(&f.harness, flagHarness, "", "harness to apply (claude-code, codex); required")
	fl.StringSliceVar(&f.components, flagComponents, nil, "components to touch (comma list); default: all of the harness")
	fl.BoolVar(&f.force, flagForce, false, "overwrite managed components edited locally since the last apply (local ones are never written)")
}

func (f *applyFlags) bindWait(cmd *cobra.Command) {
	fl := cmd.Flags()
	fl.BoolVar(&f.detach, flagDetach, false, "return once the order is made, without waiting for the Worker to report it")
	fl.DurationVar(&f.timeout, flagTimeout, defaultApplyTimeout, "how long to wait for the Worker (unless --detach)")
}

func (f *applyFlags) bindNoWait(cmd *cobra.Command) {
	cmd.Flags().BoolVar(&f.noWait, flagNoWait, false,
		"fail with CF_WORKER_BUSY instead of queueing the order behind Runs that hold the harness configuration (a dry run never waits)")
}

func (f *applyFlags) bindDryRun(cmd *cobra.Command) {
	cmd.Flags().BoolVar(&f.dryRun, flagDryRun, false, "show what would change without writing anything")
}

func (f *applyFlags) bindValues(cmd *cobra.Command) {
	cmd.Flags().BoolVar(&f.values, flagValues, false, "also show the desired values, unrendered (a secret stays its marker)")
}

func (f *applyFlags) check(needWorker bool) error {
	switch {
	case needWorker && f.worker == "":
		return usageError(errors.New("--" + flagWorker + " is required"))
	case f.harness == "":
		return usageError(errors.New("--" + flagHarness + " is required"))
	case f.timeout < 0:
		return usageError(errors.New("--" + flagTimeout + " must not be negative"))
	}
	return nil
}

func newWorkflowApplyCmds(deps workflowDeps) []*cobra.Command {
	return []*cobra.Command{
		newWorkflowStatusCmd(deps), newWorkflowDiffCmd(deps), newWorkflowApplyVerbCmd(deps),
		newWorkflowRollbackCmd(deps), newWorkflowExplainCmd(deps), newWorkflowSyncCmd(deps),
	}
}

// view is what a printed plan shows.
type view int

const (
	viewApply view = iota
	viewDiff
	viewExplain
)

type applyView struct {
	kind   view
	values bool
}

func newWorkflowApplyVerbCmd(deps workflowDeps) *cobra.Command {
	f := applyFlags{}
	cmd := &cobra.Command{
		Use:   "apply <workflow[@version]>",
		Short: "Apply a Workflow to the native configuration of a Worker",
		Long: "Orders a Worker to write a Workflow version into the global configuration of a harness. " +
			"A managed component edited locally since the last apply is skipped (and listed) unless --force; a local one is never written. " +
			"Applying what the Worker already holds is a no-op.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := f.check(true); err != nil {
				return err
			}
			return runApplyOne(cmd, deps, f, applyView{kind: viewApply, values: f.values}, func(ctx context.Context, st workflowApplyStore) (v1.ApplyWorkflowResponse, error) {
				return st.ApplyWorkflow(ctx, f.applyRequest(args[0]))
			})
		},
	}
	f.bindTarget(cmd, true)
	f.bindDryRun(cmd)
	cmd.Flags().StringVar(&f.preview, flagPreview, "", "plan digest printed by `cf workflow diff`: fail instead of applying if the plan has changed since")
	f.bindNoWait(cmd)
	f.bindWait(cmd)
	f.bindValues(cmd)
	return cmd
}

func newWorkflowRollbackCmd(deps workflowDeps) *cobra.Command {
	f := applyFlags{}
	cmd := &cobra.Command{
		Use:   "rollback",
		Short: "Reapply the version a Worker held before the current one",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if err := f.check(true); err != nil {
				return err
			}
			return runApplyOne(cmd, deps, f, applyView{kind: viewApply, values: f.values}, func(ctx context.Context, st workflowApplyStore) (v1.ApplyWorkflowResponse, error) {
				return st.RollbackWorkflow(ctx, v1.RollbackWorkflowRequest{
					Worker: f.worker, Harness: f.harness, Components: f.components, Force: f.force, DryRun: f.dryRun, NoWait: f.noWait,
				})
			})
		},
	}
	f.bindTarget(cmd, true)
	f.bindDryRun(cmd)
	f.bindNoWait(cmd)
	f.bindWait(cmd)
	f.bindValues(cmd)
	return cmd
}

func newWorkflowDiffCmd(deps workflowDeps) *cobra.Command {
	f := applyFlags{dryRun: true}
	cmd := &cobra.Command{
		Use:   "diff <workflow[@version]>",
		Short: "Show what applying a Workflow would change on a Worker (a dry run)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := f.check(true); err != nil {
				return err
			}
			return runApplyOne(cmd, deps, f, applyView{kind: viewDiff, values: f.values}, func(ctx context.Context, st workflowApplyStore) (v1.ApplyWorkflowResponse, error) {
				return st.ApplyWorkflow(ctx, f.applyRequest(args[0]))
			})
		},
	}
	f.bindTarget(cmd, true)
	f.bindWait(cmd)
	f.bindValues(cmd)
	return cmd
}

func newWorkflowExplainCmd(deps workflowDeps) *cobra.Command {
	f := applyFlags{dryRun: true}
	cmd := &cobra.Command{
		Use:   "explain <workflow[@version]>",
		Short: "Show the origin of each component before applying it (a dry run)",
		Long: "Asks the Worker for a dry run and prints, per component: where it comes from (the Workflow version, local, or absent), " +
			"its ownership, the paths it touches, what applying would do and why, and the secret refs it uses (names only).",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := f.check(true); err != nil {
				return err
			}
			return runApplyOne(cmd, deps, f, applyView{kind: viewExplain, values: f.values}, func(ctx context.Context, st workflowApplyStore) (v1.ApplyWorkflowResponse, error) {
				return st.ApplyWorkflow(ctx, f.applyRequest(args[0]))
			})
		},
	}
	f.bindTarget(cmd, true)
	f.bindWait(cmd)
	f.bindValues(cmd)
	return cmd
}

func newWorkflowSyncCmd(deps workflowDeps) *cobra.Command {
	f := applyFlags{}
	cmd := &cobra.Command{
		Use:   "sync <workflow[@version]>",
		Short: "Apply a Workflow to every online Worker",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := f.check(false); err != nil {
				return err
			}
			return runSync(cmd, deps, f, args[0])
		},
	}
	f.bindTarget(cmd, false)
	f.bindDryRun(cmd)
	f.bindNoWait(cmd)
	f.bindWait(cmd)
	return cmd
}

func newWorkflowStatusCmd(deps workflowDeps) *cobra.Command {
	var worker, harness string
	cmd := &cobra.Command{
		Use:   "status",
		Short: "Show what each Worker holds of each harness: version, drift and the latest order",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			st, err := deps.openApply(cmd)
			if err != nil {
				return err
			}
			states, err := st.WorkflowState(cmd.Context(), worker, harness)
			if err != nil {
				return err
			}
			return emit(cmd, v1.WorkflowStateResponse{States: states}, func(out io.Writer) error {
				return writeStates(out, states)
			})
		},
	}
	cmd.Flags().StringVar(&worker, flagWorker, "", "only this Worker (name or ID)")
	cmd.Flags().StringVar(&harness, flagHarness, "", "only this harness")
	return cmd
}

func (f applyFlags) applyRequest(workflow string) v1.ApplyWorkflowRequest {
	return v1.ApplyWorkflowRequest{
		Worker: f.worker, Harness: f.harness, Workflow: workflow, Components: f.components, Force: f.force, DryRun: f.dryRun,
		ExpectedPreview: f.preview, NoWait: f.noWait,
	}
}

func runApplyOne(cmd *cobra.Command, deps workflowDeps, f applyFlags, v applyView, call func(context.Context, workflowApplyStore) (v1.ApplyWorkflowResponse, error)) error {
	st, err := deps.openApply(cmd)
	if err != nil {
		return err
	}
	resp, err := call(cmd.Context(), st)
	if err != nil {
		return err
	}
	if resp.Apply != nil && !f.detach {
		settled, err := deps.await(cmd.Context(), st, *resp.Apply, f.timeout)
		if err != nil {
			return err
		}
		resp.Apply = &settled
	}
	failure := applyOutcome(resp)
	if failure == nil {
		return emit(cmd, resp, func(out io.Writer) error { return writeResponse(out, resp, v) })
	}
	// A failed order still says what the Worker reported: shown, then the
	// failure. With --json the order travels in the details of the error.
	if on, _ := cmd.Flags().GetBool(flagJSON); !on {
		if err := writeResponse(cmd.OutOrStdout(), resp, v); err != nil {
			return err
		}
	}
	return failure
}

func runSync(cmd *cobra.Command, deps workflowDeps, f applyFlags, workflow string) error {
	st, err := deps.openApply(cmd)
	if err != nil {
		return err
	}
	resp, err := st.SyncWorkflow(cmd.Context(), v1.SyncWorkflowRequest{
		Harness: f.harness, Workflow: workflow, Components: f.components, Force: f.force, DryRun: f.dryRun, NoWait: f.noWait,
	})
	if err != nil {
		return err
	}
	var failed, busy []string
	for i := range resp.Results {
		r := &resp.Results[i]
		if r.Code == string(domain.CodeWorkerBusy) {
			busy = append(busy, r.Worker)
			continue
		}
		if r.Result.Apply != nil && !f.detach {
			settled, err := deps.await(cmd.Context(), st, *r.Result.Apply, f.timeout)
			if err != nil {
				return err
			}
			r.Result.Apply = &settled
		}
		if applyOutcome(r.Result) != nil {
			failed = append(failed, r.Worker)
		}
	}
	human := func(out io.Writer) error { return writeSync(out, resp) }
	if len(failed)+len(busy) == 0 {
		return emit(cmd, resp, human)
	}
	if on, _ := cmd.Flags().GetBool(flagJSON); !on {
		if err := human(cmd.OutOrStdout()); err != nil {
			return err
		}
	}
	code := domain.CodeInternal
	if len(failed) == 0 {
		code = domain.CodeWorkerBusy
	}
	not := append(slices.Clone(failed), busy...)
	return domain.NewError(code, fmt.Sprintf("%d of %d Workers did not apply: %s", len(not), len(resp.Results), strings.Join(not, ", ")),
		map[string]any{"failed": failed, "busy": busy, "results": resp.Results})
}

// await polls the order until the Worker closes it or the timeout runs out,
// both measured with the timer of deps. The returned order is the last one read.
func (d workflowDeps) await(ctx context.Context, st workflowApplyStore, a v1.WorkflowApply, timeout time.Duration) (v1.WorkflowApply, error) {
	deadline, stopDeadline := d.timer(timeout)
	defer stopDeadline()
	for !closedOrder(a.State) {
		tick, stopTick := d.timer(d.poll)
		select {
		case <-ctx.Done():
			stopTick()
			return a, stillRunning(a, timeout)
		case <-deadline:
			stopTick()
			return a, stillRunning(a, timeout)
		case <-tick:
			stopTick()
		}
		next, err := st.WorkflowApply(ctx, a.ID)
		if err != nil {
			if ctx.Err() != nil {
				return a, stillRunning(a, timeout)
			}
			return a, err
		}
		a = next
	}
	return a, nil
}

func stillRunning(a v1.WorkflowApply, timeout time.Duration) error {
	return domain.NewError(domain.CodeInProgress,
		fmt.Sprintf("the Worker has not finished %s after %s; it keeps going: see `cf workflow status`", a.ID, timeout),
		map[string]any{"apply": a.ID, "state": a.State})
}

func closedOrder(state string) bool {
	return state == v1.ApplyStateApplied || state == v1.ApplyStateFailed || state == v1.ApplyStateSuperseded
}

// applyOutcome turns an order that closed without applying into an error.
func applyOutcome(r v1.ApplyWorkflowResponse) error {
	a := r.Apply
	if a == nil {
		return nil
	}
	switch a.State {
	case v1.ApplyStateFailed:
		return domain.NewError(domain.CodeInternal, fmt.Sprintf("%s on %s failed: %s", a.ID, printable(a.Worker), printable(dash(a.Error))),
			map[string]any{"apply": a.ID, "worker": a.Worker, "order": a})
	case v1.ApplyStateSuperseded:
		return domain.NewError(domain.CodeInvalidTransition, fmt.Sprintf("%s on %s was superseded by a newer order", a.ID, printable(a.Worker)),
			map[string]any{"apply": a.ID, "worker": a.Worker})
	}
	return nil
}

func planOf(a *v1.WorkflowApply) (v1.WorkflowApplyPlan, bool) {
	var p v1.WorkflowApplyPlan
	if a == nil || len(a.Result) == 0 || json.Unmarshal(a.Result, &p) != nil {
		return p, false
	}
	return p, true
}

func writeResponse(out io.Writer, r v1.ApplyWorkflowResponse, v applyView) error {
	if r.Apply == nil {
		_, err := fmt.Fprintf(out, "No changes: %s\n", printable(dash(r.Reason)))
		return err
	}
	a := r.Apply
	mode := a.Mode
	if a.DryRun {
		mode += " (dry run)"
	}
	fmt.Fprintf(out, "%s %s v%d (%s) on %s/%s: %s\n", mode, printable(a.Workflow), a.Version, shortDigest(a.Digest),
		printable(a.Worker), printable(a.Harness), a.State)
	if !closedOrder(a.State) {
		fmt.Fprintf(out, "Order %s; follow it with `cf workflow status --worker %s`.\n", a.ID, printable(a.Worker))
		return nil
	}
	plan, ok := planOf(a)
	if !ok {
		return nil
	}
	return writePlan(out, plan, v, a.Force)
}

func writePlan(out io.Writer, p v1.WorkflowApplyPlan, v applyView, forced bool) error {
	if p.NoOp {
		fmt.Fprintln(out, "No changes.")
	}
	if v.kind == viewDiff && p.Digest != "" {
		fmt.Fprintf(out, "Plan digest: %s (apply exactly this plan with --%s %s)\n", p.Digest, flagPreview, p.Digest)
	}
	switch v.kind {
	case viewExplain:
		writeExplain(out, p, v.values)
	default:
		if err := writePlanTable(out, p, v.kind == viewDiff); err != nil {
			return err
		}
		if v.values {
			writeValues(out, p)
		}
	}
	if skipped := skippedForDrift(p); len(skipped) > 0 && !forced {
		fmt.Fprintf(out, "\nSkipped, edited locally since the last apply: %s.\nNothing local was written; rerun with --force to overwrite the managed ones.\n", strings.Join(skipped, ", "))
	}
	return nil
}

func skippedForDrift(p v1.WorkflowApplyPlan) []string {
	var out []string
	for _, c := range p.Components {
		if c.Action == actionSkipDrift {
			out = append(out, printable(c.Name))
		}
	}
	return out
}

func writePlanTable(out io.Writer, p v1.WorkflowApplyPlan, withPaths bool) error {
	header := "COMPONENT\tACTION\tREASON"
	if withPaths {
		header += "\tPATHS"
	}
	w := newTable(out, header)
	for _, c := range p.Components {
		fmt.Fprintf(w, "%s\t%s\t%s", printable(c.Name), printable(c.Action), printable(dash(c.Reason)))
		if withPaths {
			fmt.Fprintf(w, "\t%s", printable(dash(strings.Join(c.Paths, ", "))))
		}
		fmt.Fprintln(w)
	}
	return w.Flush()
}

func writeExplain(out io.Writer, p v1.WorkflowApplyPlan, values bool) {
	for i, c := range p.Components {
		if i > 0 {
			fmt.Fprintln(out)
		}
		fmt.Fprintln(out, printable(c.Name))
		origin := c.Origin
		if c.Version != "" {
			origin += " " + c.Version
		}
		fmt.Fprintf(out, "  origin     %s\n", printable(dash(origin)))
		fmt.Fprintf(out, "  ownership  %s\n", printable(dash(c.Ownership)))
		fmt.Fprintf(out, "  paths      %s\n", printable(dash(strings.Join(c.Paths, ", "))))
		fmt.Fprintf(out, "  action     %s: %s\n", printable(c.Action), printable(dash(c.Reason)))
		fmt.Fprintf(out, "  refs       %s\n", printable(dash(strings.Join(c.Refs, ", "))))
		if values {
			writeComponentValues(out, c, "  ")
		}
	}
}

func writeValues(out io.Writer, p v1.WorkflowApplyPlan) {
	for _, c := range p.Components {
		if len(c.Values) == 0 {
			continue
		}
		fmt.Fprintf(out, "\n%s\n", printable(c.Name))
		writeComponentValues(out, c, "  ")
	}
}

func writeComponentValues(out io.Writer, c v1.WorkflowComponentPlan, indent string) {
	for _, k := range slices.Sorted(maps.Keys(c.Values)) {
		fmt.Fprintf(out, "%s%s = %s\n", indent, printable(k), printable(c.Values[k]))
	}
}

func writeSync(out io.Writer, r v1.SyncWorkflowResponse) error {
	w := newTable(out, "WORKER\tSTATE\tORDER\tDETAIL")
	for _, res := range r.Results {
		state, order, detail := "no-op", "-", res.Result.Reason
		if res.Code == string(domain.CodeWorkerBusy) {
			state = "busy"
		}
		if a := res.Result.Apply; a != nil {
			state, order, detail = a.State, a.ID, a.Error
			if plan, ok := planOf(a); ok {
				if plan.NoOp && a.State == v1.ApplyStateApplied {
					state = "no-op"
				}
				if skipped := skippedForDrift(plan); len(skipped) > 0 && !a.Force {
					detail = "skipped (local edits, use --force): " + strings.Join(skipped, ", ")
				}
			}
		}
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\n", printable(res.Worker), state, order, printable(dash(detail)))
	}
	return w.Flush()
}

func writeStates(out io.Writer, states []v1.WorkflowState) error {
	w := newTable(out, "WORKER\tHARNESS\tWORKFLOW\tAPPLIED\tDIGEST\tCOMPONENTS\tPREVIOUS\tDRIFT\tLOCK\tLATEST")
	for _, s := range states {
		applied, previous := "-", "-"
		if s.AppliedVersion > 0 {
			applied = "v" + strconv.Itoa(s.AppliedVersion)
			if s.Partial {
				applied += " (partial)"
			}
		}
		if s.PreviousVersion > 0 {
			previous = "v" + strconv.Itoa(s.PreviousVersion)
		}
		components := "all"
		if len(s.Components) > 0 {
			components = strings.Join(s.Components, ",")
		}
		latest := "-"
		if s.Latest != nil {
			latest = fmt.Sprintf("%s %s", s.Latest.Mode, s.Latest.State)
			if s.Latest.DryRun {
				latest += " (dry run)"
			}
		}
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\t%s\t%d\t%s\t%s\n", printable(s.Worker), printable(s.Harness), printable(dash(s.Workflow)),
			applied, dash(shortDigest(s.AppliedDigest)), printable(components), previous, len(s.Drift), lockCell(s), printable(latest))
	}
	if err := w.Flush(); err != nil {
		return err
	}
	for _, s := range states {
		for _, d := range s.Drift {
			fmt.Fprintf(out, "drift: %s/%s %s %s: %s\n", printable(s.Worker), printable(s.Harness), printable(d.Component), d.Ownership, printable(dash(strings.Join(d.Paths, ", "))))
		}
	}
	return nil
}

// lockCell is the configuration lock (§8.12) of a row: the config_key its
// holders share, how many there are and the order waiting for them.
func lockCell(s v1.WorkflowState) string {
	if s.Holders == 0 && s.ConfigKey == "" && s.ApplyPending == "" {
		return "-"
	}
	cell := fmt.Sprintf("holders=%d key=%s", s.Holders, dash(shortDigest(s.ConfigKey)))
	if s.ApplyPending != "" {
		cell += " apply_pending=" + printable(s.ApplyPending)
	}
	return printable(cell)
}
