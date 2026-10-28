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
//
// A materialisation writes the copies before it records them (config.json is
// written last), so a crash can leave a copy nobody has a record of, in
// config.staging or in a published config. The materialisation of a Run that
// is over is not needed any more (the harness has ended, a publication works
// on the worktree, and the files secretjit rendered into config are removed
// by Release whether or not they are still there), so what is not recorded is
// removed whole.

// removeMaterialization removes what the materialisation of the Run in runDir
// holds of the auth of the host. config.staging goes whole. With a readable
// record, the auth copies it lists go; without one the copies cannot be named,
// so config goes whole. Names are resolved inside runDir without following a
// link out of it, and a copy that is itself a link is unlinked, not followed.
// What is not there is nothing to remove.
func removeMaterialization(runDir string) error {
	root, err := os.OpenRoot(runDir)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	defer func() { _ = root.Close() }()
	errs := []error{root.RemoveAll(filepath.Base(host.StagingDir(runDir)))}
	mat, err := host.ReadMaterialization(runDir)
	if err != nil {
		errs = append(errs, root.RemoveAll(filepath.Base(host.ConfigDir(runDir))))
		return errors.Join(errs...)
	}
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

// sweepAuthCopies removes the auth of the host left in the run directories whose
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
		if err := removeMaterialization(filepath.Join(runs, d.Name())); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}
