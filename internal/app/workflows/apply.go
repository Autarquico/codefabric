package workflows

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"

	v1 "github.com/Autarquico/codefabric/internal/api/v1"
	"github.com/Autarquico/codefabric/internal/app/input"
	"github.com/Autarquico/codefabric/internal/app/mutation"
	"github.com/Autarquico/codefabric/internal/controller/pki"
	"github.com/Autarquico/codefabric/internal/controller/store"
	"github.com/Autarquico/codefabric/internal/domain"
)

// Applying a Workflow to a Worker (§8.6) is a durable order. The Controller
// records it (with its event and audit entry, in one transaction), the Worker
// finds it in its heartbeat, executes it and reports. One order is alive per
// Worker and harness: a newer one supersedes it and raises the generation, so
// the report of the older one is ignored.

// target is what one order is made for, already resolved and validated.
type target struct {
	worker     store.Worker
	harness    string
	name       string
	ref        domain.WorkflowRef
	manifest   domain.Manifest
	components []string // nil = all of the harness
	mode       string
	force      bool
	dryRun     bool
	preview    string // plan digest the person saw, "" for none
}

// Apply orders the Worker to apply the Workflow the selector names.
func (s *Service) Apply(ctx context.Context, by pki.Session, req v1.ApplyWorkflowRequest) (v1.ApplyWorkflowResponse, error) {
	if err := input.First(input.Label("harness", req.Harness), input.Ref("workflow", req.Workflow), input.Ref("worker", req.Worker)); err != nil {
		return v1.ApplyWorkflowResponse{}, err
	}
	return mutation.Do(ctx, s.db, s.clock, by, "workflow.apply", req.RequestID, req,
		func(tx *store.Tx, act mutation.Act) (v1.ApplyWorkflowResponse, error) {
			w, err := workerRef(ctx, tx, req.Worker)
			if err != nil {
				return v1.ApplyWorkflowResponse{}, err
			}
			ref, err := Resolve(ctx, tx, req.Workflow)
			if err != nil {
				return v1.ApplyWorkflowResponse{}, err
			}
			t, err := s.target(ctx, tx, w, req.Harness, ref, req.Components, v1.ApplyModeApply, req.Force, req.DryRun)
			if err != nil {
				return v1.ApplyWorkflowResponse{}, err
			}
			if req.ExpectedPreview != "" {
				if _, err := parseDigest(req.ExpectedPreview); err != nil {
					return v1.ApplyWorkflowResponse{}, domain.NewError(domain.CodeInvalidRequest, "expected_preview must be the sha256:<hex> digest a dry run printed", nil)
				}
			}
			t.preview = req.ExpectedPreview
			return s.order(ctx, tx, act, t)
		})
}

// Rollback orders the Worker to apply the version it held before the current
// one of the harness.
func (s *Service) Rollback(ctx context.Context, by pki.Session, req v1.RollbackWorkflowRequest) (v1.ApplyWorkflowResponse, error) {
	if err := input.First(input.Label("harness", req.Harness), input.Ref("worker", req.Worker)); err != nil {
		return v1.ApplyWorkflowResponse{}, err
	}
	return mutation.Do(ctx, s.db, s.clock, by, "workflow.rollback", req.RequestID, req,
		func(tx *store.Tx, act mutation.Act) (v1.ApplyWorkflowResponse, error) {
			w, err := workerRef(ctx, tx, req.Worker)
			if err != nil {
				return v1.ApplyWorkflowResponse{}, err
			}
			st, err := store.WorkflowStateOf(ctx, tx, w.ID, req.Harness)
			if errors.Is(err, store.ErrNotFound) || (err == nil && st.PreviousVersion == 0) {
				return v1.ApplyWorkflowResponse{}, domain.NewError(domain.CodeInvalidRequest,
					fmt.Sprintf("worker %s has no previous version of %s to roll back to", w.Name, req.Harness),
					map[string]any{"no_previous": true})
			}
			if err != nil {
				return v1.ApplyWorkflowResponse{}, err
			}
			v, err := store.WorkflowVersionByNumber(ctx, tx, st.AppliedWorkflow, st.PreviousVersion)
			if err != nil {
				return v1.ApplyWorkflowResponse{}, err
			}
			ref := domain.WorkflowRef{ID: st.AppliedWorkflow, Version: v.Version, Digest: v.Digest}
			t, err := s.target(ctx, tx, w, req.Harness, ref, req.Components, v1.ApplyModeRollback, req.Force, req.DryRun)
			if err != nil {
				return v1.ApplyWorkflowResponse{}, err
			}
			return s.order(ctx, tx, act, t)
		})
}

// Sync orders every online Worker to apply the Workflow the selector names.
func (s *Service) Sync(ctx context.Context, by pki.Session, req v1.SyncWorkflowRequest) (v1.SyncWorkflowResponse, error) {
	if err := input.First(input.Label("harness", req.Harness), input.Ref("workflow", req.Workflow)); err != nil {
		return v1.SyncWorkflowResponse{}, err
	}
	return mutation.Do(ctx, s.db, s.clock, by, "workflow.sync", req.RequestID, req,
		func(tx *store.Tx, act mutation.Act) (v1.SyncWorkflowResponse, error) {
			ref, err := Resolve(ctx, tx, req.Workflow)
			if err != nil {
				return v1.SyncWorkflowResponse{}, err
			}
			workers, err := store.Workers(ctx, tx, false)
			if err != nil {
				return v1.SyncWorkflowResponse{}, err
			}
			out := v1.SyncWorkflowResponse{Results: []v1.SyncWorkflowResult{}}
			for _, w := range workers {
				if w.State != domain.WorkerOnline {
					continue
				}
				t, err := s.target(ctx, tx, w, req.Harness, ref, req.Components, v1.ApplyModeApply, req.Force, req.DryRun)
				if err != nil {
					return v1.SyncWorkflowResponse{}, err
				}
				res, err := s.order(ctx, tx, act, t)
				if err != nil {
					return v1.SyncWorkflowResponse{}, err
				}
				out.Results = append(out.Results, v1.SyncWorkflowResult{Worker: w.Name, WorkerID: string(w.ID), Result: res})
			}
			return out, nil
		})
}

// workerRef finds the live Worker a reference points to.
func workerRef(ctx context.Context, q store.Querier, ref string) (store.Worker, error) {
	cands, err := store.WorkerCandidates(ctx, q)
	if err != nil {
		return store.Worker{}, err
	}
	id, err := domain.Resolve(ref, domain.KindWorker, "", cands)
	if err != nil {
		return store.Worker{}, err
	}
	w, err := store.WorkerByID(ctx, q, id)
	if err != nil {
		return store.Worker{}, err
	}
	if w.RemovedAt != nil {
		return store.Worker{}, notFound("worker %q not found", ref)
	}
	return w, nil
}

// target validates the selection against the manifest of the version: the
// harness must have components in it, and every name asked for must be one.
func (s *Service) target(ctx context.Context, q store.Querier, w store.Worker, harness string, ref domain.WorkflowRef,
	components []string, mode string, force, dryRun bool,
) (target, error) {
	if w.State == domain.WorkerOffline {
		return target{}, domain.NewError(domain.CodeWorkerOffline,
			fmt.Sprintf("worker %s is offline: an apply is not queued for a Worker that is away", w.Name), nil)
	}
	wf, err := store.WorkflowByID(ctx, q, ref.ID)
	if err != nil {
		return target{}, err
	}
	v, err := store.WorkflowVersionByNumber(ctx, q, ref.ID, ref.Version)
	if err != nil {
		return target{}, err
	}
	var m domain.Manifest
	if err := json.Unmarshal(v.Manifest, &m); err != nil {
		return target{}, fmt.Errorf("workflows: stored manifest of %s version %d: %w", wf.Name, v.Version, err)
	}
	comps, err := selection(m, harness, components)
	if err != nil {
		return target{}, err
	}
	return target{worker: w, harness: harness, name: wf.Name, ref: ref, manifest: m, components: comps, mode: mode, force: force, dryRun: dryRun}, nil
}

// selection is the sorted, deduplicated component names to apply, nil for all
// of the harness.
func selection(m domain.Manifest, harness string, asked []string) ([]string, error) {
	have := map[string]bool{}
	for _, c := range m.Components {
		if c.Harness == harness {
			have[c.Name] = true
		}
	}
	if len(have) == 0 {
		return nil, input.Invalid("the workflow has no components for harness %q", harness)
	}
	if len(asked) == 0 {
		return nil, nil
	}
	out := slices.Clone(asked)
	slices.Sort(out)
	out = slices.Compact(out)
	for _, name := range out {
		if !have[name] {
			return nil, input.Invalid("the workflow has no component %q for harness %q", name, harness)
		}
	}
	return out, nil
}

// order records the order. It never decides a no-op from the state the
// Controller holds, which can be stale (the Worker's files change on their own):
// the Worker plans afresh, writes nothing when nothing changes and reports it
// (plan.noop), and that report refreshes the drift.
func (s *Service) order(ctx context.Context, tx *store.Tx, act mutation.Act, t target) (v1.ApplyWorkflowResponse, error) {
	superseded, err := tx.SupersedeWorkflowApplies(ctx, t.worker.ID, t.harness, t.dryRun, act.Now)
	if err != nil {
		return v1.ApplyWorkflowResponse{}, err
	}
	gen, err := store.NextApplyGeneration(ctx, tx, t.worker.ID, t.harness)
	if err != nil {
		return v1.ApplyWorkflowResponse{}, err
	}
	a := store.WorkflowApply{
		ID: s.ids.New(domain.KindOperation), WorkerID: t.worker.ID, Harness: t.harness, WorkflowID: t.ref.ID, WorkflowName: t.name,
		Version: t.ref.Version, Digest: t.ref.Digest, Components: t.components, Mode: t.mode, Force: t.force, DryRun: t.dryRun,
		ExpectedPreview: t.preview, Generation: gen, State: store.ApplyPending, CreatedAt: act.Now, UpdatedAt: act.Now,
	}
	if err := tx.InsertWorkflowApply(ctx, a); err != nil {
		return v1.ApplyWorkflowResponse{}, err
	}
	data := map[string]any{
		"apply": string(a.ID), "generation": gen, "harness": t.harness, "workflow": t.name, "version": a.Version, "digest": a.Digest,
		"mode": t.mode, "force": t.force, "dry_run": t.dryRun, "components": t.components,
	}
	if superseded != nil {
		data["superseded"] = string(superseded.ID)
	}
	ev := act.Event(domain.EventWorkflowApplyRequested, fmt.Sprintf("%s %s version %d on %s (%s)", t.mode, t.name, a.Version, t.worker.Name, t.harness))
	ev.WorkerID, ev.WorkflowID, ev.Data = t.worker.ID, t.ref.ID, data
	if _, _, err := tx.AppendEvent(ctx, ev); err != nil {
		return v1.ApplyWorkflowResponse{}, err
	}
	view := applyView(a, t.worker.Name)
	if err := tx.AppendAudit(ctx, store.AuditEntry{
		Time: act.Now, ActorKind: act.Actor.Kind, ActorID: act.Actor.ID, Action: "workflow." + t.mode,
		ResourceKind: "worker", ResourceID: string(t.worker.ID), After: view, Result: "ok",
	}); err != nil {
		return v1.ApplyWorkflowResponse{}, err
	}
	return v1.ApplyWorkflowResponse{Apply: &view}, nil
}

// applyResult is what the result column holds: the error and the opaque detail
// a Worker reported.
type applyResult struct {
	Error  string          `json:"error,omitempty"`
	Detail json.RawMessage `json:"detail,omitempty"`
}

func applyView(a store.WorkflowApply, worker string) v1.WorkflowApply {
	out := v1.WorkflowApply{
		ID: string(a.ID), WorkerID: string(a.WorkerID), Worker: worker, Harness: a.Harness, Workflow: a.WorkflowName,
		Version: a.Version, Digest: a.Digest, Components: a.Components, Mode: a.Mode, Force: a.Force, DryRun: a.DryRun,
		Generation: a.Generation, State: a.State, CreatedAt: a.CreatedAt, UpdatedAt: a.UpdatedAt,
	}
	if len(a.Result) > 0 {
		var r applyResult
		if json.Unmarshal(a.Result, &r) == nil {
			out.Error, out.Result = r.Error, r.Detail
		}
	}
	return out
}

// GetApply returns an order by ID.
func (s *Service) GetApply(ctx context.Context, id string) (v1.WorkflowApply, error) {
	var out v1.WorkflowApply
	err := s.db.Read(ctx, func(q store.Querier) error {
		a, err := store.WorkflowApplyByID(ctx, q, domain.ID(id))
		if errors.Is(err, store.ErrNotFound) {
			return notFound("workflow apply %q not found", id)
		}
		if err != nil {
			return err
		}
		w, err := store.WorkerByID(ctx, q, a.WorkerID)
		if err != nil {
			return err
		}
		out = applyView(a, w.Name)
		return nil
	})
	return out, err
}

// Status lists what the Controller knows each Worker holds of each harness,
// with the latest order on it. worker and harness narrow it when set.
func (s *Service) Status(ctx context.Context, worker, harness string) ([]v1.WorkflowState, error) {
	out := []v1.WorkflowState{}
	err := s.db.Read(ctx, func(q store.Querier) error {
		var wid domain.ID
		if worker != "" {
			w, err := workerRef(ctx, q, worker)
			if err != nil {
				return err
			}
			wid = w.ID
		}
		states, err := store.ListWorkflowStates(ctx, q, wid)
		if err != nil {
			return err
		}
		latest, err := store.LatestWorkflowAppliesOf(ctx, q, wid)
		if err != nil {
			return err
		}
		workers, err := store.Workers(ctx, q, true)
		if err != nil {
			return err
		}
		names := map[domain.ID]string{}
		for _, w := range workers {
			names[w.ID] = w.Name
		}
		type key struct {
			worker  domain.ID
			harness string
		}
		rows := map[key]*v1.WorkflowState{}
		var order []key
		row := func(k key) *v1.WorkflowState {
			if r, ok := rows[k]; ok {
				return r
			}
			r := &v1.WorkflowState{WorkerID: string(k.worker), Worker: names[k.worker], Harness: k.harness, Drift: []v1.DriftedComponent{}}
			rows[k] = r
			order = append(order, k)
			return r
		}
		wfNames := map[domain.ID]string{}
		for _, st := range states {
			r := row(key{st.WorkerID, st.Harness})
			r.Partial, r.Skipped = len(st.Skipped) > 0, st.Skipped
			r.AppliedVersion, r.AppliedDigest, r.Components, r.PreviousVersion, r.UpdatedAt = st.AppliedVersion, st.AppliedDigest, st.Components, st.PreviousVersion, st.UpdatedAt
			if st.AppliedWorkflow != "" {
				wf, ok := wfNames[st.AppliedWorkflow]
				if !ok {
					w, err := store.WorkflowByID(ctx, q, st.AppliedWorkflow)
					if err != nil {
						return err
					}
					wf = w.Name
					wfNames[st.AppliedWorkflow] = wf
				}
				r.Workflow = wf
			}
			if err := json.Unmarshal(st.Drift, &r.Drift); err != nil {
				return fmt.Errorf("workflows: drift of %s/%s: %w", st.WorkerID, st.Harness, err)
			}
		}
		for _, a := range latest {
			r := row(key{a.WorkerID, a.Harness})
			v := applyView(a, names[a.WorkerID])
			r.Latest = &v
			if r.UpdatedAt.Before(a.UpdatedAt) {
				r.UpdatedAt = a.UpdatedAt
			}
		}
		for _, k := range order {
			if harness == "" || k.harness == harness {
				out = append(out, *rows[k])
			}
		}
		return nil
	})
	return out, err
}
