package executor

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"

	"github.com/Autarquico/codefabric/internal/domain"
	"github.com/Autarquico/codefabric/internal/worker/host"
	"github.com/Autarquico/codefabric/internal/worker/state"
)

// The auth copies of a Run (harness.ApplyResult.AuthCopies) hold the refresh
// token of the user. They live as long as the Run does: once it is over, and
// the Broker has reconciled and released them, forget removes them, and the janitor
// removes those a crash or a late prepare left in a Run that is over.
//
// The materialisation of a Run that is over is not needed any more (the
// harness has ended, a publication works on the worktree, and the files
// secretjit rendered into config are removed by Release whether or not they
// are still there). Whatever the harness or the Broker left in it (a copy no
// record names, a temporary of the Broker, files codex wrote) may hold the
// token, so it goes whole, with or without a record.

// removeMaterialization removes config and config.staging of the Run in runDir
// whole. Names are resolved inside runDir without following a link out of it,
// and one that is itself a link is unlinked, not followed. What is not there is
// nothing to remove.
func removeMaterialization(runDir string) error {
	root, err := os.OpenRoot(runDir)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	defer func() { _ = root.Close() }()
	return errors.Join(
		root.RemoveAll(filepath.Base(host.StagingDir(runDir))),
		root.RemoveAll(filepath.Base(host.ConfigDir(runDir))),
	)
}

// sweepAuthCopies removes the auth of the host left in the run directories whose
// Run is over (or state.db does not know) and whose session is not alive, as
// the janitor of the secrets does. A Run that is live keeps its own. A
// directory with no materialisation left, and no refresh pending, is not
// looked at again.
func (e *Executor) sweepAuthCopies(ctx context.Context) error {
	runs := filepath.Join(e.o.Paths.WorkerDataDir(), "runs")
	entries, err := os.ReadDir(runs)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	alive := e.sessionsAlive(ctx)
	var errs []error
	for _, d := range entries {
		run, epoch, ok := domain.ParseRunEpoch(d.Name())
		if !ok || !d.IsDir() {
			continue
		}
		k := state.Key{RunID: run, LeaseEpoch: epoch}
		runDir := filepath.Join(runs, d.Name())
		if !e.hasAuthToSweep(k, runDir) {
			continue
		}
		r, err := state.RunBy(ctx, e.o.DB.Reader(), k)
		switch {
		case errors.Is(err, state.ErrNotFound):
		case err != nil:
			errs = append(errs, err)
			continue
		case r.Phase.Live():
			continue
		}
		live, err := alive(e.sessionName(k))
		if err != nil {
			errs = append(errs, fmt.Errorf("is the session of the Run alive? %w", err))
			continue
		}
		if live {
			continue
		}
		if err := e.dropMaterialization(k, runDir); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// hasAuthToSweep says whether the run directory still holds a materialisation,
// or the Broker still owes the Run its last refresh.
func (e *Executor) hasAuthToSweep(k state.Key, runDir string) bool {
	for _, d := range []string{host.ConfigDir(runDir), host.StagingDir(runDir)} {
		if _, err := os.Lstat(d); !errors.Is(err, fs.ErrNotExist) {
			return true
		}
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	_, pending := e.authPending[k]
	return pending
}
