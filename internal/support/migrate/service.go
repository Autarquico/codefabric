package migrate

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"time"

	"github.com/Autarquico/codeflock/internal/config"
	"github.com/Autarquico/codeflock/internal/support/atomicfile"
	"github.com/Autarquico/codeflock/internal/support/svcunit"
)

// ErrNoOldService is what Service.Migrate says when the service of the old
// name is not installed: there is nothing to move.
var ErrNoOldService = errors.New("migrate: the service of the old name is not installed")

// Snapshot is the service of the old name as it was before anything changed.
// It is what a resumed migration goes on from: the old service is already
// stopped by then, and whether it ran is no longer to be read from it.
type Snapshot struct {
	Active bool              `json:"active"`
	Env    map[string]string `json:"env"`
	Argv   []string          `json:"argv"` // the program first
}

// Dirs are the directories a service uses, before and after the rename.
type Dirs struct{ OldConfig, NewConfig, OldData, NewData string }

// pairs are the directories that move, and on macOS the logs of launchd, which
// sit with them. A directory that is not where the old name put it, because
// the person chose it, does not move.
func (d Dirs) pairs(env svcunit.Env) []Pair {
	var pairs []Pair
	for _, p := range []Pair{{d.OldConfig, d.NewConfig}, {d.OldData, d.NewData}} {
		if p.Old != p.New {
			pairs = append(pairs, p)
		}
	}
	if len(pairs) > 0 && env.GOOS == "darwin" {
		logs := filepath.Join(env.Home, "Library", "Logs")
		pairs = append(pairs, Pair{Old: filepath.Join(logs, "codefabric"), New: filepath.Join(logs, "codeflock")})
	}
	return pairs
}

// Adopt moves the directories of an install-service with no old service to
// move them: it is run from a machine whose default directories still have the
// old name, which are the ones config.Resolve then gives. It returns the new
// paths, and refuses while an old service is running on the old ones.
func (m Mover) Adopt(ctx context.Context, env svcunit.Env, cfg, data string) (newCfg, newData string, err error) {
	dirs := Dirs{OldConfig: cfg, NewConfig: Renamed(cfg), OldData: data, NewData: Renamed(data)}
	pairs := dirs.pairs(env)
	if len(pairs) == 0 {
		return cfg, data, nil
	}
	if err := checkShared(ctx, env, dirs); err != nil {
		return "", "", err
	}
	if _, err := m.Move(pairs); err != nil {
		return "", "", err
	}
	return dirs.NewConfig, dirs.NewData, nil
}

// DirsOf are the directories the old service says it uses, and where they go.
func DirsOf(env map[string]string) (Dirs, error) {
	cfg, data := env["CF_CONFIG_DIR"], env["CF_DATA_DIR"]
	if cfg == "" || data == "" {
		return Dirs{}, errors.New("the old service does not say its CF_CONFIG_DIR and CF_DATA_DIR, so there is no telling what to move")
	}
	return Dirs{OldConfig: cfg, NewConfig: Renamed(cfg), OldData: data, NewData: Renamed(data)}, nil
}

// Renamed is where a directory goes: the old application name becomes the new
// one, and any other name is the person's own and stays.
func Renamed(dir string) string {
	if filepath.Base(dir) == "codefabric" {
		return filepath.Join(filepath.Dir(dir), "codeflock")
	}
	return dir
}

// Service moves one service from the old name to the new one, and the
// directories it uses with it, as steps that each can be put back.
type Service struct {
	Env svcunit.Env
	Old svcunit.Spec
	// Preflight refuses, before anything changes, a service that must not be
	// touched now; it is not asked again when a cut migration resumes.
	Preflight func(ctx context.Context, old Snapshot, dirs Dirs) error
	// New is the unit that replaces the old one.
	New func(old Snapshot, dirs Dirs) (svcunit.Spec, error)
	// Settle waits until the new service has had time to fail; nil is two seconds.
	Settle func(ctx context.Context) error
	// Mover is where tests inject failures.
	Mover Mover
}

// Migrate runs the move; the new service runs if the old one did. If a step fails the ones before it are undone in
// reverse order, the old service runs again if it did, and the error says what
// failed; if undoing fails too, the error says that, and the progress file
// stays so that running it again goes on. The result is that of the new unit,
// with the actions of the whole move in front.
func (s Service) Migrate(ctx context.Context) (svcunit.Result, error) {
	in, err := svcunit.Inspect(ctx, s.Env, s.Old)
	switch {
	case err != nil:
		return svcunit.Result{}, err
	case !in.Found:
		return svcunit.Result{}, ErrNoOldService
	}
	snap := Snapshot{Active: in.Active, Env: in.Env, Argv: in.Argv}
	dirs, err := DirsOf(in.Env)
	if err != nil {
		return svcunit.Result{}, err
	}
	progress := progressFile{dirs: dirs}
	if saved, ok, err := progress.read(); err != nil {
		return svcunit.Result{}, err
	} else if ok {
		snap = saved
	} else if err := s.begin(ctx, snap, dirs); err != nil {
		return svcunit.Result{}, err
	}
	spec, err := s.New(snap, dirs)
	if err != nil {
		return svcunit.Result{}, err
	}
	if err := progress.write(snap); err != nil {
		return svcunit.Result{}, err
	}
	return s.run(ctx, snap, dirs, spec, progress)
}

// begin is what a migration that starts checks: Preflight, and that no other
// service of the old name is running on the directories about to move.
func (s Service) begin(ctx context.Context, snap Snapshot, dirs Dirs) error {
	if s.Preflight != nil {
		if err := s.Preflight(ctx, snap, dirs); err != nil {
			return err
		}
	}
	return checkShared(ctx, s.Env, dirs, s.Old.Name, s.Old.Label)
}

// checkShared refuses while another service of the old name, not one of self, is
// running on the directories that are about to move: it would go on writing
// to where they were.
func checkShared(ctx context.Context, env svcunit.Env, dirs Dirs, self ...string) error {
	for _, u := range config.OldUnits(env.Home, env.GOOS) {
		if slices.Contains(self, u.Name) || u.System {
			continue
		}
		other, err := svcunit.Inspect(ctx, env, svcunit.Spec{Name: u.Name, Label: u.Name})
		if err != nil || !other.Found {
			continue
		}
		if shared := other.Env["CF_DATA_DIR"] == dirs.OldData || other.Env["CF_CONFIG_DIR"] == dirs.OldConfig; shared && other.Active {
			return fmt.Errorf("%s is running on the same directories, and they are about to move: stop it first "+
				"(and then run `cflk %s install-service` to move it as well)", u.Name, u.Kind)
		}
	}
	return nil
}

type step struct {
	name string
	do   func() error
	undo func() error
}

func (s Service) run(ctx context.Context, snap Snapshot, dirs Dirs, spec svcunit.Spec, progress progressFile) (svcunit.Result, error) {
	var (
		actions []string
		result  svcunit.Result
		pairs   = dirs.pairs(s.Env)
	)
	back := make([]Pair, len(pairs))
	for i, p := range pairs {
		back[len(pairs)-1-i] = Pair{Old: p.New, New: p.Old}
	}
	record := func(r svcunit.Result) { actions = append(actions, r.Actions...) }
	steps := []step{
		{"stopping " + s.Old.Name, func() error {
			r, err := svcunit.Stop(ctx, s.Env, s.Old)
			record(r)
			return err
		}, func() error {
			if !snap.Active {
				return nil
			}
			_, err := svcunit.Start(ctx, s.Env, s.Old)
			return err
		}},
		{"moving the directories", func() error {
			moved, err := s.Mover.Move(pairs)
			for _, p := range moved.Moved {
				actions = append(actions, "move "+p.Old+" "+p.New)
			}
			return err
		}, func() error { _, err := s.Mover.Move(back); return err }},
		{"installing the new service", func() error {
			r, err := svcunit.Install(ctx, s.Env, spec)
			result = r
			record(r)
			switch {
			case err != nil:
			case snap.Active:
				err = s.settled(ctx, spec)
			default: // it was stopped: it stays installed, and stopped
				var stopped svcunit.Result
				stopped, err = svcunit.Stop(ctx, s.Env, spec)
				record(stopped)
			}
			return err
		}, func() error { _, err := svcunit.Uninstall(ctx, s.Env, spec); return err }},
		{"removing " + s.Old.Name, func() error {
			r, err := svcunit.Uninstall(ctx, s.Env, s.Old)
			record(r)
			return err
		}, nil},
	}
	for i, st := range steps {
		if err := st.do(); err != nil {
			err = fmt.Errorf("%s: %w", st.name, err)
			if undone := s.undo(steps[:i+1]); undone != nil {
				return svcunit.Result{}, errors.Join(err, fmt.Errorf("putting the old service back failed: %w", undone),
					errors.New("run `install-service` again to go on, or put the old service back by hand"))
			}
			progress.remove()
			return svcunit.Result{}, fmt.Errorf("%w; nothing was changed: %s runs as it did", err, s.Old.Name)
		}
	}
	progress.remove()
	result.Actions, result.Changed = actions, true
	return result, nil
}

// undo puts back what the steps did, last first, and says what could not be put back.
func (s Service) undo(done []step) error {
	var errs []error
	for _, st := range slices.Backward(done) {
		if st.undo == nil {
			continue
		}
		if err := st.undo(); err != nil {
			errs = append(errs, fmt.Errorf("undoing %s: %w", st.name, err))
		}
	}
	return errors.Join(errs...)
}

// settled says the new service is still running after it had time to fail.
func (s Service) settled(ctx context.Context, spec svcunit.Spec) error {
	wait := s.Settle
	if wait == nil {
		wait = func(ctx context.Context) error {
			select {
			case <-time.After(2 * time.Second):
				return nil
			case <-ctx.Done():
				return ctx.Err()
			}
		}
	}
	if err := wait(ctx); err != nil {
		return err
	}
	in, err := svcunit.Inspect(ctx, s.Env, spec)
	switch {
	case err != nil:
		return err
	case !in.Active:
		return fmt.Errorf("%s is not running", spec.Name)
	}
	return nil
}

// progressFile is the record of a migration that is under way, in the data
// directory: the old one while it has not moved, the new one after.
type progressFile struct{ dirs Dirs }

const progressName = ".cflk-service-migration.json"

func (p progressFile) paths() []string {
	return []string{filepath.Join(p.dirs.OldData, progressName), filepath.Join(p.dirs.NewData, progressName)}
}

func (p progressFile) read() (Snapshot, bool, error) {
	for _, path := range p.paths() {
		b, err := os.ReadFile(path) //nolint:gosec // the progress file, at a path built from the old service's own data directory
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return Snapshot{}, false, fmt.Errorf("migrate: %w", err)
		}
		var snap Snapshot
		if err := json.Unmarshal(b, &snap); err != nil {
			return Snapshot{}, false, fmt.Errorf("migrate: reading %s: %w", path, err)
		}
		return snap, true, nil
	}
	return Snapshot{}, false, nil
}

// write records the migration where the data is now: if the data directory
// does not exist yet there is nothing to record against, and nothing to resume.
func (p progressFile) write(snap Snapshot) error {
	b, err := json.Marshal(snap)
	if err != nil {
		return err
	}
	for _, dir := range []string{p.dirs.OldData, p.dirs.NewData} {
		if info, err := os.Stat(dir); err == nil && info.IsDir() {
			return atomicfile.Write(filepath.Join(dir, progressName), b, 0o600)
		}
	}
	return nil
}

func (p progressFile) remove() {
	for _, path := range p.paths() {
		_ = os.Remove(path)
	}
}
