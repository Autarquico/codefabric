package executor

import (
	"context"
	"time"

	v1 "github.com/Autarquico/codefabric/internal/api/v1"
	"github.com/Autarquico/codefabric/internal/worker/execwrap"
	"github.com/Autarquico/codefabric/internal/worker/state"
)

// startHold is how long `started` waits for the harness to say its model.
const startHold = 10 * time.Second

// A harness with a structured stream says its model on its init, after the
// session exists. Its `started` is held, in the cursor of its stream, until the
// consumer sees session_started, the harness is over (finalize), or startHold
// passes: then it is reported once, with the model if there is one. The Run is
// running for the Worker from the session on; only the report waits.

// holdsStart says whether the `started` of a Run waits for the init of its harness.
func (e *Executor) holdsStart(k state.Key) bool {
	var l execwrap.Launch
	ok, err := execwrap.Read(e.runDir(k), execwrap.LaunchFile, &l)
	return err == nil && ok && l.StructuredStdout
}

// recordStart marks a Run running, in the transaction of the caller, and
// reports that the harness started, or holds the report.
func (e *Executor) recordStart(ctx context.Context, tx *state.Tx, k state.Key, handle string, at time.Time) error {
	if err := state.SetRunning(ctx, tx, k, handle, at); err != nil {
		return err
	}
	if !e.holdsStart(k) {
		return e.reportStarted(ctx, tx, k, at, "")
	}
	cur, err := state.EventCursorOf(ctx, tx, k)
	if err != nil {
		return err
	}
	cur.StartHeldAt = e.clock.Now()
	return state.SaveEventCursor(ctx, tx, cur)
}

func (e *Executor) reportStarted(ctx context.Context, tx *state.Tx, k state.Key, at time.Time, model string) error {
	digest, err := e.materializedDigest(ctx, tx, k)
	if err != nil {
		return err
	}
	return e.report(ctx, tx, k, v1.ReportStarted, at, func(en *v1.RunReportEntry) {
		en.Handle, en.Model, en.ConfigDigest = e.runtimeHandle(k), model, digest
	})
}

// materializedDigest is the digest of the configuration materialised for a Run
// that froze a Workflow, as the Worker stored it when it materialised it, before
// the harness started (so it survives a restart of the daemon and is not
// something the Run can rewrite); "" for a Run without a Workflow. A digest that
// was never stored does not stop the Run from starting: the report goes without
// it. A storage error does: it is returned, so that the transaction of the
// report is undone and the report is tried again with the digest.
func (e *Executor) materializedDigest(ctx context.Context, q state.Querier, k state.Key) (string, error) {
	r, err := state.RunBy(ctx, q, k)
	if err != nil {
		return "", err
	}
	if parseSpec(r.Spec).Workflow == "" {
		return "", nil
	}
	digest, ok, err := state.ConfigDigestOf(ctx, q, k)
	if err != nil {
		return "", err
	}
	if !ok {
		e.log.WarnContext(ctx, "the configuration of a Run was materialised but its digest was not stored; it starts without a digest", "run", k.RunID)
	}
	return digest, nil
}

// releaseStart reports a held `started` and stores the cursor, which says it is
// not held anymore, in the transaction of the caller.
func (e *Executor) releaseStart(ctx context.Context, tx *state.Tx, k state.Key, cur state.EventCursor) error {
	at := e.clock.Now()
	if r, err := state.RunBy(ctx, tx, k); err == nil && r.StartedAt != nil {
		at = *r.StartedAt
	}
	cur.StartHeldAt = time.Time{}
	if err := state.SaveEventCursor(ctx, tx, cur); err != nil {
		return err
	}
	return e.reportStarted(ctx, tx, k, at, cur.Model)
}

// releaseStartIfDue reports a `started` that has been held for startHold.
func (e *Executor) releaseStartIfDue(ctx context.Context, r state.Run) {
	cur, err := state.EventCursorOf(ctx, e.o.DB.Reader(), r.Key)
	if err != nil || cur.StartHeldAt.IsZero() || e.clock.Now().Sub(cur.StartHeldAt) < startHold {
		return
	}
	if err := e.o.DB.WithTx(ctx, func(tx *state.Tx) error { return e.releaseStart(ctx, tx, r.Key, cur) }); err != nil {
		e.log.WarnContext(ctx, "reporting a held start failed; it will be tried again", "run", r.RunID, "err", err)
		return
	}
	e.o.Journal.Notify()
}
