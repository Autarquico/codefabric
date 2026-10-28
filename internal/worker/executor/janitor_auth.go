package executor

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"

	"github.com/Autarquico/codefabric/internal/domain"
	"github.com/Autarquico/codefabric/internal/worker/host"
	"github.com/Autarquico/codefabric/internal/worker/state"
)

// The auth copies of a Run (harness.ApplyResult.AuthCopies) hold the refresh
// token of the user. They live as long as the Run does: once it is over, and
// the Broker no longer tracks them, forget removes them, and the janitor
// removes those a crash or a late prepare left in a Run that is over.

// removeAuthCopies removes the auth copies of the Run in runDir, as its
// materialisation records them. The names are resolved inside runDir without
// following a link out of it, and a copy that is itself a link is unlinked, not
// followed. What is not there is nothing to remove.
func removeAuthCopies(runDir string) error {
	mat, err := host.ReadMaterialization(runDir)
	if err != nil || len(mat.AuthCopies) == 0 {
		return nil // not materialised, or no copies: nothing was written
	}
	root, err := os.OpenRoot(runDir)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	defer func() { _ = root.Close() }()
	var errs []error
	for _, c := range mat.AuthCopies {
		rel, err := filepath.Rel(runDir, filepath.Join(mat.ConfigDir, filepath.FromSlash(c)))
		if err == nil {
			err = root.Remove(rel)
		}
		if err != nil && !errors.Is(err, fs.ErrNotExist) {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// sweepAuthCopies removes the auth copies left in the run directories whose
// Run is over (or state.db does not know). A Run that is live keeps its own.
func (e *Executor) sweepAuthCopies(ctx context.Context) error {
	runs := filepath.Join(e.o.Paths.WorkerDataDir(), "runs")
	entries, err := os.ReadDir(runs)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	var errs []error
	for _, d := range entries {
		run, epoch, ok := domain.ParseRunEpoch(d.Name())
		if !ok || !d.IsDir() {
			continue
		}
		r, err := state.RunBy(ctx, e.o.DB.Reader(), state.Key{RunID: run, LeaseEpoch: epoch})
		switch {
		case errors.Is(err, state.ErrNotFound):
		case err != nil:
			errs = append(errs, err)
			continue
		case r.Phase.Live():
			continue
		}
		if err := removeAuthCopies(filepath.Join(runs, d.Name())); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}
