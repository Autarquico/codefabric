package workflows

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"time"

	v1 "github.com/Autarquico/codefabric/internal/api/v1"
	"github.com/Autarquico/codefabric/internal/app/input"
	"github.com/Autarquico/codefabric/internal/app/mutation"
	"github.com/Autarquico/codefabric/internal/controller/pki"
	"github.com/Autarquico/codefabric/internal/controller/store"
	"github.com/Autarquico/codefabric/internal/domain"
)

const (
	maxReportResultBytes = 256 << 10
	maxReportErrorBytes  = 4 << 10
	maxReportDrift       = 256
)

// A Worker reads the Workflow of an order addressed to it, and only while that
// order is alive at the generation it names: what the manifest of the version
// says about the harness and components of the order, and the blobs of those.

// ApplyManifest is the version of the order, with the manifest narrowed to the
// harness and components the order is for.
func (s *Service) ApplyManifest(ctx context.Context, by pki.Session, id string, generation int) (Version, error) {
	var out Version
	err := s.db.Read(ctx, func(q store.Querier) error {
		a, m, v, err := s.liveOrder(ctx, q, by, id, generation)
		if err != nil {
			return err
		}
		out = view(a.WorkflowName, v)
		out.Manifest = &m
		return nil
	})
	return out, err
}

// OpenApplyBlob is the content of a blob of the narrowed manifest of the
// order, and its size. A blob outside it is not found.
func (s *Service) OpenApplyBlob(ctx context.Context, by pki.Session, id string, generation int, digest string) (io.ReadCloser, int64, error) {
	if s.blobs == nil {
		return nil, 0, domain.NewError(domain.CodeInternal, "the Controller has no blob store", nil)
	}
	d, err := parseDigest(digest)
	if err != nil {
		return nil, 0, err
	}
	var meta store.Blob
	err = s.db.Read(ctx, func(q store.Querier) error {
		_, m, _, err := s.liveOrder(ctx, q, by, id, generation)
		if err != nil {
			return err
		}
		if !names(m, digest) {
			return notFound("blob %s is not in the selection of this order", digest)
		}
		meta, err = store.BlobMeta(ctx, q, digest)
		if errors.Is(err, store.ErrNotFound) {
			return notFound("blob %s not found", digest)
		}
		return err
	})
	if err != nil {
		return nil, 0, err
	}
	rc, err := s.blobs.Open(d)
	return rc, meta.Size, err
}

// callerWorker is the Worker behind the session.
func (s *Service) callerWorker(ctx context.Context, q store.Querier, by pki.Session) (store.Worker, error) {
	if _, err := pki.RequireCaller(ctx, q, by, s.clock.Now()); err != nil {
		return store.Worker{}, err
	}
	w, err := store.WorkerByIdentity(ctx, q, by.Principal.ID)
	if errors.Is(err, store.ErrNotFound) {
		return store.Worker{}, domain.NewError(domain.CodeUnauthenticated, "this identity is not a Worker", nil)
	}
	return w, err
}

// ownOrder finds the order and checks it is addressed to the Worker.
func ownOrder(ctx context.Context, q store.Querier, w store.Worker, id string) (store.WorkflowApply, error) {
	if kind, _, err := domain.ParseID(id); err != nil || kind != domain.KindOperation {
		return store.WorkflowApply{}, notFound("workflow apply not found")
	}
	a, err := store.WorkflowApplyByID(ctx, q, domain.ID(id))
	if errors.Is(err, store.ErrNotFound) {
		return store.WorkflowApply{}, notFound("workflow apply not found")
	}
	if err != nil {
		return store.WorkflowApply{}, err
	}
	if a.WorkerID != w.ID {
		return store.WorkflowApply{}, domain.NewError(domain.CodePolicyDenied, "this order is not addressed to this Worker", nil)
	}
	return a, nil
}

// liveOrder authorises a download: the order is the Worker's, alive, and at the
// generation named. It returns the manifest narrowed to the order.
func (s *Service) liveOrder(ctx context.Context, q store.Querier, by pki.Session, id string, generation int) (store.WorkflowApply, domain.Manifest, store.WorkflowVersion, error) {
	w, err := s.callerWorker(ctx, q, by)
	if err != nil {
		return store.WorkflowApply{}, domain.Manifest{}, store.WorkflowVersion{}, err
	}
	a, err := ownOrder(ctx, q, w, id)
	if err != nil {
		return store.WorkflowApply{}, domain.Manifest{}, store.WorkflowVersion{}, err
	}
	if !a.Live() || a.Generation != generation {
		return store.WorkflowApply{}, domain.Manifest{}, store.WorkflowVersion{}, domain.NewError(domain.CodeStaleLease,
			"this is not the live generation of the order", map[string]any{"generation": a.Generation, "state": a.State})
	}
	v, err := store.WorkflowVersionByNumber(ctx, q, a.WorkflowID, a.Version)
	if err != nil {
		return store.WorkflowApply{}, domain.Manifest{}, store.WorkflowVersion{}, err
	}
	var m domain.Manifest
	if err := json.Unmarshal(v.Manifest, &m); err != nil {
		return store.WorkflowApply{}, domain.Manifest{}, store.WorkflowVersion{}, fmt.Errorf("workflows: stored manifest of %s version %d: %w", a.WorkflowName, v.Version, err)
	}
	return a, narrow(m, a.Harness, a.Components), v, nil
}

// narrow keeps the components of the harness, and of those only the names
// asked for when there are any.
func narrow(m domain.Manifest, harness string, components []string) domain.Manifest {
	out := domain.Manifest{SchemaVersion: m.SchemaVersion, Components: []domain.ManifestComponent{}}
	for _, c := range m.Components {
		if c.Harness != harness {
			continue
		}
		if components != nil && !contains(components, c.Name) {
			continue
		}
		out.Components = append(out.Components, c)
	}
	return out
}

func contains(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}

// ReportResult takes the outcome of an order from the Worker it was addressed
// to. A report of an order that is no longer alive at that generation (it was
// superseded, or already reported) is ignored and answered as not accepted.
// An applied order that is not a dry run records what the Worker now holds,
// with the previous version for rollback; a dry run records nothing: its drift is measured against the version it
// previews, not the one applied.
func (s *Service) ReportResult(ctx context.Context, by pki.Session, id string, req v1.ReportWorkflowApplyRequest) (v1.ReportWorkflowApplyResponse, error) {
	if err := validReport(req); err != nil {
		return v1.ReportWorkflowApplyResponse{}, err
	}
	return mutation.Do(ctx, s.db, s.clock, by, "workflow.report", "", struct {
		ID  string
		Req v1.ReportWorkflowApplyRequest
	}{id, req}, func(tx *store.Tx, act mutation.Act) (v1.ReportWorkflowApplyResponse, error) {
		w, err := s.callerWorker(ctx, tx, by)
		if err != nil {
			return v1.ReportWorkflowApplyResponse{}, err
		}
		a, err := ownOrder(ctx, tx, w, id)
		if err != nil {
			return v1.ReportWorkflowApplyResponse{}, err
		}
		if !a.Live() || a.Generation != req.Generation {
			return v1.ReportWorkflowApplyResponse{Accepted: false}, nil
		}
		detail, err := json.Marshal(applyResult{Error: req.Error, Detail: req.Result})
		if err != nil {
			return v1.ReportWorkflowApplyResponse{}, err
		}
		state := store.ApplyApplied
		if req.Outcome == "failed" {
			state = store.ApplyFailed
		}
		if _, err := tx.FinishWorkflowApply(ctx, a.ID, a.Generation, state, detail, act.Now); err != nil {
			return v1.ReportWorkflowApplyResponse{}, err
		}
		a.State = state
		drift, err := json.Marshal(nonNilDrift(req.Drift))
		if err != nil {
			return v1.ReportWorkflowApplyResponse{}, err
		}
		if state == store.ApplyApplied && !a.DryRun {
			if err := recordState(ctx, tx, a, drift, len(req.Skipped) > 0, act.Now); err != nil {
				return v1.ReportWorkflowApplyResponse{}, err
			}
		}
		if err := reportEvents(ctx, tx, act, a, w, req); err != nil {
			return v1.ReportWorkflowApplyResponse{}, err
		}
		return v1.ReportWorkflowApplyResponse{Accepted: true}, tx.AppendAudit(ctx, store.AuditEntry{
			Time: act.Now, ActorKind: act.Actor.Kind, ActorID: act.Actor.ID, Action: "workflow.apply." + state,
			ResourceKind: "worker", ResourceID: string(w.ID), After: applyView(a, w.Name), Result: "ok",
		})
	})
}

func nonNilDrift(d []v1.DriftedComponent) []v1.DriftedComponent {
	if d == nil {
		return []v1.DriftedComponent{}
	}
	return d
}

func validReport(req v1.ReportWorkflowApplyRequest) error {
	checks := []error{}
	if req.Generation < 1 {
		checks = append(checks, input.Invalid("generation must be the generation of the order"))
	}
	if req.Outcome != "applied" && req.Outcome != "failed" {
		checks = append(checks, input.Invalid("outcome must be applied or failed"))
	}
	if len(req.Result) > maxReportResultBytes || (len(req.Result) > 0 && !json.Valid(req.Result)) {
		checks = append(checks, input.Invalid("result must be JSON of at most %d bytes", maxReportResultBytes))
	}
	if len(req.Error) > maxReportErrorBytes {
		checks = append(checks, input.Invalid("error is at most %d bytes", maxReportErrorBytes))
	}
	if len(req.Skipped) > maxReportDrift {
		checks = append(checks, input.Invalid("skipped lists at most %d components", maxReportDrift))
	}
	if len(req.Drift) > maxReportDrift {
		checks = append(checks, input.Invalid("drift lists at most %d components", maxReportDrift))
	}
	return input.First(checks...)
}

// recordState writes what the Worker holds after an applied order that was not
// a dry run.
func recordState(ctx context.Context, tx *store.Tx, a store.WorkflowApply, drift []byte, partial bool, now time.Time) error {
	st, err := store.WorkflowStateOf(ctx, tx, a.WorkerID, a.Harness)
	if errors.Is(err, store.ErrNotFound) {
		st = store.WorkflowState{WorkerID: a.WorkerID, Harness: a.Harness}
	} else if err != nil {
		return err
	}
	st.PreviousVersion = previousAfter(st, a)
	st.AppliedWorkflow, st.AppliedVersion, st.AppliedDigest, st.Components = a.WorkflowID, a.Version, a.Digest, a.Components
	st.Drift, st.Partial, st.UpdatedAt = drift, partial, now
	return tx.PutWorkflowState(ctx, st)
}

// previousAfter is the version to roll back to once a is applied over st: the
// one held before, if it is of the same Workflow and not the same version.
func previousAfter(st store.WorkflowState, a store.WorkflowApply) int {
	switch {
	case st.AppliedVersion == 0 || st.AppliedWorkflow != a.WorkflowID:
		return 0
	case st.AppliedVersion == a.Version:
		return st.PreviousVersion
	default:
		return st.AppliedVersion
	}
}

func reportEvents(ctx context.Context, tx *store.Tx, act mutation.Act, a store.WorkflowApply, w store.Worker, req v1.ReportWorkflowApplyRequest) error {
	ev := act.Event(domain.EventWorkflowApplied, fmt.Sprintf("%s %s version %d on %s (%s): %s", a.Mode, a.WorkflowName, a.Version, w.Name, a.Harness, req.Outcome))
	ev.WorkerID, ev.WorkflowID = w.ID, a.WorkflowID
	ev.Data = map[string]any{
		"apply": string(a.ID), "generation": a.Generation, "harness": a.Harness, "workflow": a.WorkflowName, "version": a.Version,
		"digest": a.Digest, "mode": a.Mode, "dry_run": a.DryRun, "outcome": req.Outcome, "drift": len(req.Drift),
	}
	if req.Outcome == "failed" {
		ev.Level = domain.LevelError
		ev.Data["error"] = req.Error
	}
	if _, _, err := tx.AppendEvent(ctx, ev); err != nil {
		return err
	}
	if len(req.Drift) == 0 {
		return nil
	}
	drift := act.Event(domain.EventWorkflowDriftDetected, fmt.Sprintf("%d component(s) of %s on %s differ from what was applied", len(req.Drift), a.Harness, w.Name))
	drift.Level, drift.WorkerID, drift.WorkflowID = domain.LevelWarn, w.ID, a.WorkflowID
	drift.Data = map[string]any{"apply": string(a.ID), "harness": a.Harness, "components": req.Drift}
	_, _, err := tx.AppendEvent(ctx, drift)
	return err
}
