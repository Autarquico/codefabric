package executor

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"slices"
	"sync"
	"time"

	"golang.org/x/sys/unix"

	"github.com/Autarquico/codefabric/internal/worker/state"
)

// The release of the secrets of a Run (§16.7). Its delivery is kept, for the
// redaction of its output and the guard of its publication, until the Run is
// over, its capture is closed and its publication is final; then secretjit
// removes it. A release that cannot finish is tried again on every tick until
// it does, and the janitor sweeps every run directory when the daemon starts
// and once a minute, which is what finishes a release a crash interrupted and
// what releases the Runs state.db no longer knows.
//
// What is removed is removed logically: the files are unlinked and the
// unlinks synced (see secretjit).

// sweepInterval is how often the janitor sweeps the run directories.
const sweepInterval = time.Minute

// CaptureLockFile is the file of a run directory that every process that
// captures the output of the Run (the wrapper, the capture filter) holds a
// shared lock on while it runs (HoldCapture): the secrets are not released
// while one does, since it may still be draining the output through the
// redaction. A crash releases the lock.
const CaptureLockFile = "capture.lock"

// HoldCapture takes the shared lock of the capture of the Run in runDir, and
// returns its release. The descriptor is not inherited by what the caller
// starts.
func HoldCapture(runDir string) (release func(), err error) {
	fd, err := unix.Open(filepath.Join(runDir, CaptureLockFile), unix.O_RDONLY|unix.O_CREAT|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0o600)
	if err != nil {
		return nil, fmt.Errorf("executor: opening the capture lock: %w", err)
	}
	if err := flockRetry(fd, unix.LOCK_SH); err != nil {
		_ = unix.Close(fd)
		return nil, fmt.Errorf("executor: locking the capture: %w", err)
	}
	return func() { _ = unix.Close(fd) }, nil
}

// captureClosed says whether no process captures the output of the Run in
// runDir: none holds the lock of the capture, or there never was one.
func captureClosed(runDir string) (bool, error) {
	fd, err := unix.Open(filepath.Join(runDir, CaptureLockFile), unix.O_RDONLY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	if errors.Is(err, unix.ENOENT) {
		return true, nil
	}
	if err != nil {
		return false, fmt.Errorf("opening the capture lock: %w", err)
	}
	defer func() { _ = unix.Close(fd) }()
	switch err := flockRetry(fd, unix.LOCK_EX|unix.LOCK_NB); {
	case err == nil:
		return true, nil
	case errors.Is(err, unix.EWOULDBLOCK):
		return false, nil
	default:
		return false, fmt.Errorf("probing the capture lock: %w", err)
	}
}

func flockRetry(fd, how int) error {
	for {
		if err := unix.Flock(fd, how); !errors.Is(err, unix.EINTR) {
			return err
		}
	}
}

// errSecretsHeld is a release the Run is not ready for: it is tried later.
var errSecretsHeld = errors.New("executor: the secrets of the Run are still needed")

// release is a Run whose secrets are to be released.
type release struct {
	// after closes once the prepare of the Run that a stop cut short has
	// returned; nil if there was none.
	after  <-chan struct{}
	warned bool // a failure of this release was logged
}

// releases are the Runs whose secrets the executor is releasing.
type releases struct {
	mu   sync.Mutex
	runs map[state.Key]*release
}

// queueRelease has the secrets of r released once it no longer needs them,
// and after the prepare cut short that after stands for, if any. A Run that
// declares no secret has none to release: the janitor sweeps whatever a Run
// left all the same.
func (e *Executor) queueRelease(r state.Run, after <-chan struct{}) {
	if len(parseSpec(r.Spec).refs()) == 0 {
		return
	}
	e.releases.mu.Lock()
	defer e.releases.mu.Unlock()
	if e.releases.runs == nil {
		e.releases.runs = map[state.Key]*release{}
	}
	e.releases.runs[r.Key] = &release{after: after}
}

// releaseQueued tries the releases that are queued, and forgets those that
// finished.
func (e *Executor) releaseQueued(ctx context.Context) {
	e.releases.mu.Lock()
	keys := make([]state.Key, 0, len(e.releases.runs))
	for k, rel := range e.releases.runs {
		if rel.after != nil {
			select {
			case <-rel.after:
			default:
				continue // its prepare is still returning
			}
		}
		keys = append(keys, k)
	}
	e.releases.mu.Unlock()
	alive := e.sessionsAlive(ctx)
	for _, k := range keys {
		if ctx.Err() != nil {
			return
		}
		err := e.releaseRun(ctx, k, alive)
		e.releases.mu.Lock()
		rel := e.releases.runs[k]
		switch {
		case rel == nil:
		case err == nil:
			delete(e.releases.runs, k)
		case errors.Is(err, errSecretsHeld):
		case !rel.warned:
			rel.warned = true
			e.log.WarnContext(ctx, "releasing the secrets of a Run did not finish; it is tried again", "run", k.RunID, "err", err)
		default:
			e.log.DebugContext(ctx, "releasing the secrets of a Run did not finish yet", "run", k.RunID, "err", err)
		}
		e.releases.mu.Unlock()
	}
}

// releaseRun releases the secrets of the Run k if it no longer needs them.
// What it needs is looked at first without the lock of the run directory, so
// that a Run that is still using them costs no more than reading state.db,
// and again under it.
func (e *Executor) releaseRun(ctx context.Context, k state.Key, alive func(string) (bool, error)) error {
	verify := func() error {
		done, err := e.retentionClosed(ctx, k)
		if err != nil {
			return err
		}
		if !done {
			return errSecretsHeld
		}
		live, err := alive(e.sessionName(k))
		if err != nil {
			return fmt.Errorf("is the session of the Run alive? %w", err)
		}
		if live {
			return errSecretsHeld
		}
		return nil
	}
	if err := verify(); err != nil {
		return err
	}
	return e.jit.Release(e.runDir(k), verify)
}

// retentionClosed says whether the Run k needs its secrets no longer: it is
// over (or state.db does not know it), no process captures its output, and
// its publication, if it has one, is final. It only reads, and never calls
// secretjit: it runs under the lock of the run directory.
func (e *Executor) retentionClosed(ctx context.Context, k state.Key) (bool, error) {
	r, err := state.RunBy(ctx, e.o.DB.Reader(), k)
	switch {
	case errors.Is(err, state.ErrNotFound):
	case err != nil:
		return false, fmt.Errorf("reading the Run: %w", err)
	case r.Phase.Live():
		return false, nil
	}
	p, err := state.PublicationOf(ctx, e.o.DB.Reader(), k)
	switch {
	case errors.Is(err, state.ErrNotFound):
	case err != nil:
		return false, fmt.Errorf("reading the publication of the Run: %w", err)
	case p.State != state.PubPublished && p.State != state.PubFailed:
		return false, nil
	}
	closed, err := captureClosed(e.runDir(k))
	if err != nil {
		return false, fmt.Errorf("is the capture of the Run closed? %w", err)
	}
	return closed, nil
}

// sessionsAlive says whether a session of the Runtime is alive, from one
// listing taken the first time it is asked. A listing that fails is no
// answer: the session may be alive, and the error says so every time.
func (e *Executor) sessionsAlive(ctx context.Context) func(session string) (bool, error) {
	var (
		listed   bool
		sessions []string
		listErr  error
	)
	return func(session string) (bool, error) {
		if !listed {
			listed = true
			sessions, listErr = e.o.Runtime.List(ctx)
		}
		if listErr != nil {
			return false, listErr
		}
		return slices.Contains(sessions, session), nil
	}
}

// sweepSecrets is the janitor: it removes what a crash left of a delivery in
// any run directory, and releases the secrets of every Run that no longer
// needs them, whether state.db knows it or not, unless its session may be
// alive.
func (e *Executor) sweepSecrets(ctx context.Context) error {
	runs := filepath.Join(e.o.Paths.WorkerDataDir(), "runs")
	releasable := func(run string, epoch uint64) (bool, error) {
		return e.retentionClosed(ctx, state.Key{RunID: run, LeaseEpoch: epoch})
	}
	return e.jit.Sweep(runs, releasable, e.sessionsAlive(ctx))
}

// sweep runs the janitor and logs what it could not finish.
func (e *Executor) sweep(ctx context.Context) {
	if err := e.sweepSecrets(ctx); err != nil && ctx.Err() == nil {
		e.log.WarnContext(ctx, "the janitor of the secrets did not finish; it sweeps again", "err", err)
	}
}

// releaseLoop releases the secrets of the Runs that are done: those queued on
// every tick, and every run directory once a minute, until ctx ends.
func (e *Executor) releaseLoop(ctx context.Context) {
	ticker, sweeper := e.clock.NewTicker(tick), e.clock.NewTicker(sweepInterval)
	defer ticker.Stop()
	defer sweeper.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C():
			e.releaseQueued(ctx)
		case <-sweeper.C():
			e.sweep(ctx)
		}
	}
}
