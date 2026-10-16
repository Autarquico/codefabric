package executor

import (
	"context"
	"errors"
	"log/slog"
	"time"

	v1 "github.com/Autarquico/codefabric/internal/api/v1"
	"github.com/Autarquico/codefabric/internal/domain"
	"github.com/Autarquico/codefabric/internal/worker/gitops"
	"github.com/Autarquico/codefabric/internal/worker/secretjit"
	"github.com/Autarquico/codefabric/internal/worker/state"
)

// Backoff between tries of a publication that could not reach the Controller or
// the relay. It starts here and doubles up to the maximum, on the executor's
// clock, and the time of the next try is persisted: a daemon that restarts goes on.
const (
	publishBackoffInitial = 2 * time.Second
	publishBackoffMax     = 5 * time.Minute
	publishBatch          = 16
)

// persistPublish stores a publish order in the transaction of the cursor: the
// publication starts a new try, with the commit it has or sealing again if it has
// none. A Worker that has no record of the Run tells the Controller so, which
// puts the publication back to failed instead of leaving it pending for good.
func (e *Executor) persistPublish(ctx context.Context, tx *state.Tx, c v1.WorkerCommand, now time.Time) error {
	p := c.Publish
	if kind, _, err := domain.ParseID(p.RunID); err != nil || kind != domain.KindRun || p.Epoch == 0 {
		e.log.ErrorContext(ctx, "a publish order does not name a Run; it is ignored", "seq", c.Seq)
		return state.SetCursor(ctx, tx, c.Seq)
	}
	k := state.Key{RunID: p.RunID, LeaseEpoch: p.Epoch}
	requeued, err := state.RequeuePublication(ctx, tx, k, now)
	if err != nil {
		return err
	}
	if !requeued {
		if _, err := state.PublicationOf(ctx, tx, k); errors.Is(err, state.ErrNotFound) {
			if err := e.report(ctx, tx, k, v1.ReportPublicationFailed, now, func(en *v1.RunReportEntry) {
				en.Phase, en.Message = v1.PhaseSeal, "this Worker has no record of the Run"
			}); err != nil {
				return err
			}
		} else if err != nil {
			return err
		}
	}
	return state.SetCursor(ctx, tx, c.Seq)
}

func (e *Executor) kickPublisher() {
	select {
	case e.pubWake <- struct{}{}:
	default:
	}
}

// publishLoop seals and pushes, one publication at a time, until ctx ends.
func (e *Executor) publishLoop(ctx context.Context) {
	ticker := e.clock.NewTicker(tick)
	defer ticker.Stop()
	for ctx.Err() == nil {
		for e.publishDue(ctx) {
		}
		select {
		case <-ctx.Done():
			return
		case <-e.pubWake:
		case <-ticker.C():
		}
	}
}

// publishDue handles the publications whose time has come and says whether it
// did anything that may have made another due.
func (e *Executor) publishDue(ctx context.Context) bool {
	due, err := state.DuePublications(ctx, e.o.DB.Reader(), e.clock.Now(), publishBatch)
	if err != nil {
		if ctx.Err() == nil {
			e.log.ErrorContext(ctx, "reading the publications that are due", "err", err)
		}
		return false
	}
	progressed := false
	for _, p := range due {
		if ctx.Err() != nil {
			return false
		}
		if e.deferred(p.Key) {
			continue
		}
		switch p.State {
		case state.PubSealing:
			progressed = e.seal(ctx, p) || progressed
		case state.PubSealed:
			progressed = e.push(ctx, p) || progressed
		}
	}
	return progressed
}

func (e *Executor) worktree(k state.Key) string {
	return e.o.Paths.WorkerWorktreeDir(k.RunID, k.LeaseEpoch)
}

// seal makes the commit of a publication from its intent and records it, with
// the report that tells of it, in one transaction. If git fails and the state
// can still be written, the failure is final until a publish order. If the
// state cannot be written either, nothing is reported, the intent stays, and the
// next turn seals again (to the same commit: the private ref has it).
func (e *Executor) seal(ctx context.Context, p state.Publication) bool {
	local, err := state.RunBy(ctx, e.o.DB.Reader(), p.Key)
	if err != nil {
		e.log.ErrorContext(ctx, "reading the Run to seal", "run", p.RunID, "err", err)
		e.later(ctx, p, "the Run could not be read")
		return false
	}
	if p.ParentOID == "" {
		// The exit could not read HEAD (no repository, or git failing): read it again.
		if head, err := e.o.Git.Head(ctx, e.worktree(p.Key)); err == nil {
			txErr := e.o.DB.WithTx(ctx, func(tx *state.Tx) error { return state.SetParent(ctx, tx, p, head, e.clock.Now()) })
			if txErr != nil {
				return e.settled(ctx, p, txErr, "completing a sealing intent")
			}
			p.ParentOID = head
		}
	}
	var sealed gitops.Sealed
	forbidden, err := e.forbidden(local)
	if err == nil {
		sealed, err = e.o.Git.Seal(ctx, e.worktree(p.Key), gitops.Intent{
			Project: local.ProjectID, Run: p.RunID, LeaseEpoch: p.LeaseEpoch, ParentOID: p.ParentOID, At: p.SealedAt,
		}, forbidden)
	}
	now := e.clock.Now()
	if err != nil {
		if ctx.Err() != nil {
			return false
		}
		e.log.WarnContext(ctx, "sealing a Run failed", "run", p.RunID, "err", err)
		msg := "sealing failed: " + briefErr(err)
		txErr := e.o.DB.WithTx(ctx, func(tx *state.Tx) error {
			if err := state.FailPublication(ctx, tx, p, msg, now); err != nil {
				return err
			}
			return e.report(ctx, tx, p.Key, v1.ReportPublicationFailed, now, func(en *v1.RunReportEntry) {
				en.Phase, en.Message = v1.PhaseSeal, msg
			})
		})
		return e.settled(ctx, p, txErr, "recording a failed sealing")
	}
	e.log.InfoContext(ctx, "run sealed", "run", p.RunID, "commit", sealed.Commit, "excluded", len(sealed.Excluded))
	txErr := e.o.DB.WithTx(ctx, func(tx *state.Tx) error {
		if err := state.MarkSealed(ctx, tx, p, sealed.Commit, sealed.Excluded, sealed.Truncated, now); err != nil {
			return err
		}
		return e.report(ctx, tx, p.Key, v1.ReportSealed, now, func(en *v1.RunReportEntry) {
			en.ResultCommit, en.Excluded, en.ExcludedTruncated = sealed.Commit, sealed.Excluded, sealed.Truncated
		})
	})
	return e.settled(ctx, p, txErr, "recording a sealing")
}

// push sends the sealed commit to the relay, or schedules the next try.
func (e *Executor) push(ctx context.Context, p state.Publication) bool {
	local, err := state.RunBy(ctx, e.o.DB.Reader(), p.Key)
	if err != nil {
		e.log.ErrorContext(ctx, "reading the Run to push", "run", p.RunID, "err", err)
		e.later(ctx, p, "the Run could not be read")
		return false
	}
	forbidden, err := e.forbidden(local)
	if err != nil {
		return e.blocked(ctx, p, err)
	}
	if local.OrphanedAt != nil {
		return e.preserveOrphan(ctx, p, local, forbidden)
	}
	tok, err := e.o.Controller.RelayToken(ctx, p.RunID, p.LeaseEpoch)
	if err != nil {
		if ctx.Err() != nil {
			return false
		}
		return e.tokenFailed(ctx, p, err, false)
	}
	if !tok.Write {
		// The Controller does not have the sealing yet (its report is on its way) or no
		// longer waits for this result. Not an attempt: look again later.
		e.log.DebugContext(ctx, "the relay gives no write access yet", "run", p.RunID)
		e.later(ctx, p, "the relay gives no write access yet")
		return false
	}
	pushErr := e.o.Git.Push(ctx, local.ProjectID, p.ResultCommit, local.Branch, gitops.PushAuth{
		URL: e.o.Controller.RelayURL(tok.Path), Token: tok.Token,
		Cert: e.o.RelayTLS.Cert, Key: e.o.RelayTLS.Key, CA: e.o.RelayTLS.CA,
	}, forbidden)
	if ctx.Err() != nil {
		return false
	}
	if isBlocked(pushErr) {
		return e.blocked(ctx, p, pushErr)
	}
	now := e.clock.Now()
	if pushErr == nil {
		e.forgetBackoff(p.Key)
		txErr := e.o.DB.WithTx(ctx, func(tx *state.Tx) error {
			// The seq of the `published` report is kept with the push: only the confirmation of
			// that very report may end this try.
			seq, err := e.reportSeq(ctx, tx, p.Key, v1.ReportPublished, now, func(en *v1.RunReportEntry) { en.ResultCommit = p.ResultCommit })
			if err != nil {
				return err
			}
			return state.MarkPushed(ctx, tx, p, seq, now)
		})
		if errors.Is(txErr, errOrphaned) {
			// It became an orphan while it pushed: the push is not recorded, and the
			// next turn keeps its result by the orphan's ref.
			e.log.InfoContext(ctx, "the Run became an orphan during its push: its result goes to the orphan's ref", "run", p.RunID)
			e.kickPublisher()
			return true
		}
		return e.settled(ctx, p, txErr, "recording a push")
	}
	var pe *gitops.PushError
	if !errors.As(pushErr, &pe) {
		pe = &gitops.PushError{Class: gitops.PushLocal, Message: briefErr(pushErr)}
	}
	e.log.WarnContext(ctx, "pushing a result failed", "run", p.RunID, "class", pe.Class, "err", pe.Message)
	return e.pushFailed(ctx, p, pe.Counts(), pe.Message)
}

// preserveOrphan keeps the sealed result of an orphan in the relay, at the ref
// only it may create (§10.4 point 5), and tells the Controller with the one
// report it takes under an epoch that is not the live one.
func (e *Executor) preserveOrphan(ctx context.Context, p state.Publication, local state.Run, forbidden gitops.Forbidden) bool {
	tok, err := e.o.Controller.OrphanToken(ctx, p.RunID, p.LeaseEpoch, p.ResultCommit)
	if err != nil {
		if ctx.Err() != nil {
			return false
		}
		return e.tokenFailed(ctx, p, err, true)
	}
	ref := domain.OrphanRef(domain.ID(p.RunID), p.LeaseEpoch)
	if tok.Ref != ref {
		// The token is for another ref than the one of this orphan: it is not used.
		return e.tokenFailed(ctx, p, domain.NewError(domain.CodePolicyDenied, "the token is for another ref than the orphan's", nil), true)
	}
	pushErr := e.o.Git.PushRef(ctx, local.ProjectID, p.ResultCommit, ref, gitops.PushAuth{
		URL: e.o.Controller.RelayURL(tok.Path), Token: tok.Token,
		Cert: e.o.RelayTLS.Cert, Key: e.o.RelayTLS.Key, CA: e.o.RelayTLS.CA,
	}, forbidden)
	if ctx.Err() != nil {
		return false
	}
	if isBlocked(pushErr) {
		return e.blocked(ctx, p, pushErr)
	}
	if pushErr != nil {
		var pe *gitops.PushError
		if !errors.As(pushErr, &pe) {
			pe = &gitops.PushError{Class: gitops.PushLocal, Message: briefErr(pushErr)}
		}
		e.log.WarnContext(ctx, "preserving the result of an orphan failed", "run", p.RunID, "class", pe.Class, "err", pe.Message)
		return e.pushFailed(ctx, p, pe.Counts(), pe.Message)
	}
	e.forgetBackoff(p.Key)
	now := e.clock.Now()
	e.log.InfoContext(ctx, "the result of an orphan is kept in the relay", "run", p.RunID, "epoch", p.LeaseEpoch, "ref", ref)
	txErr := e.o.DB.WithTx(ctx, func(tx *state.Tx) error {
		if err := e.report(ctx, tx, p.Key, v1.ReportOrphanPreserved, now, func(en *v1.RunReportEntry) { en.ResultCommit = p.ResultCommit }); err != nil {
			return err
		}
		return state.MarkPreserved(ctx, tx, p, now)
	})
	return e.settled(ctx, p, txErr, "recording an orphan preserved")
}

// forbidden is what the publication of the Run local is checked against
// (gitops.Forbidden): the values it keeps for that until the publication is
// final. A Run that declared secrets and holds none any more, because they were
// released when an earlier publication ended, cannot be checked: it is not
// published, and the error, a *gitops.BlockedError, says why with no value.
func (e *Executor) forbidden(local state.Run) (gitops.Forbidden, error) {
	values, err := secretjit.Load(e.runDir(local.Key))
	if errors.Is(err, secretjit.ErrNotDelivered) {
		err = nil
	}
	switch {
	case err != nil:
		return gitops.Forbidden{}, &gitops.BlockedError{Reason: "the values of the secrets of the Run cannot be read, so its result cannot be checked: " + err.Error()}
	case len(values) > 0:
		return gitops.Forbidden{Since: local.SourceCommit, Values: values}, nil
	case len(parseSpec(local.Spec).refs()) > 0:
		return gitops.Forbidden{}, &gitops.BlockedError{Reason: "the values of the secrets of the Run are no longer on this Worker, so its result cannot be checked"}
	}
	return gitops.Forbidden{}, nil
}

func isBlocked(err error) bool {
	var b *gitops.BlockedError
	return errors.As(err, &b)
}

// blocked ends a publication the guard did not let through: nothing was sent,
// and trying again would find the same.
func (e *Executor) blocked(ctx context.Context, p state.Publication, err error) bool {
	e.log.WarnContext(ctx, "the publication of a Run is blocked", "run", p.RunID, "err", err)
	return e.giveUp(ctx, p, briefErr(err))
}

// tokenFailed handles a token the Controller would not give. What says the Run is
// not ours any more (stale lease, unknown Run) ends the publication; what could
// pass (the Controller down, an internal error) does not count.
func (e *Executor) tokenFailed(ctx context.Context, p state.Publication, err error, orphan bool) bool {
	var coded *domain.Error
	if errors.As(err, &coded) {
		switch coded.Code {
		case domain.CodeStaleLease:
			if !orphan {
				// The lease is not the live one: the reconciliation makes the Run an
				// orphan, and the result, which is sealed, is kept by the orphan's ref.
				// Nothing is given up meanwhile.
				e.later(ctx, p, "the lease is not the live one: waiting for the reconciliation")
				return false
			}
			return e.giveUp(ctx, p, "the Controller no longer gives this Worker a token for the Run: "+coded.Message)
		case domain.CodeNotFound, domain.CodePolicyDenied:
			return e.giveUp(ctx, p, "the Controller no longer gives this Worker a token for the Run: "+coded.Message)
		case domain.CodeUnauthenticated:
			return e.pushFailed(ctx, p, true, "the Controller does not accept the credentials of this Worker")
		}
	}
	level := slog.LevelDebug
	if e.firstDelay(p.Key) {
		level = slog.LevelWarn // once per publication; the next tries are not news
	}
	e.log.Log(ctx, level, "asking for a token failed; it will be tried again", "run", p.RunID, "err", err)
	e.later(ctx, p, "the Controller gave no token: "+briefErr(err))
	return false
}

// pushFailed schedules the next try; a failure that counts brings the publication
// closer to PublicationMaxAttempts, where it ends.
func (e *Executor) pushFailed(ctx context.Context, p state.Publication, counted bool, why string) bool {
	now := e.clock.Now()
	if counted && p.Attempts+1 >= domain.Defaults().PublicationMaxAttempts {
		return e.giveUp(ctx, p, why)
	}
	next := now.Add(e.nextBackoff(p.Key))
	err := e.o.DB.WithTx(ctx, func(tx *state.Tx) error {
		_, err := state.Retry(ctx, tx, p, counted, next, why, now)
		return err
	})
	return e.settled(ctx, p, err, "scheduling a push")
}

// giveUp ends a publication: failed here, and reported.
func (e *Executor) giveUp(ctx context.Context, p state.Publication, why string) bool {
	now := e.clock.Now()
	msg := "push failed: " + why
	err := e.o.DB.WithTx(ctx, func(tx *state.Tx) error {
		if err := state.FailPublication(ctx, tx, p, msg, now); err != nil {
			return err
		}
		return e.report(ctx, tx, p.Key, v1.ReportPublicationFailed, now, func(en *v1.RunReportEntry) {
			en.Phase, en.Message = v1.PhasePush, msg
		})
	})
	e.forgetBackoff(p.Key)
	return e.settled(ctx, p, err, "recording a failed publication")
}

// settled interprets the end of a transaction of the publisher. A compare-and-set
// that does not match means a newer try took over (a publish order): the work of
// this one is dropped. Any other error leaves the publication as it was, to be
// tried again.
func (e *Executor) settled(ctx context.Context, p state.Publication, err error, what string) bool {
	switch {
	case err == nil:
		e.o.Journal.Notify()
		return true
	case errors.Is(err, state.ErrChanged):
		e.log.InfoContext(ctx, "a newer try of the publication took over; the result of this one is dropped", "run", p.RunID, "doing", what)
		return true
	case ctx.Err() == nil:
		e.log.ErrorContext(ctx, "the state of the Worker could not be written; it will be tried again", "run", p.RunID, "doing", what, "err", err)
		e.later(ctx, p, "the state of the Worker could not be written")
	}
	return false
}

// pubMemo is what the publisher keeps in memory of a publication: how long it
// waits after the relay or the Controller could not be reached, and until when it
// leaves one alone that cannot move now without anything to persist.
type pubMemo struct {
	delay     time.Duration
	notBefore time.Time
}

// nextBackoff doubles the wait of a publication, up to the maximum.
func (e *Executor) nextBackoff(k state.Key) time.Duration {
	e.backoffMu.Lock()
	defer e.backoffMu.Unlock()
	m := e.pubMemos[k]
	if m == nil {
		m = new(pubMemo)
		e.pubMemos[k] = m
	}
	m.delay = min(max(m.delay*2, publishBackoffInitial), publishBackoffMax)
	return m.delay
}

// firstDelay says whether the publication has not waited yet.
func (e *Executor) firstDelay(k state.Key) bool {
	e.backoffMu.Lock()
	defer e.backoffMu.Unlock()
	m := e.pubMemos[k]
	return m == nil || m.delay == 0
}

func (e *Executor) forgetBackoff(k state.Key) {
	e.backoffMu.Lock()
	defer e.backoffMu.Unlock()
	delete(e.pubMemos, k)
}

// later postpones a publication that cannot move now. The postponement is
// persisted (compare-and-set on the try), so that publications that are waiting
// do not take the place of those that are due; if even that cannot be written
// the wait is kept in memory.
func (e *Executor) later(ctx context.Context, p state.Publication, why string) {
	wait := e.nextBackoff(p.Key)
	now := e.clock.Now()
	err := e.o.DB.WithTx(ctx, func(tx *state.Tx) error { return state.Defer(ctx, tx, p, now.Add(wait), why, now) })
	if err == nil || errors.Is(err, state.ErrChanged) {
		return
	}
	e.backoffMu.Lock()
	defer e.backoffMu.Unlock()
	e.pubMemos[p.Key].notBefore = now.Add(wait)
}

func (e *Executor) deferred(k state.Key) bool {
	e.backoffMu.Lock()
	defer e.backoffMu.Unlock()
	m := e.pubMemos[k]
	return m != nil && e.clock.Now().Before(m.notBefore)
}

// briefErr is the text of an error as a report may carry it.
func briefErr(err error) string { return reportable(err.Error()) }

// ConfirmView is what the sender hands over for each Run view the Controller
// answered with, in the transaction that deletes the confirmed reports: a push
// is confirmed (or failed) only when the Controller's own view of the Run says
// so. confirmed are the seqs of the reports that request settled: the view
// confirms the push whose `published` report is among them, and no other, so the
// answer to an older try never moves a newer one. A view of another lease, or of a
// Run that is not waiting for it, changes nothing.
func (e *Executor) ConfirmView(ctx context.Context, tx *state.Tx, k state.Key, confirmed []uint64, run v1.Run) error {
	if run.Lease == nil || run.Lease.Epoch != k.LeaseEpoch {
		return nil
	}
	var to state.PubState
	switch run.Publication {
	case domain.PublicationPublished:
		to = state.PubPublished
	case domain.PublicationFailed:
		to = state.PubFailed
	default:
		return nil
	}
	_, err := state.ConfirmPublication(ctx, tx, k, confirmed, to, e.clock.Now())
	return err
}
