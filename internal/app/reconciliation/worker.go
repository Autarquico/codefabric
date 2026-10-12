package reconciliation

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"unicode"

	"github.com/oklog/ulid/v2"

	v1 "github.com/Autarquico/codefabric/internal/api/v1"
	"github.com/Autarquico/codefabric/internal/app/input"
	"github.com/Autarquico/codefabric/internal/app/mutation"
	"github.com/Autarquico/codefabric/internal/app/runs"
	"github.com/Autarquico/codefabric/internal/controller/pki"
	"github.com/Autarquico/codefabric/internal/controller/store"
	"github.com/Autarquico/codefabric/internal/domain"
)

// Page sizes of the lists of an answer. An assignment carries the frozen spec
// of its Run (a prompt of up to 32 KiB), so few go in a page.
const (
	leasesPerPage = 16
	stopsPerPage  = 256
	pubsPerPage   = 256
)

// errRestartRound answers a fragment or a page that does not follow what the
// Controller holds of the round: the Worker starts the round again.
func errRestartRound() error {
	return domain.NewError(domain.CodeInvalidRequest, "this reconciliation round is unknown or out of order: start it again", nil)
}

// Reconcile is POST /v1/worker/reconcile (§27.3, §10.4 point 8): the Worker
// that the caller's identity stands for says what it holds and the Controller
// answers, for each, what it is worth, in one transaction. Every entry is
// resolved against the leases of this Worker; another Worker's or an unknown
// one gets the same verdict, unknown, and nothing else. The answer to the last
// fragment of a round, in pages, lists the accepted leases of the Worker with
// an open reservation that the round did not name, the stops it owes and the
// publications that wait for it. A round is complete when it has no lease left
// to account for and every page was served: only then is the Worker
// reconciled, and the scheduler gives it work again (§27.1).
func (s *Service) Reconcile(ctx context.Context, by pki.Session, req v1.ReconcileRequest) (v1.ReconcileResponse, error) {
	if err := validateReconcile(req); err != nil {
		return v1.ReconcileResponse{}, err
	}
	var (
		worker    domain.ID
		named     []leaseKey
		absent    int
		completed bool
	)
	out, err := mutation.Do(ctx, s.db, s.clock, by, "worker.reconcile", "", req,
		func(tx *store.Tx, act mutation.Act) (v1.ReconcileResponse, error) {
			named, absent, completed = nil, 0, false
			w, err := store.WorkerByIdentity(ctx, tx, by.Principal.ID)
			if errors.Is(err, store.ErrNotFound) {
				return v1.ReconcileResponse{}, domain.NewError(domain.CodeNotFound, "this identity is not a Worker", nil)
			}
			if err != nil {
				return v1.ReconcileResponse{}, err
			}
			worker = w.ID
			rec := reconciler{s: s, tx: tx, act: act, w: w}
			resp := v1.ReconcileResponse{Verdicts: []v1.ReconcileVerdict{}}
			if resp.Ack.CommandsAckedThrough, err = rec.ack(ctx, req.CommandsCursor); err != nil {
				return v1.ReconcileResponse{}, err
			}
			f := req.Fragment
			var seen map[leaseKey]bool
			if req.Cursor != "" {
				var ok bool
				if seen, absent, ok = s.rounds.complete(w.ID, req.Epoch, f.RoundID); !ok {
					return v1.ReconcileResponse{}, errRestartRound()
				}
			} else {
				var ok bool
				if seen, ok = s.rounds.peek(w.ID, req.Epoch, f.RoundID, f.Index, f.Count); !ok {
					return v1.ReconcileResponse{}, errRestartRound()
				}
				if f.Index == 0 {
					if err := tx.SetReconcileStarted(ctx, w.ID, act.Now); err != nil {
						return v1.ReconcileResponse{}, err
					}
				}
				if named, err = rec.entries(ctx, req, &resp); err != nil {
					return v1.ReconcileResponse{}, err
				}
				for _, k := range named {
					seen[k] = true
				}
				if f.Index < f.Count-1 {
					return resp, nil // only the last fragment is answered with the lists
				}
			}
			n, err := rec.page(ctx, seen, req.Cursor, &resp)
			if err != nil {
				return v1.ReconcileResponse{}, err
			}
			absent += n
			if resp.Next != "" || absent > 0 {
				return resp, nil
			}
			if err := rec.reconciled(ctx, req, &resp); err != nil {
				return v1.ReconcileResponse{}, err
			}
			completed = true
			return resp, nil
		})
	if err != nil {
		return out, err
	}
	// What the round remembers changes only once the transaction is in.
	f := req.Fragment
	if req.Cursor == "" {
		s.rounds.advance(worker, req.Epoch, f.RoundID, f.Index, f.Count, named)
	}
	if f.Index == f.Count-1 || req.Cursor != "" {
		s.rounds.served(worker, f.RoundID, absent)
	}
	if completed {
		s.rounds.forget(worker, f.RoundID)
	}
	return out, nil
}

// reconciler decides one request in its transaction.
type reconciler struct {
	s   *Service
	tx  *store.Tx
	act mutation.Act
	w   store.Worker

	verdicts map[string]int
}

func (r *reconciler) ack(ctx context.Context, cursor uint64) (uint64, error) {
	return r.tx.AckCommandsThrough(ctx, r.w.ID, min(cursor, r.w.LastCommandSeq), r.act.Now)
}

func (r *reconciler) count(verdict string) {
	if r.verdicts == nil {
		r.verdicts = map[string]int{}
	}
	r.verdicts[verdict]++
}

// entries decides the runs, outcomes and sessions of a fragment, in that order,
// and returns the leases they name.
func (r *reconciler) entries(ctx context.Context, req v1.ReconcileRequest, resp *v1.ReconcileResponse) ([]leaseKey, error) {
	var named []leaseKey
	for _, e := range req.Runs {
		v, err := r.run(ctx, e)
		if err != nil {
			return nil, err
		}
		if v.Verdict != v1.ReconcileUnknown {
			named = append(named, leaseKey{domain.ID(e.RunID), e.LeaseEpoch})
		}
		r.count(v.Verdict)
		resp.Verdicts = append(resp.Verdicts, v)
	}
	for _, o := range req.Unrecorded {
		v, key, err := r.outcome(ctx, o)
		if err != nil {
			return nil, err
		}
		if key.run != "" {
			named = append(named, key)
		}
		r.count(v.Verdict)
		resp.Verdicts = append(resp.Verdicts, v)
	}
	for _, name := range req.Sessions {
		v, err := r.session(ctx, name)
		if err != nil {
			return nil, err
		}
		r.count(v.Verdict)
		resp.Verdicts = append(resp.Verdicts, v)
	}
	return named, nil
}

// held is a lease of this Worker with its Run and Task, as the transaction reads them.
type held struct {
	lease store.Lease
	run   store.Run
	task  store.Task
}

// live says whether the lease is the one the Run is under now, accepted.
func (h held) live() bool {
	return h.lease.State == domain.LeaseAccepted && h.lease.Epoch == h.run.LeaseEpoch && h.run.LeaseID == h.lease.ID
}

// lookup finds the lease (run, epoch) of this Worker. found is false for a
// lease that does not exist, another Worker's, or one whose ID is not the one
// given (when one is): the three are the same answer.
func (r *reconciler) lookup(ctx context.Context, run string, epoch uint64, leaseID string) (held, bool, error) {
	if kind, _, err := domain.ParseID(run); err != nil || kind != domain.KindRun {
		return held{}, false, nil
	}
	l, err := store.LeaseByRunEpoch(ctx, r.tx, domain.ID(run), epoch)
	if errors.Is(err, store.ErrNotFound) || (err == nil && (l.WorkerID != r.w.ID || (leaseID != "" && string(l.ID) != leaseID))) {
		return held{}, false, nil
	}
	if err != nil {
		return held{}, false, err
	}
	h, err := r.heldOf(ctx, l)
	return h, err == nil, err
}

// heldOf reads the Run and the Task of a lease of this Worker.
func (r *reconciler) heldOf(ctx context.Context, l store.Lease) (held, error) {
	h := held{lease: l}
	var err error
	if h.run, err = store.RunByID(ctx, r.tx, l.RunID); err != nil {
		return held{}, err
	}
	if h.task, err = store.TaskByID(ctx, r.tx, h.run.TaskID); err != nil {
		return held{}, err
	}
	return h, nil
}

// run decides one Run the Worker holds a record of.
func (r *reconciler) run(ctx context.Context, e v1.ReconcileRun) (v1.ReconcileVerdict, error) {
	v := v1.ReconcileVerdict{RunID: e.RunID, LeaseEpoch: e.LeaseEpoch, Verdict: v1.ReconcileUnknown}
	h, found, err := r.lookup(ctx, e.RunID, e.LeaseEpoch, e.LeaseID)
	if err != nil || !found {
		return v, err
	}
	if !h.live() {
		// A lease that is not the Run's now: what runs under it is an orphan.
		if h.lease.Epoch < h.run.LeaseEpoch && (e.Alive || e.Started) {
			if _, err := runs.OrphanIn(ctx, r.tx, r.act, h.run, h.task, h.lease); err != nil {
				return v, err
			}
		}
		v.Verdict = v1.ReconcileStale
		return v, nil
	}
	fenced := e.FencedCode != ""
	if !h.run.State.IsTerminal() {
		v.Verdict, v.Resend = v1.ReconcileContinue, fenced
		return v, nil
	}
	settled := !e.Alive && v1.FinalPhase(e.Phase) && (e.PendingReports == 0 || fenced)
	if err := r.terminal(ctx, &h, e, settled); err != nil {
		return v, err
	}
	switch {
	case e.Alive && runs.ControllerEnded(h.run):
		if _, err := runs.OrderStopIn(ctx, r.tx, r.act, h.run, h.task, h.lease, runs.StopReasonOf(h.run)); err != nil {
			return v, err
		}
		if h.run.State == domain.RunLost {
			if _, err := runs.OrphanIn(ctx, r.tx, r.act, h.run, h.task, h.lease); err != nil {
				return v, err
			}
		}
		v.Verdict = v1.ReconcileStop
	case e.Alive:
		v.Verdict = v1.ReconcileStop
	default:
		v.Verdict = v1.ReconcileContinue
	}
	if fenced {
		// The fenced reports are worth sending again while the Controller has
		// something to learn from them; once the runtime is stopped and the
		// publication is not waiting, they never will be taken.
		done := h.run.Runtime == domain.RuntimeStopped && h.run.Publication != domain.PublicationPending
		v.Resend, v.Drop = !done, done
	}
	if e.Publication != nil && e.Publication.State == "pushed" {
		seq, err := store.SettledPublicationSeq(ctx, r.tx, r.w.ID, h.run.ID)
		if err != nil {
			return v, err
		}
		v.Publication = &v1.ReconcilePublicationView{
			RunID: e.RunID, LeaseEpoch: e.LeaseEpoch, Publication: h.run.Publication, ResultCommit: h.run.ResultCommit, SettledSeq: seq,
		}
	}
	return v, nil
}

// terminal brings a Run that is over for the Controller up to what the Worker
// knows of it (B5): whether its harness ran, and, when the Worker has nothing
// left of it (settled: over on its side, nothing alive and no report on its
// way), that its runtime is stopped and that a result that was never sealed
// will not be. Then its reservation is released if §10.5 allows it.
func (r *reconciler) terminal(ctx context.Context, h *held, e v1.ReconcileRun, settled bool) error {
	if e.HarnessRan != nil && h.run.HarnessRan == nil {
		if err := r.tx.SetHarnessRan(ctx, h.run, *e.HarnessRan); err != nil {
			return err
		}
		h.run.HarnessRan = e.HarnessRan
	}
	if err := runs.MarkExecutedIn(ctx, r.tx, &h.run); err != nil {
		return err
	}
	if settled && h.run.Runtime != domain.RuntimeStopped {
		if err := r.stopRuntime(ctx, h, "the Worker holds no process of the Run"); err != nil {
			return err
		}
	}
	if settled && e.Publication == nil && h.run.Publication == domain.PublicationPending && h.run.ResultCommit == "" {
		if err := r.failPublication(ctx, h, "the Worker has no result of the Run"); err != nil {
			return err
		}
	}
	return runs.ReleaseIfAllowedIn(ctx, r.tx, r.act, h.run, h.lease)
}

func (r *reconciler) event(h *held, t domain.EventType, message string, data map[string]any) domain.Event {
	ev := r.act.Event(t, message)
	ev.ProjectID, ev.TaskID, ev.RunID, ev.WorkerID = h.task.ProjectID, h.task.ID, h.run.ID, r.w.ID
	ev.Data = map[string]any{"short_id": domain.ShortID{Kind: domain.KindRun, N: h.run.Short}.String(), "lease_epoch": h.lease.Epoch, "source": "reconcile"}
	for k, v := range data {
		ev.Data[k] = v
	}
	return ev
}

// stopRuntime records, on the word of the Worker's reconciliation, that no
// process of the Run is left.
func (r *reconciler) stopRuntime(ctx context.Context, h *held, why string) error {
	if err := r.tx.StopRuntime(ctx, h.run); err != nil {
		return err
	}
	never := h.run.Runtime == domain.RuntimeNone
	h.run.Runtime = domain.RuntimeStopped
	_, _, err := r.tx.AppendEvent(ctx, r.event(h, domain.EventRunStopped, "run stopped: "+why, map[string]any{"never_started": never}))
	return err
}

// failPublication gives up a result the Worker cannot have.
func (r *reconciler) failPublication(ctx context.Context, h *held, why string) error {
	if err := r.tx.FailPublication(ctx, h.run); err != nil {
		return err
	}
	h.run.Publication = domain.PublicationFailed
	ev := r.event(h, domain.EventRunPublicationFailed, "run publication failed: "+why, map[string]any{"phase": v1.PhaseSeal})
	ev.Level = "warn"
	if _, _, err := r.tx.AppendEvent(ctx, ev); err != nil {
		return err
	}
	return r.s.rederive(ctx, r.tx, r.act, h.task.ID)
}

// outcome applies what became of a lease the Worker had no record of (§10.4
// point 8). Each is decided on the Run as it is now, so repeating it, or a
// cancel, lost or exit that came in between, is no harm: what is done already
// is not done again and writes nothing.
func (r *reconciler) outcome(ctx context.Context, o v1.ReconcileOutcome) (v1.ReconcileVerdict, leaseKey, error) {
	v := v1.ReconcileVerdict{LeaseEpoch: o.LeaseEpoch, Verdict: v1.ReconcileUnknown}
	if kind, _, err := domain.ParseID(o.LeaseID); err != nil || kind != domain.KindLease {
		return v, leaseKey{}, nil
	}
	l, err := store.LeaseByID(ctx, r.tx, domain.ID(o.LeaseID))
	if errors.Is(err, store.ErrNotFound) || (err == nil && (l.WorkerID != r.w.ID || l.Epoch != o.LeaseEpoch)) {
		return v, leaseKey{}, nil
	}
	if err != nil {
		return v, leaseKey{}, err
	}
	h, err := r.heldOf(ctx, l)
	if err != nil {
		return v, leaseKey{}, err
	}
	v.RunID = string(h.run.ID)
	key := leaseKey{h.run.ID, l.Epoch}
	if !h.live() {
		v.Verdict = v1.ReconcileStale
		return v, key, nil
	}
	v.Verdict = v1.ReconcileContinue
	switch o.Outcome {
	case v1.OutcomeAssignmentUnknown:
		return v, key, r.assignmentUnknown(ctx, &h)
	case v1.OutcomeWorktreeLost:
		if !h.run.State.IsTerminal() {
			if err := r.fail(ctx, &h, domain.EndWorktreeLost); err != nil {
				return v, key, err
			}
		}
		if _, err := runs.OrderStopIn(ctx, r.tx, r.act, h.run, h.task, h.lease, domain.StopWorktreeLost); err != nil {
			return v, key, err
		}
		v.Verdict = v1.ReconcileStop
	default: // adopted
		if runs.ControllerEnded(h.run) {
			if _, err := runs.OrderStopIn(ctx, r.tx, r.act, h.run, h.task, h.lease, runs.StopReasonOf(h.run)); err != nil {
				return v, key, err
			}
			v.Verdict = v1.ReconcileStop
		}
	}
	return v, key, nil
}

// assignmentUnknown is a lease whose Worker has neither a record nor a session
// of it: nothing runs, and nothing of it is left. A Run that is not over fails
// (assignment_lost, publication none, its retry policy applies); a Run that is
// over keeps its state. Either way its runtime is stopped and its reservation
// released, after giving up a result that was never sealed (§10.5).
func (r *reconciler) assignmentUnknown(ctx context.Context, h *held) error {
	if !h.run.State.IsTerminal() {
		if err := r.fail(ctx, h, domain.EndAssignmentLost); err != nil {
			return err
		}
	} else {
		// A session that started may have left a result, of which nothing is left
		// now: it is given up, never released as if there had been none.
		mayHave := h.run.StartedAt != nil && (h.run.HarnessRan == nil || *h.run.HarnessRan)
		if h.run.Publication == domain.PublicationNone && mayHave {
			if err := r.tx.MarkExecuted(ctx, h.run); err != nil {
				return err
			}
			h.run.Publication = domain.PublicationPending
		}
		if h.run.Publication == domain.PublicationPending && h.run.ResultCommit == "" {
			if err := r.failPublication(ctx, h, "the Worker lost the result"); err != nil {
				return err
			}
		}
	}
	if h.run.Runtime != domain.RuntimeStopped {
		if err := r.stopRuntime(ctx, h, "the Worker has no session of the Run"); err != nil {
			return err
		}
	}
	return runs.ReleaseIfAllowedIn(ctx, r.tx, r.act, h.run, h.lease)
}

// fail ends a Run that is not over for a reason of the reconciliation.
func (r *reconciler) fail(ctx context.Context, h *held, reason domain.EndReason) error {
	if err := r.tx.FinishRun(ctx, h.run, domain.RunFailed, reason, r.act.Now); err != nil {
		return err
	}
	from := h.run.State
	h.run.State, h.run.EndReason = domain.RunFailed, reason
	if reason == domain.EndWorktreeLost {
		if err := runs.EndByControllerIn(ctx, r.tx, r.act, &h.run, h.task, domain.StopWorktreeLost); err != nil {
			return err
		}
	}
	ev := r.event(h, domain.EventRunFailed, "run failed: "+string(reason), map[string]any{"phase": string(from), "end_reason": string(reason)})
	ev.Level = "warn"
	if _, _, err := r.tx.AppendEvent(ctx, ev); err != nil {
		return err
	}
	return r.s.rederive(ctx, r.tx, r.act, h.task.ID)
}

// session decides a session of the Worker's Runtime it has no record of. A
// session of a lease of this Worker that is not the live one is an orphan; one
// that is no lease of this Worker is ended by its name, and nothing changes.
func (r *reconciler) session(ctx context.Context, name string) (v1.ReconcileVerdict, error) {
	v := v1.ReconcileVerdict{Session: name, Verdict: v1.ReconcileStopSession}
	run, epoch, ok := domain.ParseSessionName(name)
	if !ok {
		return v, nil
	}
	h, found, err := r.lookup(ctx, run, epoch, "")
	if err != nil || !found {
		return v, err
	}
	switch {
	case !h.live():
		if h.lease.Epoch < h.run.LeaseEpoch {
			if _, err := runs.OrphanIn(ctx, r.tx, r.act, h.run, h.task, h.lease); err != nil {
				return v, err
			}
		}
		v.Verdict = v1.ReconcileStale
	case h.run.Runtime == domain.RuntimeStopped:
		v.Verdict = v1.ReconcileStopSession // its processes were confirmed gone: a leftover session
	default:
		v.Verdict = v1.ReconcileContinue // its lease is listed among those to account for
	}
	v.RunID, v.LeaseEpoch = run, epoch
	return v, nil
}

// cursor is where a page of the answer starts: a list and the last key served of it.
type cursor struct {
	list string // "l" leases, "s" stops, "p" publications
	key  string
}

func parseCursor(raw string) (cursor, error) {
	if raw == "" {
		return cursor{list: "l"}, nil
	}
	list, key, ok := strings.Cut(raw, ":")
	if !ok || (list != "l" && list != "s" && list != "p") {
		return cursor{}, input.Invalid("cursor is not one this Controller gave")
	}
	if list == "s" {
		if _, err := strconv.ParseUint(key, 10, 63); err != nil {
			return cursor{}, input.Invalid("cursor is not one this Controller gave")
		}
	}
	return cursor{list, key}, nil
}

// page fills one page of the answer from the cursor: the leases the round did
// not name, then the stops, then the publications, each in its order; Next says
// where the next page starts. It returns how many absent leases it listed.
func (r *reconciler) page(ctx context.Context, named map[leaseKey]bool, raw string, resp *v1.ReconcileResponse) (int, error) {
	c, err := parseCursor(raw)
	if err != nil {
		return 0, err
	}
	switch c.list {
	case "l":
		after := domain.ID(c.key)
		for len(resp.Leases) < leasesPerPage {
			batch, err := store.OpenAcceptedLeases(ctx, r.tx, r.w.ID, after, leasesPerPage)
			if err != nil {
				return 0, err
			}
			for _, l := range batch {
				after = l.ID
				if named[leaseKey{l.RunID, l.Epoch}] {
					continue
				}
				lease, err := r.leaseView(ctx, l)
				if err != nil {
					return 0, err
				}
				resp.Leases = append(resp.Leases, lease)
				if len(resp.Leases) == leasesPerPage {
					resp.Next = "l:" + string(l.ID)
					return len(resp.Leases), nil
				}
			}
			if len(batch) < leasesPerPage {
				break
			}
		}
		resp.Next = "s:0"
	case "s":
		after, _ := strconv.ParseUint(c.key, 10, 63)
		stops, err := store.OpenStopsOf(ctx, r.tx, r.w.ID, after, stopsPerPage)
		if err != nil {
			return 0, err
		}
		for _, st := range stops {
			var cmd v1.StopCommand
			if err := json.Unmarshal(st.Payload, &cmd); err != nil {
				return 0, fmt.Errorf("reconciliation: stop %d of %s is corrupt: %w", st.Seq, r.w.ID, err)
			}
			resp.Stops = append(resp.Stops, cmd)
		}
		resp.Next = "p:"
		if len(stops) == stopsPerPage {
			resp.Next = "s:" + strconv.FormatUint(stops[len(stops)-1].Seq, 10)
		}
	case "p":
		pending, err := store.PendingPublicationsOf(ctx, r.tx, r.w.ID, domain.ID(c.key), pubsPerPage)
		if err != nil {
			return 0, err
		}
		for _, run := range pending {
			resp.Publications = append(resp.Publications, v1.ReconcilePublicationView{
				RunID: string(run.ID), LeaseEpoch: run.LeaseEpoch, Publication: run.Publication, ResultCommit: run.ResultCommit,
			})
		}
		resp.Next = ""
		if len(pending) == pubsPerPage {
			resp.Next = "p:" + string(pending[len(pending)-1].ID)
		}
	}
	if resp.Next != "" && len(resp.Leases)+len(resp.Stops)+len(resp.Publications) == 0 {
		// An empty list says nothing: the next one is looked at in the same request.
		next := resp.Next
		resp.Next = ""
		return r.page(ctx, named, next, resp)
	}
	return len(resp.Leases), nil
}

// leaseView is an absent lease with its whole assignment.
func (r *reconciler) leaseView(ctx context.Context, l store.Lease) (v1.ReconcileLease, error) {
	cmd, err := store.AssignOf(ctx, r.tx, l.ID)
	if err != nil {
		return v1.ReconcileLease{}, err
	}
	var out v1.ReconcileLease
	if err := json.Unmarshal(cmd.Payload, &out.Assign); err != nil {
		return v1.ReconcileLease{}, fmt.Errorf("reconciliation: the assignment of %s is corrupt: %w", l.ID, err)
	}
	run, err := store.RunByID(ctx, r.tx, l.RunID)
	if err != nil {
		return v1.ReconcileLease{}, err
	}
	out.State, out.Runtime, out.StartedAt, out.Publication = run.State, run.Runtime, run.StartedAt, run.Publication
	return out, nil
}

// reconciled closes a complete round: the Worker is reconciled for its epoch.
func (r *reconciler) reconciled(ctx context.Context, req v1.ReconcileRequest, resp *v1.ReconcileResponse) error {
	if err := r.tx.SetReconciled(ctx, r.w.ID, req.Epoch, r.act.Now); err != nil {
		return err
	}
	now := r.act.Now
	resp.Ack.Complete, resp.Ack.ReconciledAt = true, &now
	ev := r.act.Event(domain.EventWorkerReconciled, "worker reconciled")
	ev.WorkerID = r.w.ID
	verdicts := map[string]any{}
	for k, n := range r.verdicts {
		verdicts[k] = n
	}
	ev.Data = map[string]any{
		"epoch": req.Epoch, "round_id": req.Fragment.RoundID, "fragments": req.Fragment.Count,
		"last_seq": req.LastSeq, "verdicts": verdicts,
	}
	_, _, err := r.tx.AppendEvent(ctx, ev)
	return err
}

// validateReconcile checks the shape of a request at the edge.
func validateReconcile(req v1.ReconcileRequest) error {
	f := req.Fragment
	entries := len(req.Runs) + len(req.Sessions) + len(req.Unrecorded)
	switch {
	case ulidOK(req.Epoch) != nil:
		return input.Invalid("epoch must be a ULID")
	case ulidOK(f.RoundID) != nil:
		return input.Invalid("fragment.round_id must be a ULID")
	case f.Count < 1 || f.Count > v1.MaxReconcileFragments || f.Index < 0 || f.Index >= f.Count:
		return input.Invalid("a round has between 1 and %d fragments, and index is one of them", v1.MaxReconcileFragments)
	case entries > v1.MaxReconcileEntries:
		return input.Invalid("a fragment has at most %d runs, sessions and outcomes", v1.MaxReconcileEntries)
	case req.Cursor != "" && (entries > 0 || f.Index != f.Count-1):
		return input.Invalid("a request for the next page carries nothing else, and names the last fragment")
	case len(req.Cursor) > 128:
		return input.Invalid("cursor is not one this Controller gave")
	case req.LastSeq > 1<<62 || req.CommandsCursor > 1<<62:
		return input.Invalid("last_seq and commands_cursor are at most 2^62")
	}
	for _, e := range req.Runs {
		if e.RunID == "" || e.LeaseID == "" || e.LeaseEpoch == 0 || e.LeaseEpoch > 1<<62 || !short(e.RunID) || !short(e.LeaseID) ||
			!short(e.Phase) || !short(e.FencedCode) || e.PendingReports < 0 {
			return input.Invalid("each run names its Run, lease and epoch, with a short phase")
		}
		if p := e.Publication; p != nil && (!short(p.State) || (p.ResultCommit != "" && !domain.ValidCommitID(p.ResultCommit))) {
			return input.Invalid("the publication of a run has a short state and a commit ID")
		}
	}
	for _, s := range req.Sessions {
		if s == "" || !short(s) {
			return input.Invalid("sessions are short names")
		}
	}
	for _, o := range req.Unrecorded {
		switch o.Outcome {
		case v1.OutcomeAssignmentUnknown, v1.OutcomeAdopted, v1.OutcomeWorktreeLost:
		default:
			return input.Invalid("outcome must be assignment_unknown, adopted or worktree_lost")
		}
		if o.LeaseID == "" || o.LeaseEpoch == 0 || o.LeaseEpoch > 1<<62 || !short(o.LeaseID) {
			return input.Invalid("each outcome names its lease and epoch")
		}
	}
	return nil
}

func ulidOK(s string) error {
	_, err := ulid.ParseStrict(s)
	return err
}

// short is an identifier-like text: printable, no spaces, at most 128 bytes.
func short(s string) bool {
	return len(s) <= 128 && !strings.ContainsFunc(s, func(r rune) bool { return unicode.IsSpace(r) || !unicode.IsPrint(r) })
}
