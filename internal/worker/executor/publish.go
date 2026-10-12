package executor

import (
	"context"
	"errors"
	"time"

	v1 "github.com/Autarquico/codefabric/internal/api/v1"
	"github.com/Autarquico/codefabric/internal/domain"
	"github.com/Autarquico/codefabric/internal/worker/gitops"
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
	e.kickPublisher()
	e.o.Journal.Notify()
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
		e.later(p)
		return false
	}
	sealed, err := e.o.Git.Seal(ctx, e.worktree(p.Key), gitops.Intent{
		Project: local.ProjectID, Run: p.RunID, LeaseEpoch: p.LeaseEpoch, Branch: local.Branch, ParentOID: p.ParentOID, At: p.SealedAt,
	})
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
		e.later(p)
		return false
	}
	tok, err := e.o.Controller.RelayToken(ctx, p.RunID, p.LeaseEpoch)
	if err != nil {
		if ctx.Err() != nil {
			return false
		}
		return e.tokenFailed(ctx, p, err)
	}
	if !tok.Write {
		// The Controller does not have the sealing yet (its report is on its way) or no
		// longer waits for this result. Not an attempt: look again later.
		e.log.DebugContext(ctx, "the relay gives no write access yet", "run", p.RunID)
		e.later(p)
		return false
	}
	pushErr := e.o.Git.Push(ctx, local.ProjectID, p.ResultCommit, local.Branch, gitops.PushAuth{
		URL: e.o.Controller.RelayURL(tok.Path), Token: tok.Token,
		Cert: e.o.RelayTLS.Cert, Key: e.o.RelayTLS.Key, CA: e.o.RelayTLS.CA,
	})
	if ctx.Err() != nil {
		return false
	}
	now := e.clock.Now()
	if pushErr == nil {
		e.forgetBackoff(p.Key)
		txErr := e.o.DB.WithTx(ctx, func(tx *state.Tx) error {
			if err := state.MarkPushed(ctx, tx, p, now); err != nil {
				return err
			}
			return e.report(ctx, tx, p.Key, v1.ReportPublished, now, func(en *v1.RunReportEntry) { en.ResultCommit = p.ResultCommit })
		})
		return e.settled(ctx, p, txErr, "recording a push")
	}
	var pe *gitops.PushError
	if !errors.As(pushErr, &pe) {
		pe = &gitops.PushError{Class: gitops.PushLocal, Message: briefErr(pushErr)}
	}
	e.log.WarnContext(ctx, "pushing a result failed", "run", p.RunID, "class", pe.Class, "err", pe.Message)
	return e.pushFailed(ctx, p, pe.Counts(), pe.Message)
}

// tokenFailed handles a token the Controller would not give. What says the Run is
// not ours any more (stale lease, unknown Run) ends the publication; what could
// pass (the Controller down, an internal error) does not count.
func (e *Executor) tokenFailed(ctx context.Context, p state.Publication, err error) bool {
	var coded *domain.Error
	if errors.As(err, &coded) {
		switch coded.Code {
		case domain.CodeStaleLease, domain.CodeNotFound, domain.CodePolicyDenied:
			return e.giveUp(ctx, p, "the Controller no longer gives this Worker a token for the Run: "+coded.Message)
		case domain.CodeUnauthenticated:
			return e.pushFailed(ctx, p, true, "the Controller does not accept the credentials of this Worker")
		}
	}
	e.log.DebugContext(ctx, "asking for a token failed; it will be tried again", "run", p.RunID, "err", err)
	e.later(p)
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
		e.later(p)
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

func (e *Executor) forgetBackoff(k state.Key) {
	e.backoffMu.Lock()
	defer e.backoffMu.Unlock()
	delete(e.pubMemos, k)
}

// later keeps the publisher from spinning on a publication that cannot move
// now. Nothing is written: the persisted time is only for tries that were made.
func (e *Executor) later(p state.Publication) {
	wait := e.nextBackoff(p.Key)
	e.backoffMu.Lock()
	defer e.backoffMu.Unlock()
	e.pubMemos[p.Key].notBefore = e.clock.Now().Add(wait)
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
// so. A view of another lease, or of a Run that is not waiting for it, changes
// nothing.
func (e *Executor) ConfirmView(ctx context.Context, tx *state.Tx, k state.Key, run v1.Run) error {
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
	_, err := state.ConfirmPublication(ctx, tx, k, to, e.clock.Now())
	return err
}
