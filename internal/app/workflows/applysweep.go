package workflows

import (
	"context"
	"encoding/json"
	"time"

	v1 "github.com/Autarquico/codefabric/internal/api/v1"
	"github.com/Autarquico/codefabric/internal/app/mutation"
	"github.com/Autarquico/codefabric/internal/controller/store"
)

// FailStuckApplies fails, in tx, the real orders alive that have not moved since
// before (store.FailStuckWorkflowApplies) and closes each one the way a failed
// report does: the event and the audit entry of the actor of act, in the same
// transaction as the state change and the release of the lock, so that none of
// them remains without the others. It returns how many it failed.
func FailStuckApplies(ctx context.Context, tx *store.Tx, act mutation.Act, before time.Time, reason string) (int, error) {
	result, err := json.Marshal(applyResult{Error: reason})
	if err != nil {
		return 0, err
	}
	ids, err := tx.FailStuckWorkflowApplies(ctx, before, result, act.Now)
	if err != nil {
		return 0, err
	}
	for _, id := range ids {
		a, err := store.WorkflowApplyByID(ctx, tx, id)
		if err != nil {
			return 0, err
		}
		w, err := store.WorkerByID(ctx, tx, a.WorkerID)
		if err != nil {
			return 0, err
		}
		if err := reportEvents(ctx, tx, act, a, w, v1.ReportWorkflowApplyRequest{Outcome: "failed", Error: reason}); err != nil {
			return 0, err
		}
		err = tx.AppendAudit(ctx, store.AuditEntry{
			Time: act.Now, ActorKind: act.Actor.Kind, ActorID: act.Actor.ID, Action: "workflow.apply." + store.ApplyFailed,
			ResourceKind: "worker", ResourceID: string(w.ID), After: applyView(a, w.Name), Result: "ok",
		})
		if err != nil {
			return 0, err
		}
	}
	return len(ids), nil
}
