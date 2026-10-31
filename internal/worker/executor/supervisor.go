package executor

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"

	cfruntime "github.com/Autarquico/codefabric/internal/adapters/runtime"

	"github.com/Autarquico/codefabric/internal/adapters/harness"
	v1 "github.com/Autarquico/codefabric/internal/api/v1"
	"github.com/Autarquico/codefabric/internal/domain"
	"github.com/Autarquico/codefabric/internal/worker/execwrap"
	"github.com/Autarquico/codefabric/internal/worker/host"
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
	// An accepted Run whose Environment supervisor was launched is prepared: its
	// prepare would make the worktree again under the scripts that ran in it. A
	// record that cannot be read is not "none": the step waits for the next tick.
	launched := false
	if r.Phase == state.PhaseAccepted {
		if launched, err = e.hasEnvironment(ctx, k); err != nil {
			e.log.ErrorContext(ctx, "reading the Environment of a Run", "run", k.RunID, "err", err)
			return false
		}
	}
	if r.Phase == state.PhaseAccepted && (r.FencedCode != "" || (!launched && !e.prepareDue(k))) {
		// A fenced Run waits for the reconciliation; one whose prepare could not
		// finish waits for its retry.
		return false
	}
	if r.Phase == state.PhaseAccepted && !launched {
		// Preparing may take long (it will clone), so it is outside the lock; the
		// phase is checked again inside it, and before every effect on secrets.
		if prepErr = e.preparing(ctx, r); errors.Is(prepErr, errAckPending) {
			e.log.DebugContext(ctx, "preparing a Run", "run", k.RunID, "wait", prepErr)
		} else if prepErr != nil {
			e.log.ErrorContext(ctx, "preparing a Run", "run", k.RunID, "err", prepErr)
		}
		switch {
		case errors.Is(prepErr, errPrepareFenced):
			e.fenceForGrant(ctx, k)
			e.postponePrepare(k)
			return false
		case errors.Is(prepErr, errAckPending):
			e.postponePrepareFor(k, ackPoll)
			return false
		case errors.Is(prepErr, errPrepareLater):
			e.postponePrepare(k)
			return false
		case prepErr == nil:
			e.preparedNow(k)
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
			var coded *codedFailure
			if errors.As(prepErr, &coded) {
				return e.settleNeverRanWith(ctx, k, facts{message: coded.msg, code: coded.code})
			}
			return e.settleNeverRan(ctx, k, "preparing the Run failed: "+prepErr.Error())
		}
		return e.beginOrEnvironment(ctx, r)
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

// prepare gives the Run what it runs with: its directory, its worktree, its
// configuration when the harness reads one per Run, what the harness makes of
// the prompt, and launch.json for the wrapper. It runs only while the Run is
// accepted: once it is starting, the harness may be writing to its
// configuration, and nothing here is done again.
//
// A Run with secrets asks the Controller for them first, under a new prepare
// attempt, before anything is cloned; the values are read and delivered last,
// once the environment of the harness is known, just before launch.json.
func (e *Executor) prepare(ctx context.Context, r state.Run) error {
	spec := parseSpec(r.Spec)
	adapter, ok := e.harnesses.Get(spec.Requirements.Harness)
	if !ok {
		return fmt.Errorf("this Worker has no harness %q", spec.Requirements.Harness)
	}
	if spec.Schema == domain.RunSpecSchemaRepoConfig {
		// Before anything is made for the Run, or any secret asked for.
		if err := e.resolveRepoConfig(ctx, r, spec); err != nil {
			return err
		}
	}
	if err := checkSpecSecrets(spec); err != nil {
		return fmt.Errorf("%s: the secrets of the Run: %w", domain.CodeInvalidRequest, err)
	}
	var (
		snap    harness.NativeSnapshot
		cfgRefs []string
	)
	if adapter.Capabilities().ConfigIsolation {
		var err error
		if snap, cfgRefs, err = e.configOf(ctx, r, adapter); err != nil {
			return err
		}
	}
	var sp *secretPrepare
	attempt := func() (*secretPrepare, error) {
		if sp == nil {
			var err error
			sp, err = e.beginSecrets(ctx, r)
			return sp, err
		}
		return sp, nil
	}
	// The secrets of the configuration are asked for with the rest. The
	// Controller does not know them from the spec yet: it denies a ref the spec
	// does not declare, which fails the prepare here, before anything is cloned.
	want := slices.Sorted(slices.Values(slices.Concat(spec.refs(), cfgRefs)))
	want = slices.Compact(want)
	if len(want) > 0 {
		p, err := attempt()
		if err != nil {
			return err
		}
		if err := p.grant(ctx, want); err != nil {
			return err
		}
	}
	dir, worktree := e.runDir(r.Key), e.o.Paths.WorkerWorktreeDir(r.RunID, r.LeaseEpoch)
	if err := mkdir(dir); err != nil {
		return err
	}
	if err := e.makeWorktree(ctx, r, spec, worktree); err != nil {
		return err
	}
	mat, err := e.materialize(ctx, adapter, dir, snap)
	if err != nil {
		return err
	}
	e.trackAuth(r.Key, adapter, mat)
	configDir := mat.ConfigDir
	run := harness.RunSpec{
		RunID: domain.ID(r.RunID), LeaseEpoch: r.LeaseEpoch, Mode: domain.Mode(spec.Mode), Prompt: spec.Prompt, Model: spec.Requirements.Model,
		RunDir: dir, ConfigDir: configDir, Worktree: worktree, OutputPath: filepath.Join(dir, execwrap.OutputLog),
	}
	if spec.Mode != execwrap.ModeInteractive {
		// Where the structured stdout goes if the adapter asks for it; the path
		// is the same on every rebuild of the Handle.
		run.EventsPath = filepath.Join(dir, execwrap.EventsFile)
	}
	launch, err := adapter.Start(ctx, e.o.Host, run)
	if err != nil {
		return err
	}
	l := execwrap.Launch{
		Argv: launch.Argv, Env: launch.Env, Dir: worktree, Mode: spec.Mode, Grace: e.grace,
		StructuredStdout: launch.StructuredStdout && spec.Mode != execwrap.ModeInteractive,
	}
	env, extra, err := launchSecretEnv(spec, launch.SecretEnv)
	if err != nil {
		return err
	}
	l.SecretEnv = env
	l.RedactOutput = len(cfgRefs) > 0
	extra = slices.DeleteFunc(extra, func(ref string) bool { return slices.Contains(want, ref) })
	if len(want)+len(extra) > 0 {
		p, err := attempt()
		if err != nil {
			return err
		}
		// What the adapter asks for and the spec does not declare is the
		// Controller's to deny: it is asked like the rest.
		if err := p.grant(ctx, extra); err != nil {
			return err
		}
		values, err := p.deliver(ctx, dir)
		if err != nil {
			return err
		}
		if err := p.render(ctx, dir, mat, values); err != nil {
			return err
		}
	}
	if spec.Mode == execwrap.ModeInteractive {
		l.Gate = filepath.Join(dir, paneReady)
	}
	return execwrap.Write(dir, execwrap.LaunchFile, l)
}

// materialize writes the configuration of the Run into its directory (§8.12),
// with the markers of its secrets unrendered, when the harness reads one per
// Run; otherwise it does nothing and says nothing. The secrets are rendered
// into it once they are delivered.
func (e *Executor) materialize(ctx context.Context, adapter harness.Adapter, dir string, snap harness.NativeSnapshot) (host.Materialization, error) {
	if !adapter.Capabilities().ConfigIsolation {
		return host.Materialization{}, nil
	}
	m, err := e.materializer.Materialize(ctx, adapter, dir, snap)
	if errors.Is(err, harness.ErrSecretUnavailable) {
		return host.Materialization{}, fmt.Errorf("%s: %w", domain.CodeSecretUnavailable, err)
	}
	return m, err
}

// trackAuth registers the auth copies of a materialised Run, if the adapter
// keeps them in step; the Run is forgotten when it is over (forget).
func (e *Executor) trackAuth(k state.Key, adapter harness.Adapter, mat host.Materialization) {
	if auth, ok := adapter.(harness.AuthSyncer); ok && e.o.AuthSync != nil && len(mat.AuthCopies) > 0 {
		e.o.AuthSync.Track(authID(k), auth, e.o.Host, mat.AuthCopyPaths())
	}
}

func authID(k state.Key) string { return fmt.Sprintf("%s/%d", k.RunID, k.LeaseEpoch) }

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
		return e.recordStart(ctx, tx, r.Key, raw, now)
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
	run := parseSpec(r.Spec)
	if run.Mode == execwrap.ModeInteractive {
		spec.Capture, spec.Ready = filepath.Join(dir, execwrap.OutputLog), filepath.Join(dir, paneReady)
		if e.usesSecrets(r) {
			// The terminal of a Run with secrets is captured through a filter that
			// redacts them, and the gate stays shut until it says it does.
			spec.CaptureFilter = append(append([]string{}, e.redactPipe...), "--run-dir", dir)
			spec.CaptureAck = filepath.Join(dir, execwrap.CaptureAckFile)
		}
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
	m := e.markers(r.Key)
	if h := e.o.Hooks.AfterMarkers; h != nil {
		h(r)
	}
	if m.started != nil {
		e.revokeOnce(ctx, r)
	}
	if m.exit == nil && m.started != nil {
		e.consumeEvents(ctx, r)
		// A stop ordered by what the harness said is the phase from here on.
		if fresh, err := state.RunBy(ctx, e.o.DB.Reader(), r.Key); err == nil {
			r = fresh
		}
	}
	if m.exit == nil && r.Phase.Live() {
		e.releaseStartIfDue(ctx, r)
	}
	if r.Phase == state.PhaseStopping && m.exit == nil && e.firstLook(r.Key) {
		// A stop the previous daemon began: the signal may not have been sent.
		e.signalStop(ctx, r, m)
	}
	switch {
	case m.wrapperErr != nil:
		// The wrapper cannot be identified: nothing is signalled, whatever the other markers say.
		return e.unresolved(ctx, r, "wrapper.json is unreadable")
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

// markers is what a Run's wrapper has left on disk. A marker that exists and
// cannot be read is not nil and not an error of the whole: its error is kept, and
// what it means depends on what is known of the wrapper (settle.go).
type markers struct {
	wrapper *execwrap.Wrapper
	started *execwrap.Started
	exit    *execwrap.Exit

	wrapperErr, startedErr, exitErr error
}

func (e *Executor) markers(k state.Key) markers {
	var m markers
	dir := e.runDir(k)
	m.exit, m.exitErr = readMarker[execwrap.Exit](dir, execwrap.ExitFile)
	m.wrapper, m.wrapperErr = readMarker[execwrap.Wrapper](dir, execwrap.WrapperFile)
	m.started, m.startedErr = readMarker[execwrap.Started](dir, execwrap.StartedFile)
	return m
}

// diagnosis says, briefly, which markers could not be read and why. It goes in the
// message of a report, which is cleaned before it is written.
func (m markers) diagnosis() string {
	var parts []string
	for _, f := range []struct {
		name string
		err  error
	}{{execwrap.ExitFile, m.exitErr}, {execwrap.StartedFile, m.startedErr}} {
		if f.err != nil {
			parts = append(parts, f.name+" is unreadable ("+brief(f.err)+")")
		}
	}
	return strings.Join(parts, "; ")
}

// brief is the cause of a read error without the paths of this machine.
func brief(err error) string {
	var pe *fs.PathError
	if errors.As(err, &pe) {
		return pe.Err.Error()
	}
	return "corrupt"
}

// readMarker returns nil for a marker that does not exist, and the error for one
// that cannot be read.
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
		return e.recordStart(ctx, tx, r.Key, raw, at)
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
