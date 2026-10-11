package executor

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	cfruntime "github.com/Autarquico/codefabric/internal/adapters/runtime"

	"github.com/Autarquico/codefabric/internal/adapters/harness"
	v1 "github.com/Autarquico/codefabric/internal/api/v1"
	"github.com/Autarquico/codefabric/internal/domain"
	"github.com/Autarquico/codefabric/internal/worker/execwrap"
	"github.com/Autarquico/codefabric/internal/worker/procident"
	"github.com/Autarquico/codefabric/internal/worker/state"
)

const paneReady = "pane.ready"

// step moves a Run one step along its life and reports whether it is over. It
// is what the supervisor does at every tick, and what recovery does first: the
// state it looks at is the one on disk, so it makes no difference whether the
// daemon has been here before.
func (e *Executor) step(ctx context.Context, k state.Key) (done bool) {
	r, err := state.RunBy(ctx, e.o.DB.Reader(), k)
	if err != nil {
		e.log.ErrorContext(ctx, "reading a Run", "run", k.RunID, "err", err)
		return false
	}
	if !r.Phase.Live() {
		e.forget(k)
		return true
	}
	var prepErr error
	if r.Phase == state.PhaseAccepted {
		// Preparing may take long (it will clone), so it is outside the lock; the
		// phase is checked again inside it.
		if prepErr = e.prepare(ctx, r); prepErr != nil {
			e.log.ErrorContext(ctx, "preparing a Run", "run", k.RunID, "err", prepErr)
		}
	}
	unlock := e.lock(k)
	defer unlock()
	if r, err = state.RunBy(ctx, e.o.DB.Reader(), k); err != nil || !r.Phase.Live() {
		if err == nil {
			e.forget(k)
		}
		return err == nil
	}
	switch r.Phase {
	case state.PhaseAssigned:
		return true // not ours to supervise yet: the decider answers it
	case state.PhaseAccepted:
		if prepErr != nil {
			return e.settleNeverRan(ctx, k, "preparing the Run failed: "+prepErr.Error())
		}
		return errors.Is(e.begin(ctx, r), ErrAborted)
	case state.PhaseStarting, state.PhaseRunning, state.PhaseStopping:
		return e.poll(ctx, r)
	case state.PhaseStopUnverified:
		if e.retryDue(k) {
			return e.poll(ctx, r)
		}
	}
	return false
}

func (e *Executor) noteFailure(k state.Key, err error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.memo(k).lastErr = err.Error()
}

func mkdir(dir string) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("making %s: %w", dir, err)
	}
	return nil
}

func (e *Executor) retryDue(k state.Key) bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	return !e.clock.Now().Before(e.memo(k).retryAt)
}

// prepare gives the Run what it runs with: its directory, its worktree, the
// script the harness makes of its prompt, and launch.json for the wrapper.
func (e *Executor) prepare(ctx context.Context, r state.Run) error {
	spec := parseSpec(r.Spec)
	launcher, ok := e.harnesses[spec.Requirements.Harness]
	if !ok {
		return fmt.Errorf("this Worker has no harness %q", spec.Requirements.Harness)
	}
	dir, worktree := e.runDir(r.Key), e.o.Paths.WorkerWorktreeDir(r.RunID, r.LeaseEpoch)
	if err := mkdir(dir); err != nil {
		return err
	}
	if err := e.o.Preparer(ctx, r, worktree); err != nil {
		return err
	}
	launch, err := launcher.Launch(harness.LaunchSpec{RunDir: dir, Worktree: worktree, Prompt: spec.Prompt, Mode: domain.Mode(spec.Mode)})
	if err != nil {
		return err
	}
	l := execwrap.Launch{Argv: launch.Argv, Env: launch.Env, Dir: worktree, Mode: spec.Mode, Grace: e.grace}
	if spec.Mode == execwrap.ModeInteractive {
		l.Gate = filepath.Join(dir, paneReady)
	}
	return execwrap.Write(dir, execwrap.LaunchFile, l)
}

// begin takes a Run from accepted to running: the intention first (phase
// starting), then the Runtime, then the handle with the report that the harness
// started, in one transaction. It runs under the lock of the Run, so that a
// stop is either before all of it or after all of it.
func (e *Executor) begin(ctx context.Context, r state.Run) error {
	now := e.clock.Now()
	if err := e.o.DB.WithTx(ctx, func(tx *state.Tx) error {
		return state.Advance(ctx, tx, r.Key, state.PhaseAccepted, state.PhaseStarting, now)
	}); err != nil {
		if errors.Is(err, state.ErrChanged) {
			return ErrAborted
		}
		return err
	}
	r.Phase = state.PhaseStarting
	return e.startSession(ctx, r)
}

// startSession calls the Runtime, which adopts the session if it exists, and
// records that the harness started. The caller holds the lock and the Run is
// in phase starting.
func (e *Executor) startSession(ctx context.Context, r state.Run) error {
	if h := e.o.Hooks.BeforeRuntimeStart; h != nil {
		h(r)
	}
	spec := e.sessionSpec(r)
	handle, err := e.o.Runtime.Start(ctx, spec)
	if err != nil {
		e.noteFailure(r.Key, err)
		e.log.WarnContext(ctx, "the Runtime could not start the session", "run", r.RunID, "err", err)
		// Whatever it left would be adopted by the next try: it ends here.
		_ = e.o.Runtime.Stop(ctx, e.o.Runtime.HandleFor(spec.Name))
		return err
	}
	if h := e.o.Hooks.AfterRuntimeStart; h != nil {
		h(r)
	}
	now := e.clock.Now()
	raw := mustJSON(v1.RuntimeHandle{Runtime: handle.Runtime, Ref: handle.Ref})
	err = e.o.DB.WithTx(ctx, func(tx *state.Tx) error {
		if err := state.SetRunning(ctx, tx, r.Key, raw, now); err != nil {
			return err
		}
		return e.report(ctx, tx, r.Key, v1.ReportStarted, now, func(en *v1.RunReportEntry) {
			en.Handle = &v1.RuntimeHandle{Runtime: handle.Runtime, Ref: handle.Ref}
		})
	})
	if err != nil {
		return err
	}
	e.o.Journal.Notify()
	return nil
}

// sessionSpec is the session of a Run: the same whenever it is asked for.
func (e *Executor) sessionSpec(r state.Run) cfruntime.Spec {
	dir := e.runDir(r.Key)
	spec := cfruntime.Spec{
		Name: e.sessionName(r.Key), Dir: dir,
		Command: append(append([]string{}, e.wrapper...), "--run-dir", dir),
	}
	if parseSpec(r.Spec).Mode == execwrap.ModeInteractive {
		spec.Capture, spec.Ready = filepath.Join(dir, execwrap.OutputLog), filepath.Join(dir, paneReady)
	}
	return spec
}

// poll looks at a Run that has been started and does what the evidence on disk
// calls for. The session is read before the markers: if it is gone by then, the
// markers are everything it ever left.
func (e *Executor) poll(ctx context.Context, r state.Run) (done bool) {
	alive, err := e.sessionAlive(ctx, r.Key)
	if err != nil {
		e.log.WarnContext(ctx, "asking the Runtime about a session failed", "run", r.RunID, "err", err)
		return false
	}
	m, err := e.markers(r.Key)
	if err != nil {
		e.log.ErrorContext(ctx, "reading the markers of a Run", "run", r.RunID, "err", err)
		return false
	}
	if h := e.o.Hooks.AfterMarkers; h != nil {
		h(r)
	}
	if r.Phase == state.PhaseStopping && m.exit == nil && e.firstLook(r.Key) {
		// A stop the previous daemon began: the signal may not have been sent.
		e.signalStop(ctx, r, m)
	}
	switch {
	case m.exit != nil:
		return e.settleExit(ctx, r, m)
	case m.wrapper != nil:
		return e.pollWrapper(ctx, r, m)
	case alive:
		// The wrapper writes its marker as the first thing it does.
		if e.clock.Now().Sub(r.UpdatedAt) > noWrapperLimit {
			e.log.WarnContext(ctx, "the session has no wrapper after too long: ending it", "run", r.RunID)
			_ = e.o.Runtime.Stop(ctx, e.o.Runtime.HandleFor(e.sessionName(r.Key)))
		}
		return false
	case r.Phase == state.PhaseStarting:
		return e.restart(ctx, r)
	}
	return e.settleNeverRan(ctx, r.Key, e.lastError(r.Key, "the session ended before its wrapper started"))
}

// firstLook is true once per daemon for a Run.
func (e *Executor) firstLook(k state.Key) bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	m := e.memo(k)
	if m.signaled {
		return false
	}
	m.signaled = true
	return true
}

// firstUnresolved is true the first time a Run's wrapper cannot be identified,
// until it can be again.
func (e *Executor) firstUnresolved(k state.Key) bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	m := e.memo(k)
	first := !m.unresolved
	m.unresolved = true
	return first
}

func (e *Executor) resolved(k state.Key) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if m := e.memos[k]; m != nil {
		m.unresolved = false
	}
}

func (e *Executor) lastError(k state.Key, fallback string) string {
	e.mu.Lock()
	defer e.mu.Unlock()
	if msg := e.memo(k).lastErr; msg != "" {
		return msg
	}
	return fallback
}

// restart is recovery of a Run in phase starting with no trace of its session:
// the daemon died between writing the intention and the effect. Start adopts a
// session if one is there. A Runtime that keeps failing ends the Run.
func (e *Executor) restart(ctx context.Context, r state.Run) (done bool) {
	err := e.startSession(ctx, r)
	if err == nil {
		return false
	}
	e.mu.Lock()
	m := e.memo(r.Key)
	m.failures++
	n := m.failures
	e.mu.Unlock()
	if n >= startAttempts {
		return e.settleNeverRan(ctx, r.Key, fmt.Sprintf("the Runtime could not start the session: %v", err))
	}
	return false
}

func (e *Executor) sessionAlive(ctx context.Context, k state.Key) (bool, error) {
	st, err := e.o.Runtime.Status(ctx, e.o.Runtime.HandleFor(e.sessionName(k)))
	return st.Alive, err
}

type markers struct {
	wrapper *execwrap.Wrapper
	started *execwrap.Started
	exit    *execwrap.Exit
}

func (e *Executor) markers(k state.Key) (m markers, err error) {
	dir := e.runDir(k)
	if m.exit, err = readMarker[execwrap.Exit](dir, execwrap.ExitFile); err != nil {
		return markers{}, err
	}
	if m.wrapper, err = readMarker[execwrap.Wrapper](dir, execwrap.WrapperFile); err != nil {
		return markers{}, err
	}
	if m.started, err = readMarker[execwrap.Started](dir, execwrap.StartedFile); err != nil {
		return markers{}, err
	}
	return m, nil
}

// readMarker returns nil for a marker that does not exist.
func readMarker[T any](dir, name string) (*T, error) {
	var v T
	ok, err := execwrap.Read(dir, name, &v)
	if err != nil || !ok {
		return nil, err
	}
	return &v, nil
}

// pollWrapper watches a Run whose wrapper exists and has not written an exit.
func (e *Executor) pollWrapper(ctx context.Context, r state.Run, m markers) (done bool) {
	switch v := verdictOf(m.wrapper); v {
	case procident.Same:
		e.resolved(r.Key)
		switch r.Phase {
		case state.PhaseStarting:
			return e.adopt(ctx, r, m)
		case state.PhaseStopping:
			return e.escalateStop(ctx, r, m)
		case state.PhaseStopUnverified:
			// It can be verified now: the stop goes on from where it was, if one was ordered.
			if _, err := state.StopOrderOf(ctx, e.o.DB.Reader(), r.Key); err != nil {
				return false
			}
			if err := e.o.DB.WithTx(ctx, func(tx *state.Tx) error {
				return state.Advance(ctx, tx, r.Key, state.PhaseStopUnverified, state.PhaseStopping, e.clock.Now())
			}); err == nil {
				r.Phase = state.PhaseStopping
				e.signalStop(ctx, r, m)
			}
		}
		return false
	case procident.Gone, procident.Rebooted, procident.Different:
		e.resolved(r.Key)
		return e.settleLost(ctx, r, m, v)
	default:
		return e.unresolved(ctx, r, "the identity of the wrapper cannot be verified")
	}
}

// adopt records, for a Run found in phase starting with its wrapper alive, that
// the session is running: what the daemon that died would have recorded.
func (e *Executor) adopt(ctx context.Context, r state.Run, m markers) (done bool) {
	// The daemon may have died between creating the session and wiring it: the
	// wiring is finished before the Run is taken as started, without a second Start.
	if err := e.o.Runtime.Wire(ctx, e.sessionSpec(r)); err != nil {
		e.log.WarnContext(ctx, "wiring an adopted session failed; it will be tried again", "run", r.RunID, "err", err)
		return false
	}
	at := e.clock.Now()
	if m.started != nil {
		at = m.started.StartedAt
	}
	raw := mustJSON(*e.runtimeHandle(r.Key))
	err := e.o.DB.WithTx(ctx, func(tx *state.Tx) error {
		if err := state.SetRunning(ctx, tx, r.Key, raw, at); err != nil {
			return err
		}
		return e.report(ctx, tx, r.Key, v1.ReportStarted, at, func(en *v1.RunReportEntry) { en.Handle = e.runtimeHandle(r.Key) })
	})
	if err != nil {
		e.log.ErrorContext(ctx, "adopting a session", "run", r.RunID, "err", err)
		return false
	}
	e.o.Journal.Notify()
	return false
}

func mustJSON(v any) string {
	raw, err := json.Marshal(v)
	if err != nil {
		panic(err) // a value of ours that cannot be encoded is a bug
	}
	return string(raw)
}
