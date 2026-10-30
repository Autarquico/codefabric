package executor

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"time"

	v1 "github.com/Autarquico/codefabric/internal/api/v1"
	"github.com/Autarquico/codefabric/internal/domain"
	"github.com/Autarquico/codefabric/internal/repoconfig"
	"github.com/Autarquico/codefabric/internal/worker/envwrap"
	"github.com/Autarquico/codefabric/internal/worker/execwrap"
	"github.com/Autarquico/codefabric/internal/worker/procident"
	"github.com/Autarquico/codefabric/internal/worker/state"
)

// The Environment of a Run (§25) is run by a supervisor, `cf worker exec env`,
// that the daemon launches in a process group of its own and that outlives it
// (package envwrap). The daemon's part is the protocol around it:
//
//	intention (a row in state.db) -> launch -> the supervisor's identity -> authorization
//	-> prepared -> services ready -> the harness   (environmentStage)
//	stop order -> env-exit.json -> the group is proven empty (settleEnvironment)
//
// Nothing of it blocks: every step looks at the files of the run directory and
// the process, and does the next thing or says it is not time yet.

// Timings of the Environment, on the clock of the executor.
const (
	// envGateWait is how long a launched supervisor may take to say who it is: its
	// gate times out after envwrap.GateTimeout, so past this it is not coming.
	envGateWait = envwrap.GateTimeout + stopMargin
	// envMaxLaunches is how many supervisors a Run gets: one that dies without
	// leaving an exit is launched again, but not for ever.
	envMaxLaunches = 3
	// envMaxRecoveries is how many supervisors, beyond those, a Run gets to stop
	// the services of one that was lost: a recovery supervisor that dies leaves no
	// stop proven, so it is launched again, but not for ever.
	envMaxRecoveries = 2
)

// EnvironmentTimeouts are the limits of the scripts of an Environment. Zero is
// the default of package envwrap: prepare 10 minutes, ready and stop of each
// service 2 minutes and 30 seconds, 30 seconds of grace for what a script leaves.
type EnvironmentTimeouts struct {
	Prepare, Ready, Stop, Grace time.Duration
}

type envStatus int

const (
	envPending envStatus = iota // not yet
	envReady                    // prepared, and every service is ready
	envFailed                   // it failed; the message says why
)

// environmentOf is the Environment a Run must run before its harness: nil for a
// Run that has none, or whose Environment has neither prepare nor services.
func (e *Executor) environmentOf(ctx context.Context, r state.Run) (*domain.EnvironmentDef, error) {
	if parseSpec(r.Spec).Schema != domain.RunSpecSchemaRepoConfig {
		return nil, nil
	}
	res, ok, err := state.ResolutionOf(ctx, e.o.DB.Reader(), r.Key)
	if err != nil || !ok {
		return nil, err
	}
	var eff repoconfig.Effective
	if err := json.Unmarshal(res.Effective, &eff); err != nil {
		return nil, fmt.Errorf("the stored configuration of the Run is corrupt: %w", err)
	}
	if env := eff.Environment; env != nil && (env.Prepare != "" || len(env.Services) > 0) {
		return env, nil
	}
	return nil, nil
}

// hasEnvironment says whether a Run has the record of an Environment supervisor.
// A record that cannot be read is an error, never "no Environment": what a Run
// without one does (prepare again, be aborted) is not undoable under a supervisor.
func (e *Executor) hasEnvironment(ctx context.Context, k state.Key) (bool, error) {
	_, ok, err := state.EnvironmentOf(ctx, e.o.DB.Reader(), k)
	return ok, err
}

// beginOrEnvironment takes an accepted Run on: to its harness if it has no
// Environment to bring up, or when it is up; otherwise one step of the Environment.
// The caller holds the lock of the Run.
func (e *Executor) beginOrEnvironment(ctx context.Context, r state.Run) (done bool) {
	def, err := e.environmentOf(ctx, r)
	if err != nil {
		return e.settleNeverRan(ctx, r.Key, "reading the Environment of the Run failed: "+err.Error())
	}
	if def == nil {
		return e.beginAborted(ctx, r)
	}
	switch st, msg := e.environmentStage(ctx, r, def); st {
	case envPending:
		return false
	case envFailed:
		return e.settleNeverRanWith(ctx, r.Key, facts{message: msg})
	}
	return e.beginAborted(ctx, r)
}

func (e *Executor) beginAborted(ctx context.Context, r state.Run) bool {
	return errors.Is(e.begin(ctx, r), ErrAborted)
}

// environmentStage is one step of bringing the Environment of an accepted Run up.
// The caller holds the lock of the Run, so a stop is before all of a step or
// after all of it: it cannot find a supervisor launched or authorized behind
// its back.
func (e *Executor) environmentStage(ctx context.Context, r state.Run, def *domain.EnvironmentDef) (envStatus, string) {
	k, dir := r.Key, e.runDir(r.Key)
	now := e.clock.Now()
	rec, ok, err := state.EnvironmentOf(ctx, e.o.DB.Reader(), k)
	if err == nil && !ok {
		// The intention, before anything is launched: a supervisor that exists
		// has a row, whatever the daemon does next.
		err = e.o.DB.WithTx(ctx, func(tx *state.Tx) error { return state.BeginEnvironment(ctx, tx, k, now) })
		rec = state.Environment{Key: k, CreatedAt: now}
	}
	if err != nil {
		e.log.ErrorContext(ctx, "recording the Environment of a Run", "run", k.RunID, "err", err)
		return envPending, ""
	}
	if st, msg, decided := e.environmentOutcome(dir); decided {
		if st == envReady {
			return e.confirmReady(ctx, k, dir)
		}
		return st, msg
	}
	w, err := readMarker[execwrap.Wrapper](dir, envwrap.WrapperFile)
	if err != nil {
		return envFailed, "the identity of the Environment supervisor is unreadable"
	}
	if w == nil {
		if rec.Launches > 0 && now.Sub(rec.LaunchedAt) < envGateWait {
			return envPending, "" // it is starting
		}
		return e.launchEnvironment(ctx, r, def, rec)
	}
	switch v := verdictOf(w); v {
	case procident.Same:
		e.resolved(k)
		if !fileExists(filepath.Join(dir, envwrap.AuthorizedFile)) {
			// Its identity is read, its intention is durable: it may run.
			// It authorizes this identity, and no other process.
			a := envwrap.Authorized{RunID: k.RunID, LeaseEpoch: k.LeaseEpoch, PID: w.PID, PGID: w.PGID, Start: w.Start, At: now}
			if err := execwrap.Write(dir, envwrap.AuthorizedFile, a); err != nil {
				e.log.ErrorContext(ctx, "authorizing the Environment supervisor", "run", k.RunID, "err", err)
			}
		}
		return envPending, ""
	case procident.Unknown:
		e.log.Log(ctx, slog.LevelDebug, "the Environment supervisor of a Run cannot be identified; looking again", "run", k.RunID)
		return envPending, ""
	case procident.Gone:
		// It died with no exit. Whatever it left in its group is ended first: a
		// supervisor is never launched over the processes of the one before.
		if !e.groupEmpty(ctx, w) {
			e.log.WarnContext(ctx, "processes of an Environment survive its supervisor: killing them", "run", k.RunID)
			_ = syscall.Kill(-w.PGID, syscall.SIGKILL)
			return envPending, ""
		}
	}
	// Gone and empty, or another boot, or the pid in other hands (whose group is
	// then empty): the supervisor is over.
	return e.launchEnvironment(ctx, r, def, rec)
}

// confirmReady accepts the ready markers only while the supervisor that wrote
// them is alive and ours: they say the Environment was up, not that it is. If it
// is gone the harness does not start; the Run fails and the settlement of the
// Run ends what the supervisor left (and stops the services it started).
func (e *Executor) confirmReady(ctx context.Context, k state.Key, dir string) (envStatus, string) {
	const lost = "the Environment supervisor ended after the services were ready"
	w, err := readMarker[execwrap.Wrapper](dir, envwrap.WrapperFile)
	switch {
	case err != nil:
		return envFailed, "the identity of the Environment supervisor is unreadable"
	case w == nil:
		return envFailed, lost
	}
	switch verdictOf(w) {
	case procident.Same:
		return envReady, ""
	case procident.Unknown:
		e.log.Log(ctx, slog.LevelDebug, "the Environment supervisor of a Run cannot be identified; looking again", "run", k.RunID)
		return envPending, ""
	}
	return envFailed, lost
}

// environmentOutcome is what the markers of the supervisor already say.
func (e *Executor) environmentOutcome(dir string) (st envStatus, msg string, decided bool) {
	prepared, perr := readMarker[envwrap.Prepared](dir, envwrap.PreparedFile)
	services, serr := readMarker[envwrap.Services](dir, envwrap.ServicesFile)
	exit, xerr := readMarker[envwrap.Exit](dir, envwrap.ExitFile)
	switch {
	case perr != nil || serr != nil || xerr != nil:
		return envFailed, "a marker of the Environment is unreadable", true
	case prepared != nil && prepared.Status == envwrap.StatusFailed:
		return envFailed, orDefault(prepared.Message, "prepare failed"), true
	case services != nil && services.Status == envwrap.StatusFailed:
		return envFailed, orDefault(services.Message, "a service failed"), true
	case exit != nil:
		// Its stop was never ordered: it ended on its own (or was signalled).
		return envFailed, orDefault(exit.Error, "the Environment supervisor ended before the Run started"), true
	case prepared != nil && prepared.Status == envwrap.StatusOK && services != nil && services.Status == envwrap.StatusOK:
		return envReady, "", true
	}
	return envPending, "", false
}

func orDefault(s, def string) string {
	if strings.TrimSpace(s) == "" {
		return def
	}
	return s
}

// launchEnvironment launches the supervisor of a Run: the launch file, the
// intention to launch (in state.db), then the process. Nothing of a previous
// supervisor is left to be taken for this one's. It is the first launch or a
// relaunch; the markers prepared.json (which says prepare never runs again)
// stays.
func (e *Executor) launchEnvironment(ctx context.Context, r state.Run, def *domain.EnvironmentDef, rec state.Environment) (envStatus, string) {
	k, dir := r.Key, e.runDir(r.Key)
	if rec.Launches >= envMaxLaunches {
		return envFailed, "the Environment supervisor kept dying before it finished"
	}
	launch, err := e.environmentLaunch(r, def)
	if err != nil {
		return envFailed, err.Error()
	}
	for _, name := range []string{envwrap.WrapperFile, envwrap.AuthorizedFile, envwrap.ServicesFile, envwrap.StopFile} {
		if err := os.Remove(filepath.Join(dir, name)); err != nil && !os.IsNotExist(err) {
			e.log.ErrorContext(ctx, "clearing the files of a previous Environment supervisor", "run", k.RunID, "err", err)
			return envPending, ""
		}
	}
	if err := execwrap.Write(dir, envwrap.LaunchFile, launch); err != nil {
		e.log.ErrorContext(ctx, "writing the launch file of an Environment", "run", k.RunID, "err", err)
		return envPending, ""
	}
	err = e.o.DB.WithTx(ctx, func(tx *state.Tx) error { return state.NoteEnvironmentLaunch(ctx, tx, k, e.clock.Now()) })
	if err != nil {
		e.log.ErrorContext(ctx, "recording the launch of an Environment", "run", k.RunID, "err", err)
		return envPending, ""
	}
	if err := e.spawnEnvironment(ctx, dir); err != nil {
		e.mu.Lock()
		e.memo(k).noEnvProcess = true
		e.mu.Unlock()
		return envFailed, "launching the Environment supervisor failed: " + err.Error()
	}
	return envPending, ""
}

// environmentLaunch is what the supervisor of a Run runs. It runs with the
// secrets the Run declared for its environment (the same names the harness gets,
// never the ones an adapter adds for itself), which the supervisor resolves
// itself; the daemon passes refs, never values.
func (e *Executor) environmentLaunch(r state.Run, def *domain.EnvironmentDef) (envwrap.Launch, error) {
	env, _, err := launchSecretEnv(parseSpec(r.Spec), nil)
	if err != nil {
		return envwrap.Launch{}, err
	}
	t := e.o.EnvironmentTimeouts
	return envwrap.Launch{
		RunID: r.RunID, LeaseEpoch: r.LeaseEpoch,
		Dir: e.worktree(r.Key), Prepare: def.Prepare, Services: slices.Clone(def.Services),
		SecretEnv: env, RedactOutput: e.usesSecrets(r),
		PrepareTimeout: t.Prepare, ReadyTimeout: t.Ready, StopTimeout: t.Stop, Grace: t.Grace,
	}, nil
}

// spawnEnvironment starts the supervisor in a process group of its own and does
// not bind it to the daemon: it is not killed with the daemon's context, nothing
// of it is read through the daemon's pipes, and it is reaped here only to leave
// no zombie while the daemon lives (procgroup would end its group with the context,
// which is what a supervisor that outlives the daemon must not have).
func (e *Executor) spawnEnvironment(ctx context.Context, dir string) error {
	argv := append(slices.Clone(e.envArgv), "--run-dir", dir)
	// Not bound to ctx: the supervisor outlives the daemon.
	cmd := exec.CommandContext(context.WithoutCancel(ctx), argv[0], argv[1:]...) //nolint:gosec // our own binary, from Options
	cmd.Dir = dir
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		return err
	}
	go func() { _ = cmd.Wait() }()
	return nil
}

// envSettlement is how far the Environment of a Run is from being gone.
type envSettlement struct {
	done bool // nothing of it is left (or it had nothing)
	// unverified, if not empty, is why it cannot be proven gone: nothing is signalled.
	unverified string
	failures   []envwrap.StopFailure
}

// settleEnvironment brings the Environment of a Run to an end that can be
// proven, before the Controller is told the Run stopped: it orders the stop,
// waits (bounded) for the supervisor to stop the services and say so, checks
// who the supervisor is, kills its group if it outlives the time it had, and
// asks that the group is empty. It is called again until it is done; the caller
// holds the lock of the Run. A Run with no record is done.
func (e *Executor) settleEnvironment(ctx context.Context, k state.Key) envSettlement {
	rec, ok, err := state.EnvironmentOf(ctx, e.o.DB.Reader(), k)
	if err != nil {
		return envSettlement{unverified: "the Environment record cannot be read"}
	}
	if !ok {
		return envSettlement{done: true}
	}
	dir, now := e.runDir(k), e.clock.Now()
	// Writing the order signals nothing, so it needs no identity.
	order := e.orderEnvironmentStop(ctx, k, dir, now)
	w, err := readMarker[execwrap.Wrapper](dir, envwrap.WrapperFile)
	if err != nil {
		return envSettlement{unverified: "env-wrapper.json is unreadable"} // no identity: nothing is signalled
	}
	if w == nil {
		return e.settleUnidentified(ctx, k, rec, now)
	}
	switch v := verdictOf(w); v {
	case procident.Same:
		return e.settleLiving(ctx, k, dir, w, rec, order, now)
	case procident.Gone:
		if !e.groupEmpty(ctx, w) {
			e.log.WarnContext(ctx, "processes of an Environment survive its supervisor: killing them", "run", k.RunID)
			_ = syscall.Kill(-w.PGID, syscall.SIGKILL)
			return envSettlement{}
		}
	case procident.Rebooted, procident.Different:
		// Another boot, or another process with the pid: the group of the supervisor
		// is empty (a pid is not reused while a group with its ID has members), and
		// whatever has the number now is not ours.
	default:
		return envSettlement{unverified: "the identity of the Environment supervisor cannot be verified"}
	}
	return e.environmentGone(ctx, k, dir, rec)
}

// settleUnidentified is a supervisor that never said who it is. Until its gate
// would have timed out it may still come: the stop order is there for it to
// find, and it ends at its gate. After, it is not coming.
func (e *Executor) settleUnidentified(ctx context.Context, k state.Key, rec state.Environment, now time.Time) envSettlement {
	e.mu.Lock()
	none := e.memos[k] != nil && e.memos[k].noEnvProcess // read: a memo is not made for a stray
	e.mu.Unlock()
	if none || rec.Launches == 0 || now.Sub(rec.LaunchedAt) >= envGateWait {
		if !none && rec.Launches > 0 {
			e.log.WarnContext(ctx, "an Environment supervisor never identified itself: it is taken as not having run", "run", k.RunID)
		}
		return e.environmentGone(ctx, k, e.runDir(k), rec)
	}
	return envSettlement{}
}

// settleLiving is a supervisor that is alive and ours.
func (e *Executor) settleLiving(ctx context.Context, k state.Key, dir string, w *execwrap.Wrapper, rec state.Environment, order envwrap.StopOrder, now time.Time) envSettlement {
	e.authorizeRecovery(ctx, k, dir, w, now)
	if exit, _ := readMarker[envwrap.Exit](dir, envwrap.ExitFile); exit != nil {
		if now.Sub(exit.ExitedAt) < wrapperExitWait {
			return envSettlement{} // it is ending
		}
		e.log.WarnContext(ctx, "the Environment supervisor wrote its exit and did not end: killing it", "run", k.RunID)
		_ = syscall.Kill(-w.PGID, syscall.SIGKILL)
		return envSettlement{}
	}
	// The time runs from the later of the order and the launch of this supervisor:
	// a recovery supervisor is launched after the order.
	if now.Before(later(order.At, rec.LaunchedAt).Add(e.environmentStopBudget(dir))) {
		return envSettlement{}
	}
	e.log.WarnContext(ctx, "the Environment did not stop in time: killing its group", "run", k.RunID)
	_ = syscall.Kill(-w.PGID, syscall.SIGKILL)
	return envSettlement{}
}

// authorizeRecovery opens the gate of a recovery supervisor that is alive and
// ours, as environmentStage does for the supervisor of a Run that is starting:
// it authorizes the identity just verified, and no other process. A supervisor
// that is not a recovery one finds its gate in environmentStage, or ends at it.
func (e *Executor) authorizeRecovery(ctx context.Context, k state.Key, dir string, w *execwrap.Wrapper, now time.Time) {
	var l envwrap.Launch
	if ok, err := execwrap.Read(dir, envwrap.LaunchFile, &l); err != nil || !ok || !l.Recover || fileExists(filepath.Join(dir, envwrap.AuthorizedFile)) {
		return
	}
	a := envwrap.Authorized{RunID: k.RunID, LeaseEpoch: k.LeaseEpoch, PID: w.PID, PGID: w.PGID, Start: w.Start, At: now}
	if err := execwrap.Write(dir, envwrap.AuthorizedFile, a); err != nil {
		e.log.ErrorContext(ctx, "authorizing the recovery of an Environment", "run", k.RunID, "err", err)
	}
}

func later(a, b time.Time) time.Time {
	if b.After(a) {
		return b
	}
	return a
}

// environmentGone is the end of a settlement whose supervisor and group are gone:
// what its exit says of the stops that failed. A supervisor that left no exit but
// recorded services as started (env-started.json) is lost with them running, as
// far as anyone knows: a recovery supervisor is launched to stop them, which goes
// through the same identity, gate and exit as any other, and this is "not yet"
// until its exit is there and its group is gone. If none can be launched (or too
// many died), the stops are failures; the reservation is released all the same
// (best effort, §25), but never while a recovery supervisor may still be running.
func (e *Executor) environmentGone(ctx context.Context, k state.Key, dir string, rec state.Environment) envSettlement {
	if exit, _ := readMarker[envwrap.Exit](dir, envwrap.ExitFile); exit != nil {
		return envSettlement{done: true, failures: exit.StopFailures}
	}
	started, err := readMarker[envwrap.Started](dir, envwrap.StartedFile)
	switch {
	case err != nil:
		return envSettlement{done: true, failures: []envwrap.StopFailure{{Service: "environment", Error: "stop not attempted: the record of started services is unreadable"}}}
	case started == nil || len(started.Services) == 0:
		return envSettlement{done: true}
	}
	lost := func() envSettlement {
		out := envSettlement{done: true}
		for _, name := range started.Services {
			out.failures = append(out.failures, envwrap.StopFailure{Service: name, Error: "stop not attempted: supervisor lost"})
		}
		return out
	}
	if rec.Launches >= envMaxLaunches+envMaxRecoveries {
		return lost()
	}
	var launch envwrap.Launch
	if ok, err := execwrap.Read(dir, envwrap.LaunchFile, &launch); err != nil || !ok {
		return lost()
	}
	return e.launchRecovery(ctx, k, dir, launch)
}

// launchRecovery launches a supervisor whose only job is to stop the services
// the lost one started. Nothing of a previous supervisor is left to be taken for
// this one's; the stop order stands, and it is not heeded by the recovery.
func (e *Executor) launchRecovery(ctx context.Context, k state.Key, dir string, launch envwrap.Launch) envSettlement {
	pending := envSettlement{unverified: "the recovery of the Environment could not be launched"}
	for _, name := range []string{envwrap.WrapperFile, envwrap.AuthorizedFile} {
		if err := os.Remove(filepath.Join(dir, name)); err != nil && !os.IsNotExist(err) {
			e.log.ErrorContext(ctx, "clearing the files of a lost Environment supervisor", "run", k.RunID, "err", err)
			return pending
		}
	}
	launch.Recover = true
	if err := execwrap.Write(dir, envwrap.LaunchFile, launch); err != nil {
		e.log.ErrorContext(ctx, "writing the launch file of an Environment recovery", "run", k.RunID, "err", err)
		return pending
	}
	err := e.o.DB.WithTx(ctx, func(tx *state.Tx) error { return state.NoteEnvironmentLaunch(ctx, tx, k, e.clock.Now()) })
	if err != nil {
		e.log.ErrorContext(ctx, "recording the launch of an Environment recovery", "run", k.RunID, "err", err)
		return pending
	}
	spawnErr := e.spawnEnvironment(ctx, dir)
	e.mu.Lock()
	if spawnErr != nil {
		e.memo(k).noEnvProcess = true
	} else if m := e.memos[k]; m != nil {
		m.noEnvProcess = false // there is a process to wait for again
	}
	e.mu.Unlock()
	if spawnErr != nil {
		e.log.ErrorContext(ctx, "launching the recovery of an Environment failed", "run", k.RunID, "err", spawnErr)
	}
	return envSettlement{}
}

// orderEnvironmentStop writes the stop order of the supervisor if there is none
// and returns the order that stands. The order is a file, written under the lock
// of the Run, and the supervisor reads it before every spawn; whatever it spawned as the order landed is stopped with the rest.
func (e *Executor) orderEnvironmentStop(ctx context.Context, k state.Key, dir string, now time.Time) envwrap.StopOrder {
	if o, err := readMarker[envwrap.StopOrder](dir, envwrap.StopFile); err == nil && o != nil {
		return *o
	}
	o := envwrap.StopOrder{At: now, Reason: "the Run is over"}
	if err := execwrap.Write(dir, envwrap.StopFile, o); err != nil {
		e.log.ErrorContext(ctx, "ordering the Environment to stop", "run", k.RunID, "err", err)
	}
	return o
}

// environmentStopBudget is how long the supervisor has, from the order, to stop
// every service, one after the other, and its group: after it the group is killed.
func (e *Executor) environmentStopBudget(dir string) time.Duration {
	var l envwrap.Launch
	_, _ = execwrap.Read(dir, envwrap.LaunchFile, &l)
	stop, grace := l.StopTimeout, l.Grace
	if stop <= 0 {
		stop = envwrap.DefaultStopTimeout
	}
	if grace <= 0 {
		grace = envwrap.DefaultGrace
	}
	return time.Duration(len(l.Services)+1)*stop + grace + stopMargin
}

// settleStrays settles the Environments whose Run is over, which a crash left
// behind (a Run that is live settles its own, when it ends), and forgets the
// ones that are gone. It runs at the start of the daemon and at every sweep.
func (e *Executor) settleStrays(ctx context.Context) {
	recs, err := state.Environments(ctx, e.o.DB.Reader())
	if err != nil {
		e.log.ErrorContext(ctx, "listing the Environments", "err", err)
		return
	}
	for _, rec := range recs {
		if ctx.Err() != nil {
			return
		}
		e.settleStray(ctx, rec.Key)
	}
}

func (e *Executor) settleStray(ctx context.Context, k state.Key) {
	unlock := e.lock(k)
	defer unlock()
	r, err := state.RunBy(ctx, e.o.DB.Reader(), k)
	switch {
	case errors.Is(err, state.ErrNotFound):
		// The Run is gone: so is its Environment.
	case err != nil:
		// Its phase is unknown, and a Run that lives settles its own Environment.
		e.log.ErrorContext(ctx, "reading the Run of an Environment; it is tried again", "run", k.RunID, "err", err)
		return
	case r.Phase.Live():
		return
	}
	res := e.settleEnvironment(ctx, k)
	if !res.done {
		if res.unverified != "" {
			e.log.ErrorContext(ctx, "the Environment of a Run that is over cannot be verified; it is tried again", "run", k.RunID, "why", res.unverified)
		}
		return
	}
	if err := e.o.DB.WithTx(ctx, func(tx *state.Tx) error { return state.DropEnvironment(ctx, tx, k) }); err != nil {
		e.log.ErrorContext(ctx, "forgetting a settled Environment", "run", k.RunID, "err", err)
		return
	}
	e.warnStopFailures(ctx, k, res.failures)
}

// warnStopFailures logs the services whose stop failed (best effort, §25).
func (e *Executor) warnStopFailures(ctx context.Context, k state.Key, failures []envwrap.StopFailure) {
	if len(failures) > 0 {
		e.log.WarnContext(ctx, "services of an Environment did not stop cleanly", "run", k.RunID, "services", stopFailureNames(failures))
	}
}

func stopFailureNames(failures []envwrap.StopFailure) []string {
	names := make([]string, len(failures))
	for i, f := range failures {
		names[i] = f.Service
	}
	return names
}

// reportedStopFailures is what the stopped report says of the services that did
// not stop: the Controller turns it into an EnvironmentStopFailed event. It is
// bounded and printable, like every text of a report.
func reportedStopFailures(failures []envwrap.StopFailure) []v1.StopFailure {
	out := make([]v1.StopFailure, 0, min(len(failures), v1.MaxStopFailures))
	encoded := 0
	for _, f := range failures[:min(len(failures), v1.MaxStopFailures)] {
		sf := v1.StopFailure{Service: orDefault(reportable(f.Service), "service"), Error: reportable(f.Error)}
		raw, err := json.Marshal(sf)
		// The Controller measures the escaped report (< and & take six bytes), so
		// the budget counts the encoded size, not the raw text.
		if err != nil || (len(out) > 0 && encoded+len(raw)+1 > stopFailuresEncodedBudget) {
			break
		}
		encoded += len(raw) + 1
		out = append(out, sf)
	}
	return out
}

// stopFailuresEncodedBudget keeps a stopped well inside v1.MaxReportBytes.
const stopFailuresEncodedBudget = 12 << 10

// environmentFailure is why the Environment of a Run that never ran its harness
// failed, as the supervisor left it ("" if it did not fail).
func (e *Executor) environmentFailure(k state.Key) string {
	dir := e.runDir(k)
	if exit, _ := readMarker[envwrap.Exit](dir, envwrap.ExitFile); exit != nil && (exit.Cause == envwrap.CauseStop || exit.Cause == envwrap.CauseLost) {
		// The stop was ours (or the supervisor was lost and the Run ended); a prepare that was cut short by it left no marker.
		if p, _ := readMarker[envwrap.Prepared](dir, envwrap.PreparedFile); p == nil || p.Status != envwrap.StatusFailed {
			return ""
		}
	}
	if st, msg, decided := e.environmentOutcome(dir); decided && st == envFailed {
		return msg
	}
	return ""
}
