package executor

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"slices"
	"strings"
	"syscall"
	"time"

	"github.com/Autarquico/codefabric/internal/adapters/harness"
	v1 "github.com/Autarquico/codefabric/internal/api/v1"
	"github.com/Autarquico/codefabric/internal/config"
	"github.com/Autarquico/codefabric/internal/domain"
	"github.com/Autarquico/codefabric/internal/worker/gitops"
	"github.com/Autarquico/codefabric/internal/worker/procident"
	"github.com/Autarquico/codefabric/internal/worker/state"
)

// ReasonCheckpoint is the reason of the stop the Worker orders to quiesce the
// harness for a capture: the harness that ends by it ends with the cause stop.
const ReasonCheckpoint = "checkpoint"

// captureWindow is how long, from the order, the Worker keeps trying a capture
// that fails for what is not its fault (the Controller or the relay out of
// reach) before it gives up and settles the Run, on top of the checkpoint.grace
// that quiescing may take. The Controller times out a capture that never reports.
const captureWindow = 15 * time.Minute

// CheckpointClient is what capturing a checkpoint needs of the Controller, besides
// Controller. *client.Client implements it; a Controller that does not is one
// whose Worker cannot capture, and the order is refused.
type CheckpointClient interface {
	CheckpointToken(ctx context.Context, run string, req v1.CheckpointTokenRequest) (v1.RelayTokenResponse, error)
	CompleteCheckpoint(ctx context.Context, run, checkpoint string, req v1.CheckpointCompleteRequest) (v1.CheckpointReported, error)
	FailCheckpoint(ctx context.Context, run, checkpoint string, req v1.CheckpointFailRequest) (v1.CheckpointReported, error)
	PutNativeState(ctx context.Context, run, checkpoint string, leaseEpoch uint64, digest string, r io.Reader, size int64) (v1.NativeStateUploaded, error)
}

// refusal is a checkpoint order this Worker cannot carry out, which it tells
// the Controller once the order is persisted: the harness is alive and nothing
// was touched.
type refusal struct {
	cmd v1.CheckpointCommand
	why string
}

// persistCheckpoint takes a checkpoint order, in the transaction of the cursor.
// A Run that can be quiesced goes to checkpointing with its capture and a stop
// of the Worker, together, so a daemon that dies after the acknowledgement knows
// what it was doing; quiesce is for after the commit. A Run that cannot is
// refused, with the harness alive. An order for a Run that is not running (it
// is already stopping, or over) or for a lease that is not the Run's is only
// acknowledged: the exit, the stop or the loss of the Run is what the Controller
// arbitrates, and it aborts the capture it reserved.
func (e *Executor) persistCheckpoint(ctx context.Context, tx *state.Tx, c v1.WorkerCommand) (quiesce *state.Key, refused *refusal, err error) {
	o := c.Checkpoint
	k := state.Key{RunID: o.RunID, LeaseEpoch: o.Epoch}
	if err := state.SetCursor(ctx, tx, c.Seq); err != nil {
		return nil, nil, err
	}
	r, err := state.RunBy(ctx, tx, k)
	if errors.Is(err, state.ErrNotFound) {
		e.log.WarnContext(ctx, "a checkpoint order names a Run this Worker has no record of; it is ignored", "run", o.RunID, "epoch", o.Epoch)
		return nil, nil, nil
	}
	if err != nil {
		return nil, nil, err
	}
	if r.LeaseID != o.LeaseID || r.Phase != state.PhaseRunning && r.Phase != state.PhaseStarting {
		e.log.InfoContext(ctx, "a checkpoint order for a Run that is not running is ignored", "run", o.RunID, "phase", r.Phase)
		return nil, nil, nil
	}
	if why := e.cannotQuiesce(r); why != "" {
		return nil, &refusal{cmd: *o, why: why}, nil
	}
	now := e.clock.Now()
	exclude := o.Exclude
	if len(exclude) == 0 {
		exclude = config.DefaultCheckpointExclude()
	}
	created, err := state.BeginCapture(ctx, tx, state.Capture{
		Key: k, CheckpointID: o.CheckpointID, Trigger: string(o.Trigger), Ref: o.Ref, Exclude: exclude, NativeState: o.NativeState, CreatedAt: now,
	}, r.Phase)
	if err != nil || !created {
		return nil, nil, err
	}
	order := e.stopOrder(k, ReasonCheckpoint, SourceWorker, now)
	order.WasRunning = true
	if _, err := state.AddStopOrder(ctx, tx, order); err != nil {
		return nil, nil, err
	}
	return &k, nil, nil
}

// cannotQuiesce says why this Worker cannot capture the Run, "" if it can: it
// needs the harness running under a wrapper it can identify (the one process it
// can stop and wait for), git, and a Controller that takes the report.
func (e *Executor) cannotQuiesce(r state.Run) string {
	if _, ok := e.o.Controller.(CheckpointClient); !ok || e.o.Git == nil {
		return "this Worker cannot capture checkpoints"
	}
	m := e.markers(r.Key)
	switch {
	case r.Phase != state.PhaseRunning || m.started == nil || m.exit != nil:
		return "the harness is not running"
	case m.wrapper == nil || m.wrapperErr != nil || verdictOf(m.wrapper) != procident.Same:
		return "the harness cannot be stopped: its wrapper is not the process it was"
	}
	return ""
}

// quiesce stops the harness of a Run that went to checkpointing: the signal
// that ends the group, which the wrapper follows with a kill after stop.grace
// and the supervisor, if the group outlives checkpoint.grace, with its own. The
// capture is made when the group is gone (settleExit), never before.
func (e *Executor) quiesce(ctx context.Context, k state.Key) {
	unlock := e.lock(k)
	defer unlock()
	r, err := state.RunBy(ctx, e.o.DB.Reader(), k)
	if err != nil || r.Phase != state.PhaseCheckpointing {
		return
	}
	e.mu.Lock()
	e.memo(k).signaled = true
	e.mu.Unlock()
	e.signalStop(ctx, r, e.markers(k))
	if e.spawn != nil {
		e.spawn(k)
	}
}

// refuse tells the Controller that a capture was not started, with the harness alive.
func (e *Executor) refuse(ctx context.Context, f refusal) {
	cc, ok := e.o.Controller.(CheckpointClient)
	if !ok {
		e.log.WarnContext(ctx, "a checkpoint order was refused and this Worker cannot say so", "run", f.cmd.RunID, "why", f.why)
		return
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	_, err := cc.FailCheckpoint(ctx, f.cmd.RunID, f.cmd.CheckpointID, v1.CheckpointFailRequest{LeaseEpoch: f.cmd.Epoch, Reason: reportable(f.why), HarnessAlive: true})
	if err != nil && ctx.Err() == nil {
		e.log.WarnContext(ctx, "telling the Controller that a capture was refused failed", "run", f.cmd.RunID, "err", err)
	}
}

// cancelCapture cuts short the capture of a Run that is in progress, if any.
func (e *Executor) cancelCapture(k state.Key) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if m := e.memos[k]; m != nil && m.cancelCapture != nil {
		m.cancelCapture()
	}
}

// escalateCheckpoint kills the group of a Run that was asked to quiesce and has
// not ended checkpoint.grace later, as escalateStop does for a stop.
func (e *Executor) escalateCheckpoint(ctx context.Context, r state.Run, m markers) {
	c, err := state.CaptureOf(ctx, e.o.DB.Reader(), r.Key)
	if err != nil || c.DoneAt != nil || e.clock.Now().Before(c.CreatedAt.Add(e.checkpointGrace+stopMargin)) {
		return
	}
	e.log.WarnContext(ctx, "quiescing did not finish within checkpoint.grace: killing the group", "run", r.RunID)
	if verdictOf(m.wrapper) == procident.Same {
		_ = syscall.Kill(-m.wrapper.PGID, syscall.SIGKILL)
	}
	_ = e.o.Runtime.Stop(ctx, e.o.Runtime.HandleFor(e.sessionName(r.Key)))
}

// capturedBeforeSettle makes the capture of a Run in checkpointing, now that its
// group is gone, and says whether the Run may be settled. Settling removes the
// ConfigDir with the session of the harness, so it waits for the capture to be
// reported (or abandoned): false means come back at the next tick.
func (e *Executor) capturedBeforeSettle(ctx context.Context, r state.Run) (settle bool) {
	c, err := state.CaptureOf(ctx, e.o.DB.Reader(), r.Key)
	if errors.Is(err, state.ErrNotFound) || err == nil && c.DoneAt != nil {
		return true
	}
	if err != nil {
		e.log.ErrorContext(ctx, "reading the capture of a Run", "run", r.RunID, "err", err)
		return false
	}
	cctx, cancel := context.WithCancel(ctx)
	e.mu.Lock()
	e.memo(r.Key).cancelCapture = cancel
	e.mu.Unlock()
	defer func() {
		cancel()
		e.mu.Lock()
		e.memo(r.Key).cancelCapture = nil
		e.mu.Unlock()
	}()
	outcome, retry := e.capture(cctx, r, c)
	if retry {
		if e.clock.Now().Before(c.CreatedAt.Add(e.checkpointGrace + captureWindow)) {
			return false
		}
		e.log.ErrorContext(ctx, "a capture could not be reported in time: the Run is settled without it", "run", r.RunID, "checkpoint", c.CheckpointID)
		outcome = state.CaptureFailed
	}
	if _, err := e.endCapture(ctx, r.Key, outcome); err != nil {
		e.log.ErrorContext(ctx, "closing the capture of a Run", "run", r.RunID, "err", err)
		return false
	}
	return true
}

func (e *Executor) endCapture(ctx context.Context, k state.Key, outcome string) (bool, error) {
	var ended bool
	err := e.o.DB.WithTx(ctx, func(tx *state.Tx) (err error) {
		ended, err = state.EndCapture(ctx, tx, k, outcome, e.clock.Now())
		return err
	})
	return ended, err
}

// capture takes the worktree of a Run whose harness is gone, pushes it to the
// ref of the checkpoint, exports the native state of the harness and reports
// the result. Its outcome is what was reported ("" when nothing was) and retry
// says it is to be tried again: the Controller or the relay did not answer, and
// every step can be repeated.
func (e *Executor) capture(ctx context.Context, r state.Run, c state.Capture) (outcome string, retry bool) {
	cc, ok := e.o.Controller.(CheckpointClient)
	if !ok || e.o.Git == nil {
		return state.CaptureAborted, false // it was checked when the order came; a daemon without them has nothing to say
	}
	fail := func(why string) (string, bool) {
		if ctx.Err() != nil {
			return "", true // cut short: what failed is not the capture's
		}
		return e.failCapture(ctx, cc, r, c, why)
	}

	forbidden, err := e.forbidden(r)
	if err != nil {
		return fail("the capture cannot be checked for secrets: " + forbidden.Redact(err.Error()))
	}
	head, err := e.o.Git.Head(ctx, e.worktree(r.Key))
	if err != nil {
		return fail("HEAD of the worktree cannot be read: " + forbidden.Redact(err.Error()))
	}
	sealed, err := e.o.Git.Snapshot(ctx, e.worktree(r.Key), gitops.SnapshotIntent{
		Project: r.ProjectID, ParentOID: head, At: c.CreatedAt, Exclude: c.Exclude,
	}, forbidden)
	if err != nil {
		if ctx.Err() != nil {
			return "", true
		}
		return fail("capturing the worktree failed: " + forbidden.Redact(err.Error()))
	}
	tok, err := cc.CheckpointToken(ctx, r.RunID, v1.CheckpointTokenRequest{LeaseEpoch: r.LeaseEpoch, CheckpointID: c.CheckpointID, Commit: sealed.Commit})
	if err != nil {
		return e.afterControllerError(ctx, r, err, fail)
	}
	if tok.Ref != c.Ref {
		return fail("the token is for another ref than the checkpoint's")
	}
	pushErr := e.o.Git.PushRef(ctx, r.ProjectID, sealed.Commit, c.Ref, gitops.PushAuth{
		URL: e.o.Controller.RelayURL(tok.Path), Token: tok.Token,
		Cert: e.o.RelayTLS.Cert, Key: e.o.RelayTLS.Key, CA: e.o.RelayTLS.CA,
	}, forbidden)
	if ctx.Err() != nil {
		return "", true
	}
	if pushErr != nil {
		var pe *gitops.PushError
		if errors.As(pushErr, &pe) && pe.Class == gitops.PushUnreachable {
			e.log.WarnContext(ctx, "pushing a checkpoint failed; it will be tried again", "run", r.RunID, "err", pe.Message)
			return "", true
		}
		return fail("pushing the capture failed: " + forbidden.Redact(briefErr(pushErr)))
	}
	native, warnings, retry := e.exportNativeState(ctx, cc, r, c, forbidden)
	if retry {
		return "", true
	}
	_, err = cc.CompleteCheckpoint(ctx, r.RunID, c.CheckpointID, v1.CheckpointCompleteRequest{
		LeaseEpoch: r.LeaseEpoch, Commit: sealed.Commit, NativeState: native, Excluded: sealed.Excluded, Warnings: warnings,
	})
	if err != nil {
		return e.afterControllerError(ctx, r, err, fail)
	}
	e.log.InfoContext(ctx, "checkpoint captured", "run", r.RunID, "checkpoint", c.CheckpointID, "commit", sealed.Commit, "excluded", len(sealed.Excluded), "native_state", native != nil)
	return state.CaptureCompleted, false
}

// afterControllerError is what an error of the Controller means for a capture: a
// Controller that does not know the capture any more, or no longer takes this
// lease, ended it (the Run exited, was cancelled or lost), which is not for
// this Worker to report; one that refused the request says why, and that is a
// failed capture; any other error is a Controller out of reach.
func (e *Executor) afterControllerError(ctx context.Context, r state.Run, err error, fail func(string) (string, bool)) (string, bool) {
	if ctx.Err() != nil {
		return "", true
	}
	var coded *domain.Error
	if errors.As(err, &coded) {
		switch coded.Code {
		case domain.CodeNotFound, domain.CodePolicyDenied, domain.CodeStaleLease, domain.CodeInvalidTransition:
			e.log.InfoContext(ctx, "the Controller ended the capture of a Run", "run", r.RunID, "code", coded.Code)
			return state.CaptureAborted, false
		case domain.CodeInvalidRequest:
			return fail("the Controller refused the capture: " + coded.Message)
		}
	}
	e.log.WarnContext(ctx, "the Controller did not take a capture; it will be tried again", "run", r.RunID, "err", err)
	return "", true
}

// failCapture tells the Controller that the capture failed with the harness
// gone: the Run ends failed there, and keeps its worktree for another try.
func (e *Executor) failCapture(ctx context.Context, cc CheckpointClient, r state.Run, c state.Capture, why string) (string, bool) {
	e.log.WarnContext(ctx, "a capture failed", "run", r.RunID, "checkpoint", c.CheckpointID, "why", why)
	_, err := cc.FailCheckpoint(ctx, r.RunID, c.CheckpointID, v1.CheckpointFailRequest{LeaseEpoch: r.LeaseEpoch, Reason: reportable(why), HarnessAlive: false})
	if err == nil {
		return state.CaptureFailed, false
	}
	if ctx.Err() != nil {
		return "", true
	}
	var coded *domain.Error
	if errors.As(err, &coded) && coded.Code != domain.CodeInternal && coded.Code != domain.CodeUnauthenticated && coded.Code != domain.CodeControllerUnreachable {
		return state.CaptureAborted, false
	}
	return "", true
}

// exportNativeState uploads the session of the harness of the Run, if its
// adapter can export it (harness.SessionPorter) and the Run has one, and
// describes it for the report. It never makes the capture fail: a session that
// cannot be exported, that is too large, or that holds a value of a secret of
// the Run is left out with a warning in the log (names and lines, never values),
// and the checkpoint is source and summary. retry says the upload did not get
// through and is to be tried again.
func (e *Executor) exportNativeState(ctx context.Context, cc CheckpointClient, r state.Run, c state.Capture, forbidden gitops.Forbidden) (ref *v1.NativeStateRef, warnings []string, retry bool) {
	if !c.NativeState {
		return nil, nil, false
	}
	spec := parseSpec(r.Spec)
	adapter, ok := e.harnesses.Get(spec.Requirements.Harness)
	if !ok {
		return nil, nil, false
	}
	porter, ok := adapter.(harness.SessionPorter)
	if !ok || r.SessionID == "" {
		return nil, nil, false
	}
	h, err := e.handleOf(adapter, r, spec)
	if err != nil {
		e.log.WarnContext(ctx, "the native state of a Run is left out of its checkpoint: its handle cannot be rebuilt", "run", r.RunID, "err", err)
		return nil, nil, false
	}
	sref := harness.SessionRef{ConfigDir: h.ConfigDir, Cwd: h.Worktree, SessionID: h.SessionID}
	leave := func(why string) (*v1.NativeStateRef, []string, bool) {
		e.log.WarnContext(ctx, "the native state of a Run is left out of its checkpoint: "+why, "run", r.RunID, "checkpoint", c.CheckpointID, "adapter", adapter.ID())
		return nil, nil, false
	}
	files, err := porter.Locate(ctx, sref)
	if err != nil {
		return leave("its session was not found: " + forbidden.Redact(err.Error()))
	}
	if why := e.sessionHoldsSecrets(files, forbidden); why != "" {
		ref, _, retry := leave(why)
		return ref, []string{"Secret in the session: " + why + "; the native state was left out of the checkpoint"}, retry
	}
	tmp, err := os.CreateTemp(e.o.Paths.WorkerDataDir(), "native-")
	if err != nil {
		return leave("a file for it could not be made: " + brief(err))
	}
	defer func() { _ = tmp.Close(); _ = os.Remove(tmp.Name()) }()
	sum := sha256.New()
	manifest, err := porter.Export(ctx, sref, harness.ExportOptions{}, io.MultiWriter(tmp, sum))
	if err != nil {
		if ctx.Err() != nil {
			return nil, nil, true
		}
		return leave("exporting its session failed: " + forbidden.Redact(err.Error()))
	}
	size, err := tmp.Seek(0, io.SeekCurrent)
	if err != nil {
		return leave("its size is unknown: " + brief(err))
	}
	if _, err := tmp.Seek(0, io.SeekStart); err != nil {
		return leave("it cannot be read back: " + brief(err))
	}
	digest := "sha256:" + hex.EncodeToString(sum.Sum(nil))
	if _, err := cc.PutNativeState(ctx, r.RunID, c.CheckpointID, r.LeaseEpoch, digest, tmp, size); err != nil {
		var coded *domain.Error
		if errors.As(err, &coded) && coded.Code != domain.CodeInternal && coded.Code != domain.CodeUnauthenticated && coded.Code != domain.CodeControllerUnreachable {
			return leave("the Controller refused it: " + coded.Message)
		}
		e.log.WarnContext(ctx, "uploading the native state of a Run failed; it will be tried again", "run", r.RunID, "err", err)
		return nil, nil, true
	}
	return &v1.NativeStateRef{Adapter: adapter.ID(), AdapterVersion: manifest.Version, Digest: digest}, nil, false
}

// sessionHoldsSecrets reads every file of the session for the values of the
// secrets of the Run, and says, with the names of the files and the numbers of
// the lines, what it found; "" if nothing. The files cannot be redacted (a
// signed block would break), so a finding leaves the session out whole. What
// cannot be read to the end is a finding too: it could not be checked.
func (e *Executor) sessionHoldsSecrets(files harness.SessionFiles, forbidden gitops.Forbidden) string {
	var found []string
	for _, path := range slices.Concat([]string{files.Transcript}, files.Attachments) {
		f, err := os.Open(path) //nolint:gosec // a file of the session the adapter located
		if err != nil {
			return "it cannot be checked for secrets: " + brief(err)
		}
		lines, err := gitops.LinesWithSecrets(f, forbidden)
		_ = f.Close()
		if err != nil {
			return "it cannot be checked for secrets: " + forbidden.Redact(err.Error())
		}
		if len(lines) > 0 {
			found = append(found, fmt.Sprintf("%s lines %s", baseName(path), joinInts(lines)))
		}
	}
	if len(found) == 0 {
		return ""
	}
	return "it holds the value of a secret of the Run (" + strings.Join(found, "; ") + ")"
}

func baseName(path string) string { return path[strings.LastIndexByte(path, os.PathSeparator)+1:] }

// joinInts lists the numbers, at most a few: a transcript that holds a secret
// holds it where it was printed, and the count says how bad it is.
func joinInts(ns []int) string {
	const most = 20
	parts := make([]string, 0, min(len(ns), most)+1)
	for _, n := range ns[:min(len(ns), most)] {
		parts = append(parts, fmt.Sprint(n))
	}
	if len(ns) > most {
		parts = append(parts, fmt.Sprintf("and %d more", len(ns)-most))
	}
	return strings.Join(parts, ",")
}

// validCheckpointOrder says that an order names a checkpoint and the ref that
// is its own: anything else would push a Run's work to a ref of someone else's.
func validCheckpointOrder(o *v1.CheckpointCommand) bool {
	kind, _, err := domain.ParseID(o.CheckpointID)
	return err == nil && kind == domain.KindCheckpoint && o.Ref == domain.CheckpointRef(domain.ID(o.CheckpointID))
}
