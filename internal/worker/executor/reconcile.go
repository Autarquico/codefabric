package executor

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"time"

	"github.com/oklog/ulid/v2"

	cfruntime "github.com/Autarquico/codefabric/internal/adapters/runtime"

	v1 "github.com/Autarquico/codefabric/internal/api/v1"
	"github.com/Autarquico/codefabric/internal/domain"
	"github.com/Autarquico/codefabric/internal/worker/execwrap"
	"github.com/Autarquico/codefabric/internal/worker/procident"
	"github.com/Autarquico/codefabric/internal/worker/state"
)

// errStaleRound says that a reconciliation was overtaken by a newer request for
// one: what it learnt is not enough to take leases.
var errStaleRound = errors.New("executor: a newer reconciliation was asked for")

// Reconciled says whether the Worker has completed a reconciliation since its
// last start or since the Controller last asked for one. Until it has, no lease
// is answered (§27.3).
func (e *Executor) Reconciled() bool { return e.reconciledGen.Load() == e.reconcileGen.Load() }

// RequestReconcile asks for a reconciliation, as the answer to a heartbeat does
// when the Controller has none of this daemon epoch since it started. No lease
// is answered until it completes. A reconciliation in flight when it is asked
// is not enough: another one follows it.
func (e *Executor) RequestReconcile() {
	e.reconcileGen.Add(1)
	select {
	case e.reconcileWake <- struct{}{}:
	default:
	}
}

// reconcileLoop reconciles at start and whenever it is asked again, with a
// backoff on the clock while the Controller cannot be reached or a round does
// not complete. One reconciliation is in flight at a time.
func (e *Executor) reconcileLoop(ctx context.Context) {
	var backoff time.Duration
	for ctx.Err() == nil {
		gen := e.reconcileGen.Load()
		err := e.reconcile(ctx, gen)
		switch {
		case ctx.Err() != nil:
			return
		case err == nil && e.reconcileGen.Load() == gen:
			backoff = 0
			e.reconciledGen.Store(gen)
			e.log.InfoContext(ctx, "the Worker is reconciled with the Controller")
			e.kickDecider()
			// A wake for a request this round already answered asks for nothing.
			for e.Reconciled() {
				select {
				case <-ctx.Done():
					return
				case <-e.reconcileWake:
				}
			}
			continue
		case err != nil && !errors.Is(err, errStaleRound):
			backoff = nextBackoff(backoff)
			e.log.WarnContext(ctx, "reconciling with the Controller failed; retrying", "err", err, "in", backoff)
			e.pause(ctx, backoff)
		}
	}
}

// sent is what the Worker said of an assignment in a round, to check, before
// acting on the verdict, that it is still so.
type sent struct {
	phase  state.Phase
	fenced string
}

// reconcile is one reconciliation (§27.3, §10.4 point 8): a round with what
// the Worker holds, the verdicts applied, then, if the Controller listed leases
// this Worker has no record of, their outcomes in a second round. It succeeds
// only if the last round is complete.
func (e *Executor) reconcile(ctx context.Context, gen uint64) error {
	runs, held, err := e.snapshot(ctx)
	if err != nil {
		return err
	}
	sessions, err := e.unrecordedSessions(ctx, held)
	if err != nil {
		return err
	}
	out, err := e.round(ctx, gen, runs, sessions, nil, held)
	if err != nil {
		return err
	}
	e.applyStops(ctx, out.Stops, out.Leases)
	// The leases this Worker has no record of are resolved before any result is
	// given up as lost: one it adopts (by its session or its markers) seals and
	// publishes its result, and of one it cannot the Controller decides.
	outcomes, err := e.resolveLeases(ctx, out.Leases)
	if err != nil {
		return err
	}
	if err := e.applyPublications(ctx, out.Publications, out.Leases); err != nil {
		return err
	}
	if out.Ack.Complete {
		return nil
	}
	if runs, held, err = e.snapshot(ctx); err != nil {
		return err
	}
	out, err = e.round(ctx, gen, runs, nil, outcomes, held)
	if err != nil {
		return err
	}
	e.applyStops(ctx, out.Stops, nil)
	if !out.Ack.Complete {
		return errors.New("executor: the Controller holds leases this Worker cannot account for")
	}
	return nil
}

// snapshot is what the Worker holds, read in one view of its state: the
// assignments that are live, fenced, with a publication not settled, with
// reports in the journal or with a stop not done, each once.
func (e *Executor) snapshot(ctx context.Context) ([]v1.ReconcileRun, map[state.Key]sent, error) {
	type item struct {
		run state.Run
		pub *state.Publication
		n   int
	}
	var items []item
	all := map[state.Key]state.Run{}
	err := e.o.DB.Snapshot(ctx, func(q state.Querier) error {
		runs, err := state.AllRuns(ctx, q)
		if err != nil {
			return err
		}
		for _, r := range runs {
			all[r.Key] = r
		}
		pubs, err := state.UnsettledPublications(ctx, q)
		if err != nil {
			return err
		}
		pubOf := map[state.Key]*state.Publication{}
		for i := range pubs {
			pubOf[pubs[i].Key] = &pubs[i]
		}
		counts, err := state.JournalCounts(ctx, q)
		if err != nil {
			return err
		}
		stops, err := state.PendingStopOrders(ctx, q)
		if err != nil {
			return err
		}
		pending := map[state.Key]bool{}
		for _, o := range stops {
			pending[o.Key] = true
		}
		for _, r := range runs {
			if r.Phase.Live() || r.FencedCode != "" || pubOf[r.Key] != nil || counts[r.Key] > 0 || pending[r.Key] {
				items = append(items, item{run: r, pub: pubOf[r.Key], n: counts[r.Key]})
			}
		}
		return nil
	})
	if err != nil {
		return nil, nil, err
	}
	held := map[state.Key]sent{}
	for k, r := range all {
		held[k] = sent{phase: r.Phase, fenced: r.FencedCode}
	}
	out := make([]v1.ReconcileRun, 0, len(items))
	for _, it := range items {
		r := it.run
		alive, err := e.sessionAlive(ctx, r.Key)
		if err != nil {
			return nil, nil, err
		}
		alive = alive || (r.Phase.Live() && e.wrapperAlive(r.Key))
		entry := v1.ReconcileRun{
			RunID: r.RunID, LeaseID: r.LeaseID, LeaseEpoch: r.LeaseEpoch, Phase: string(r.Phase), Alive: alive,
			Started: r.StartedAt != nil, FencedCode: r.FencedCode, PendingReports: it.n, HarnessRan: e.harnessRan(r),
		}
		if p := it.pub; p != nil {
			entry.Publication = &v1.ReconcilePublication{State: string(p.State), ResultCommit: p.ResultCommit, PublishedSeq: p.PublishedSeq}
		}
		out = append(out, entry)
	}
	return out, held, nil
}

// harnessRan is what the markers of a Run that is over say of its harness: it
// ran if it left started.json, and not if its directory exists and has none.
// Of a Run still live, or whose markers cannot be read, nothing is said.
func (e *Executor) harnessRan(r state.Run) *bool {
	if r.Phase.Live() {
		return nil
	}
	if _, err := os.Stat(e.runDir(r.Key)); err != nil {
		return nil
	}
	_, err := os.Stat(filepath.Join(e.runDir(r.Key), execwrap.StartedFile))
	var ran bool
	switch {
	case err == nil:
		ran = true
	case errors.Is(err, fs.ErrNotExist):
		ran = false
	default:
		return nil
	}
	return &ran
}

// unrecordedSessions are the sessions of Runs the Runtime has and this Worker
// has no record of.
func (e *Executor) unrecordedSessions(ctx context.Context, held map[state.Key]sent) ([]string, error) {
	names, err := e.o.Runtime.List(ctx)
	if err != nil {
		return nil, err
	}
	var out []string
	for _, name := range names {
		run, epoch, ok := cfruntime.ParseName(name)
		if _, known := held[state.Key{RunID: run, LeaseEpoch: epoch}]; ok && !known {
			out = append(out, name)
		}
	}
	return out, nil
}

// round sends a round in fragments, applies the verdicts of each, and follows
// the pages of the answer to the last. It returns the answer to the last
// fragment with every page's lists.
func (e *Executor) round(ctx context.Context, gen uint64, runs []v1.ReconcileRun, sessions []string, outcomes []v1.ReconcileOutcome, held map[state.Key]sent) (v1.ReconcileResponse, error) {
	type entry struct {
		run     *v1.ReconcileRun
		session string
		outcome *v1.ReconcileOutcome
	}
	var entries []entry
	for i := range runs {
		entries = append(entries, entry{run: &runs[i]})
	}
	for i := range outcomes {
		entries = append(entries, entry{outcome: &outcomes[i]})
	}
	for _, s := range sessions {
		entries = append(entries, entry{session: s})
	}
	count := max(1, (len(entries)+v1.MaxReconcileEntries-1)/v1.MaxReconcileEntries)
	if count > v1.MaxReconcileFragments {
		return v1.ReconcileResponse{}, fmt.Errorf("executor: %d entries do not fit a round", len(entries))
	}
	cursor, err := state.Cursor(ctx, e.o.DB.Reader())
	if err != nil {
		return v1.ReconcileResponse{}, err
	}
	last, err := state.LastJournalSeq(ctx, e.o.DB.Reader())
	if err != nil {
		return v1.ReconcileResponse{}, err
	}
	req := v1.ReconcileRequest{
		Epoch: e.o.Journal.Epoch(), Fragment: v1.ReconcileFragment{RoundID: ulid.Make().String(), Count: count},
		LastSeq: last, CommandsCursor: cursor,
	}
	var out v1.ReconcileResponse
	for i := range count {
		frag := req
		frag.Fragment.Index = i
		for _, en := range entries[i*v1.MaxReconcileEntries : min(len(entries), (i+1)*v1.MaxReconcileEntries)] {
			switch {
			case en.run != nil:
				frag.Runs = append(frag.Runs, *en.run)
			case en.outcome != nil:
				frag.Unrecorded = append(frag.Unrecorded, *en.outcome)
			default:
				frag.Sessions = append(frag.Sessions, en.session)
			}
		}
		if out, err = e.o.Controller.Reconcile(ctx, frag); err != nil {
			return out, err
		}
		if e.reconcileGen.Load() != gen {
			return out, errStaleRound
		}
		e.applyVerdicts(ctx, out.Verdicts, held, frag.Sessions)
	}
	for out.Next != "" {
		page, err := e.o.Controller.Reconcile(ctx, v1.ReconcileRequest{
			Epoch: req.Epoch, Fragment: v1.ReconcileFragment{RoundID: req.Fragment.RoundID, Index: count - 1, Count: count}, Cursor: out.Next,
		})
		if err != nil {
			return out, err
		}
		out.Leases, out.Stops = append(out.Leases, page.Leases...), append(out.Stops, page.Stops...)
		out.Publications = append(out.Publications, page.Publications...)
		out.Next, out.Ack = page.Next, page.Ack
	}
	if e.reconcileGen.Load() != gen {
		return out, errStaleRound
	}
	return out, nil
}

// applyVerdicts acts on what the Controller decided, each under the lock of its
// Run and only if the Run is as it was when it was sent: what changed meanwhile
// is decided by the next round.
func (e *Executor) applyVerdicts(ctx context.Context, verdicts []v1.ReconcileVerdict, held map[state.Key]sent, sessions []string) {
	for _, v := range verdicts {
		if v.Session != "" {
			// Only a session this Worker sent, in this fragment, is ended for a verdict.
			if slices.Contains(sessions, v.Session) {
				e.applySessionVerdict(ctx, v)
			}
			continue
		}
		k := state.Key{RunID: v.RunID, LeaseEpoch: v.LeaseEpoch}
		before, ok := held[k]
		if !ok {
			continue
		}
		if err := e.applyVerdict(ctx, k, before, v); err != nil && ctx.Err() == nil {
			e.log.ErrorContext(ctx, "applying a verdict of the reconciliation", "run", v.RunID, "verdict", v.Verdict, "err", err)
		}
	}
}

func (e *Executor) applyVerdict(ctx context.Context, k state.Key, before sent, v v1.ReconcileVerdict) error {
	stop := false
	err := func() error {
		defer e.lock(k)()
		r, err := state.RunBy(ctx, e.o.DB.Reader(), k)
		if err != nil {
			return err
		}
		if r.Phase != before.phase || r.FencedCode != before.fenced {
			return nil // it moved since it was sent
		}
		if r.Phase == state.PhaseAssigned {
			return nil // not answered yet: the answer to the lease says what it is worth
		}
		now := e.clock.Now()
		switch v.Verdict {
		case v1.ReconcileStale, v1.ReconcileUnknown:
			e.log.WarnContext(ctx, "the Controller does not own this assignment: it is stopped as an orphan, its worktree kept",
				"run", k.RunID, "epoch", k.LeaseEpoch, "verdict", v.Verdict)
			if err := e.o.DB.WithTx(ctx, func(tx *state.Tx) error {
				became, err := state.MarkOrphaned(ctx, tx, k, now)
				if err != nil {
					return err
				}
				if err := state.DeleteJournalOf(ctx, tx, k); err != nil {
					return err
				}
				if !became {
					return nil // an orphan already: its publication is on its own way, attempts and all
				}
				// On becoming an orphan, a publication that gave up, or pushed the
				// branch, is tried again for the orphan's ref; once, not at every stale.
				return state.RequeueForOrphan(ctx, tx, k, now)
			}); err != nil {
				return err
			}
			e.kickPublisher()
			stop = r.Phase.Live()
		case v1.ReconcileStop:
			stop = r.Phase.Live()
		}
		if v.Drop {
			if err := e.o.DB.WithTx(ctx, func(tx *state.Tx) error { return state.DeleteJournalOf(ctx, tx, k) }); err != nil {
				return err
			}
		} else if v.Resend && r.FencedCode != "" {
			if err := e.o.DB.WithTx(ctx, func(tx *state.Tx) error { return state.Unfence(ctx, tx, k, r.FencedCode) }); err != nil {
				return err
			}
			e.o.Journal.Notify()
		}
		if p := v.Publication; p != nil {
			return e.confirmFromView(ctx, k, *p)
		}
		return nil
	}()
	if err != nil || !stop {
		return err
	}
	reason := "reconcile"
	if v.Verdict != v1.ReconcileStop {
		reason = string(domain.StopOrphan)
	}
	return e.Stop(ctx, k.RunID, k.LeaseEpoch, reason, SourceController)
}

// confirmFromView settles a pushed publication whose `published` report the
// journal no longer holds (it was applied, and the answer lost with the daemon),
// from the Controller's view: only if the commit is the one pushed and the
// report that settled it is this push's own.
func (e *Executor) confirmFromView(ctx context.Context, k state.Key, view v1.ReconcilePublicationView) error {
	p, err := state.PublicationOf(ctx, e.o.DB.Reader(), k)
	if err != nil || p.State != state.PubPushed || p.ResultCommit != view.ResultCommit || p.PublishedSeq == 0 || p.PublishedSeq != view.SettledSeq {
		return nil
	}
	if inJournal, err := state.HasJournalSeq(ctx, e.o.DB.Reader(), p.PublishedSeq); err != nil || inJournal {
		return err // still on its way: the answer to it confirms it
	}
	var to state.PubState
	switch view.Publication {
	case domain.PublicationPublished:
		to = state.PubPublished
	case domain.PublicationFailed:
		to = state.PubFailed
	default:
		return nil
	}
	return e.o.DB.WithTx(ctx, func(tx *state.Tx) error {
		_, err := state.ConfirmPublication(ctx, tx, k, []uint64{p.PublishedSeq}, to, e.clock.Now())
		return err
	})
}

// applySessionVerdict ends a session the Controller says is not of a lease of
// this Worker, by its name. One that is an orphan of this Worker is stopped by
// the stop order the Controller queued for it.
func (e *Executor) applySessionVerdict(ctx context.Context, v v1.ReconcileVerdict) {
	if v.Verdict != v1.ReconcileStopSession {
		return
	}
	run, epoch, ok := cfruntime.ParseName(v.Session)
	if !ok {
		return // never a session of a Run: not ours to end
	}
	if _, err := state.RunBy(ctx, e.o.DB.Reader(), state.Key{RunID: run, LeaseEpoch: epoch}); err == nil {
		return // it has a record now: it is stopped the normal way
	}
	e.log.WarnContext(ctx, "a session that is no Run of this Worker is ended", "session", v.Session)
	if err := e.o.Runtime.Stop(ctx, e.o.Runtime.HandleFor(v.Session)); err != nil {
		e.log.WarnContext(ctx, "ending a session failed", "session", v.Session, "err", err)
	}
}

// applyStops carries out the stops the Controller owes this Worker. One of a
// lease listed as unaccounted for is left to the second round, which settles it.
func (e *Executor) applyStops(ctx context.Context, stops []v1.StopCommand, listed []v1.ReconcileLease) {
	skip := map[state.Key]bool{}
	for _, l := range listed {
		skip[state.Key{RunID: l.Assign.RunID, LeaseEpoch: l.Assign.Epoch}] = true
	}
	for _, s := range stops {
		k := state.Key{RunID: s.RunID, LeaseEpoch: s.Epoch}
		if skip[k] {
			continue
		}
		if kind, _, err := domain.ParseID(s.RunID); err != nil || kind != domain.KindRun || s.Epoch == 0 {
			continue
		}
		if _, err := state.RunBy(ctx, e.o.DB.Reader(), k); err == nil {
			if err := e.o.DB.WithTx(ctx, func(tx *state.Tx) error {
				_, err := state.AddStopOrder(ctx, tx, state.StopOrder{Key: k, Reason: string(s.Reason), Source: SourceController, CreatedAt: e.clock.Now()})
				return err
			}); err != nil {
				e.log.ErrorContext(ctx, "recording a stop of the reconciliation", "run", s.RunID, "err", err)
				continue
			}
		}
		if err := e.stopOrdered(ctx, s); err != nil && ctx.Err() == nil {
			e.log.ErrorContext(ctx, "carrying out a stop of the reconciliation", "run", s.RunID, "err", err)
		}
	}
}

// applyPublications answers the publications the Controller waits for and this
// Worker has no record of at all: it cannot publish them, and says so once.
// One of a lease the round listed is not among them (it was adopted, or the
// Controller decides of it by its outcome), and each is looked at again under
// the lock of its Run before its result is said to be lost.
func (e *Executor) applyPublications(ctx context.Context, views []v1.ReconcilePublicationView, listed []v1.ReconcileLease) error {
	skip := map[state.Key]bool{}
	for _, l := range listed {
		skip[state.Key{RunID: l.Assign.RunID, LeaseEpoch: l.Assign.Epoch}] = true
	}
	for _, p := range views {
		k := state.Key{RunID: p.RunID, LeaseEpoch: p.LeaseEpoch}
		if kind, _, err := domain.ParseID(p.RunID); err != nil || kind != domain.KindRun || p.LeaseEpoch == 0 || skip[k] {
			continue
		}
		if err := e.publicationLost(ctx, k); err != nil {
			return err
		}
	}
	return nil
}

// publicationLost reports, under the lock of the Run, that this Worker has no
// result of it, if it still has no record, no publication and no report of it
// on its way. (A Run whose lease still holds its reservation is listed, and
// adopted from its markers; this one's result was sealed, and its record is gone.)
func (e *Executor) publicationLost(ctx context.Context, k state.Key) error {
	defer e.lock(k)()
	if _, err := state.RunBy(ctx, e.o.DB.Reader(), k); !errors.Is(err, state.ErrNotFound) {
		return nil // it has a record, or it cannot be read: not lost
	}
	if _, err := state.PublicationOf(ctx, e.o.DB.Reader(), k); !errors.Is(err, state.ErrNotFound) {
		return nil
	}
	counts, err := state.JournalCounts(ctx, e.o.DB.Reader())
	if err != nil || counts[k] > 0 {
		return err // said already, and on its way
	}
	err = e.o.DB.WithTx(ctx, func(tx *state.Tx) error {
		return e.report(ctx, tx, k, v1.ReportPublicationFailed, e.clock.Now(), func(en *v1.RunReportEntry) {
			en.Phase, en.Message = v1.PhaseSeal, "this Worker has no record of the Run"
		})
	})
	if err == nil {
		e.o.Journal.Notify()
	}
	return err
}

// wrapperAlive says whether the wrapper of the Run is the process it recorded
// and alive: a session is only as sure as the Runtime that answers for it, and
// the markers of the Run are the evidence that does not depend on it.
func (e *Executor) wrapperAlive(k state.Key) bool {
	var w execwrap.Wrapper
	if found, err := execwrap.Read(e.runDir(k), execwrap.WrapperFile, &w); err != nil || !found {
		return false
	}
	return verdictOf(&w) == procident.Same
}

// ranHere says whether the wrapper of the Run left any marker: it started on
// this Worker, whatever became of it since.
func (e *Executor) ranHere(k state.Key) bool {
	for _, name := range []string{execwrap.WrapperFile, execwrap.StartedFile, execwrap.ExitFile} {
		if _, err := os.Lstat(filepath.Join(e.runDir(k), name)); err == nil {
			return true
		}
	}
	return false
}

// resolveLeases decides what became of each lease the Controller listed that
// this Worker did not name (§10.4 point 8). One it has a record of goes in the
// runs of the next round. Of one it has none: no session and no markers of its
// wrapper, assignment_unknown; a session or markers without its worktree,
// worktree_lost; with it, the record is rebuilt from the lease and the Run
// adopted, and the recovery of #11 supervises or settles it from its markers.
func (e *Executor) resolveLeases(ctx context.Context, leases []v1.ReconcileLease) ([]v1.ReconcileOutcome, error) {
	var out []v1.ReconcileOutcome
	for _, l := range leases {
		a := l.Assign
		if kind, _, err := domain.ParseID(a.RunID); err != nil || kind != domain.KindRun || a.Epoch == 0 {
			continue
		}
		k := state.Key{RunID: a.RunID, LeaseEpoch: a.Epoch}
		if _, err := state.RunBy(ctx, e.o.DB.Reader(), k); err == nil {
			continue // it goes in the runs of the next round
		} else if !errors.Is(err, state.ErrNotFound) {
			return nil, err
		}
		outcome, err := e.resolveLease(ctx, k, l)
		if err != nil {
			return nil, err
		}
		out = append(out, v1.ReconcileOutcome{LeaseID: a.LeaseID, LeaseEpoch: a.Epoch, Outcome: outcome})
	}
	return out, nil
}

func (e *Executor) resolveLease(ctx context.Context, k state.Key, l v1.ReconcileLease) (string, error) {
	defer e.lock(k)()
	alive, err := e.sessionAlive(ctx, k)
	if err != nil {
		return "", err
	}
	// The session is not the only evidence that the Run executed here: the wrapper
	// leaves its markers in runs/<run>-e<epoch>, and a harness that ended while no
	// daemon was looking has a session no more, but an exit.json and a worktree to
	// seal. Only with neither did nothing ever run.
	if !alive && !e.ranHere(k) {
		e.log.WarnContext(ctx, "the Controller holds a lease this Worker has no record, session or markers of", "run", k.RunID, "epoch", k.LeaseEpoch)
		return v1.OutcomeAssignmentUnknown, nil
	}
	if _, err := os.Stat(e.worktree(k)); err != nil {
		e.log.WarnContext(ctx, "a Run executed here and its worktree is gone", "run", k.RunID, "epoch", k.LeaseEpoch, "session", alive)
		return v1.OutcomeWorktreeLost, nil
	}
	a := l.Assign
	spec := parseSpec(a.Spec)
	r := state.Run{
		Key: k, LeaseID: a.LeaseID, RunShort: a.RunShort, TaskID: a.TaskID, ProjectID: a.ProjectID, AcceptBy: a.AcceptBy,
		SourceCommit: a.SourceCommit, Branch: a.Branch, Repo: a.Repo, CPU: a.Resources.CPU, MemoryMB: a.Resources.MemoryMB,
		Spec: a.Spec, Harness: spec.Requirements.Harness, Mode: spec.Mode, CreatedAt: e.clock.Now(),
	}
	// The matrix of the adoption: a session the Controller knows of is running and
	// its `started` is never reported again; one it does not is taken up as
	// starting, whose recovery wires it and reports the first `started`. The
	// markers decide the rest: an exit is settled, a wrapper is supervised.
	if l.Runtime == domain.RuntimeNone {
		r.Phase = state.PhaseStarting
	} else {
		if alive {
			if err := e.o.Runtime.Wire(ctx, e.sessionSpec(r)); err != nil {
				return "", fmt.Errorf("executor: wiring an adopted session: %w", err)
			}
		}
		at := e.clock.Now()
		if l.StartedAt != nil {
			at = *l.StartedAt
		}
		r.Phase, r.StartedAt, r.Handle = state.PhaseRunning, &at, mustJSON(*e.runtimeHandle(k))
	}
	e.log.InfoContext(ctx, "a Run this Worker had no record of is adopted", "run", k.RunID, "epoch", k.LeaseEpoch, "phase", r.Phase, "session", alive)
	if err := e.o.DB.WithTx(ctx, func(tx *state.Tx) error {
		_, err := state.InsertAdopted(ctx, tx, r)
		return err
	}); err != nil {
		return "", err
	}
	if e.spawn != nil {
		e.spawn(k)
	}
	return v1.OutcomeAdopted, nil
}
