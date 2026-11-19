package migrate

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
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

// legacyDirs are the directories a service of the old name uses when its
// environment names none.
func legacyDirs(home string) (cfg, data string) {
	return filepath.Join(home, ".config", "codefabric"), filepath.Join(home, ".local", "share", "codefabric")
}

// uses says whether a service with this environment uses a directory that is
// about to move: it would go on writing to where it was.
func (d Dirs) uses(env map[string]string, home string) bool {
	cfg, data := legacyDirs(home)
	return d.OldConfig != d.NewConfig && cmp.Or(env["CF_CONFIG_DIR"], cfg) == d.OldConfig ||
		d.OldData != d.NewData && cmp.Or(env["CF_DATA_DIR"], data) == d.OldData
}

// Adopt moves the directories of an install-service with no old service to
// move them: it is run from a machine whose default directories still have the
// old name, which are the ones config.Resolve then gives. Only those move: a
// directory given by CFLK_*_DIR or CF_*_DIR is the person's. It refuses while
// another service of the old name uses them, and while runs, if given, refuses.
// It returns the new paths.
func (m Mover) Adopt(ctx context.Context, env svcunit.Env, cfg, data string, runs func(context.Context, Dirs) error) (newCfg, newData string, err error) {
	dirs := Dirs{OldConfig: cfg, NewConfig: cfg, OldData: data, NewData: data}
	loc, err := config.Locate(os.Getenv, func() (string, error) { return env.Home, nil })
	if err != nil {
		return cfg, data, nil // the command reports it
	}
	if loc.ConfigIsDefault && cfg == config.LegacyOf(loc.ConfigDir) {
		dirs.NewConfig = loc.ConfigDir
	}
	if loc.DataIsDefault && data == config.LegacyOf(loc.DataDir) {
		dirs.NewData = loc.DataDir
	}
	pairs := dirs.pairs(env)
	if len(pairs) == 0 {
		return cfg, data, nil
	}
	others, err := peers(ctx, env, dirs, "", "")
	if err != nil {
		return "", "", err
	}
	if len(others) > 0 {
		return "", "", fmt.Errorf("%s uses the directories about to move: run `cflk %s install-service` first, which moves it",
			others[0].spec.Name, config.KindOf(others[0].spec.Name))
	}
	if runs != nil {
		if err := runs(ctx, dirs); err != nil {
			return "", "", err
		}
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

// LiveRuns refuses, with an error that says why, while the Worker whose data
// directory is dataDir and whose tmux uses tmuxTmpDir has Runs live (or cannot
// be asked): any move of that directory would lose them.
type LiveRuns func(ctx context.Context, tmuxTmpDir, dataDir string) error

// Service moves one service from the old name to the new one, and the
// directories it uses with it, as steps that each can be put back. Another
// service of the old name on the same directories (a Controller and a Worker
// share a root) is moved in the same run, since it could not be left behind: if
// it cannot be moved, nothing is.
type Service struct {
	Env svcunit.Env
	Old svcunit.Spec
	// New is the unit that replaces the old one.
	New func(old Snapshot, dirs Dirs) (svcunit.Spec, error)
	// Binary is the cflk the units moved along with this one run; empty is this executable.
	Binary string
	// LiveRuns is how a Worker is checked for live Runs, before anything
	// changes: the Worker side supplies it. nil refuses to move a Worker.
	LiveRuns LiveRuns
	// Settle waits until the new service has had time to fail; nil is two seconds.
	Settle func(ctx context.Context) error
	// Mover is where tests inject failures.
	Mover Mover
}

// member is one of the services of a migration.
type member struct {
	old  svcunit.Spec
	kind string
	snap Snapshot
	live bool // running now, whatever snap says
}

func newMember(old svcunit.Spec, in svcunit.Installed) member {
	return member{old: old, kind: config.KindOf(old.Name), snap: Snapshot{Active: in.Active, Env: in.Env, Argv: in.Argv}, live: in.Active}
}

// peer is another service of the old name that uses the directories about to move.
type peer struct {
	spec svcunit.Spec
	in   svcunit.Installed
}

// peers are the services of the old name, other than self, that use the
// directories about to move, running or not. One that cannot be read, or that
// cflk cannot move, is an error: what is not known is not safe.
func peers(ctx context.Context, env svcunit.Env, dirs Dirs, self, binary string) ([]peer, error) {
	var out []peer
	for _, u := range config.OldUnits(env.Home, env.GOOS) {
		if u.Name == self {
			continue
		}
		if u.System || env.GOOS == "darwin" && u.Kind == "controller" {
			if err := unmanagedPeer(u, dirs, env.Home, binary); err != nil {
				return nil, err
			}
			continue
		}
		spec := svcunit.Spec{Name: u.Name, Label: u.Name, KeepProcesses: u.Kind == "worker"}
		in, err := svcunit.Inspect(ctx, env, spec)
		switch {
		case err != nil:
			return nil, fmt.Errorf("%s cannot be read, so it is not known whether it uses the directories about to move: nothing was changed: %w", u.Name, err)
		case !in.Found:
			continue
		case dirs.uses(in.Env, env.Home):
			out = append(out, peer{spec: spec, in: in})
		}
	}
	return out, nil
}

// unmanagedPeer refuses a service on the directories that cflk cannot move: a
// LaunchDaemon of the system, which only root can change, or a Controller under
// launchd, which cflk never installs on macOS and so never writes. Nothing
// changes; the commands to move it are the person's.
func unmanagedPeer(u config.Unit, dirs Dirs, home, binary string) error {
	unreadable := func(err error) error {
		return fmt.Errorf("%s cannot be read, so it is not known whether it uses the directories about to move: nothing was changed: %w", u.Path, err)
	}
	content, err := os.ReadFile(u.Path) //nolint:gosec // the property list of an old service
	if err != nil {
		return unreadable(err)
	}
	info, err := svcunit.ReadPlist(content)
	if err != nil {
		return unreadable(err)
	}
	if !dirs.uses(info.Env, home) {
		return nil
	}
	if u.System {
		return fmt.Errorf("%s is a LaunchDaemon of the system on the same directories, which only root can move: nothing was changed; "+
			"run `cflk %s install-service` to see how to move it, and do that first", u.Path, u.Kind)
	}
	program, _, err := svcunit.Program(binary)
	if err != nil {
		return err
	}
	commands, err := LaunchPlan{Plist: u.Path, Content: content, Binary: program, UserHome: home}.Commands()
	if err != nil {
		return fmt.Errorf("%s is a Controller under launchd on the same directories and cannot be moved: %w", u.Path, err)
	}
	return fmt.Errorf("%s is a Controller under launchd on the same directories, which cflk does not manage: nothing was changed. "+
		"To move it, run, in order, and then run this again:\n\n%s", u.Path, strings.Join(commands, "\n"))
}

// Migrate runs the move; the new services run if the old ones did. If a step
// fails the ones before it are undone in reverse order, the old services run
// again if they did, and the error says what failed; if undoing fails too, the
// error says that, and the progress file stays so that running it again goes
// on. The result is that of the new unit of s, with the actions of the whole
// move in front.
func (s Service) Migrate(ctx context.Context) (svcunit.Result, error) {
	in, err := svcunit.Inspect(ctx, s.Env, s.Old)
	switch {
	case err != nil:
		return svcunit.Result{}, err
	case !in.Found:
		return svcunit.Result{}, ErrNoOldService
	}
	dirs, err := DirsOf(in.Env)
	if err != nil {
		return svcunit.Result{}, err
	}
	others, err := peers(ctx, s.Env, dirs, s.Old.Name, s.Binary)
	if err != nil {
		return svcunit.Result{}, err
	}
	members := []member{newMember(s.Old, in)}
	for _, p := range others {
		members = append(members, newMember(p.spec, p.in))
	}
	progress := progressFile{dirs: dirs}
	saved, err := progress.read()
	if err != nil {
		return svcunit.Result{}, err
	}
	_, resumed := saved[s.Old.Name]
	if resumed {
		for i, m := range members {
			if snap, ok := saved[m.old.Name]; ok {
				members[i].snap = snap
			}
		}
	}
	// A cut migration goes on without asking again, unless an old service
	// runs once more: it may have taken Runs since.
	if !resumed || slices.ContainsFunc(members, func(m member) bool { return m.live }) {
		if err := s.begin(ctx, members); err != nil {
			return svcunit.Result{}, err
		}
	}
	specs := make([]svcunit.Spec, len(members))
	for i, m := range members {
		if specs[i], err = s.successor(i, m, dirs); err != nil {
			return svcunit.Result{}, err
		}
	}
	if err := progress.write(members); err != nil {
		return svcunit.Result{}, err
	}
	return s.run(ctx, members, specs, dirs, progress)
}

// begin refuses, before anything changes, a Worker with Runs live.
func (s Service) begin(ctx context.Context, members []member) error {
	home := s.Env.Home
	_, defaultData := legacyDirs(home)
	for _, m := range members {
		if m.kind != "worker" {
			continue
		}
		if s.LiveRuns == nil {
			return fmt.Errorf("%s is a Worker and there is no way to tell whether Runs are live on it: nothing was changed", m.old.Name)
		}
		if err := s.LiveRuns(ctx, m.snap.Env["TMUX_TMPDIR"], cmp.Or(m.snap.Env["CF_DATA_DIR"], defaultData)); err != nil {
			return err
		}
	}
	return nil
}

// successor is the unit that replaces a member: the first is the caller's own,
// the others carry over what their old unit had, with the new names and the
// directories where they are now.
func (s Service) successor(i int, m member, dirs Dirs) (svcunit.Spec, error) {
	if i == 0 {
		return s.New(m.snap, dirs)
	}
	binary, _, err := svcunit.Program(s.Binary)
	if err != nil {
		return svcunit.Spec{}, err
	}
	if len(m.snap.Argv) == 0 {
		return svcunit.Spec{}, fmt.Errorf("%s says no program to run, so it cannot be moved", m.old.Name)
	}
	cfg, data := legacyDirs(s.Env.Home)
	where := func(have, old, now string) string {
		if have == old {
			return now
		}
		return have
	}
	env := []svcunit.KV{
		{Key: "CFLK_CONFIG_DIR", Value: where(cmp.Or(m.snap.Env["CF_CONFIG_DIR"], cfg), dirs.OldConfig, dirs.NewConfig)},
		{Key: "CFLK_DATA_DIR", Value: where(cmp.Or(m.snap.Env["CF_DATA_DIR"], data), dirs.OldData, dirs.NewData)},
	}
	for _, key := range slices.Sorted(maps.Keys(m.snap.Env)) {
		if key != "CF_CONFIG_DIR" && key != "CF_DATA_DIR" {
			env = append(env, svcunit.KV{Key: key, Value: m.snap.Env[key]})
		}
	}
	return svcunit.Spec{
		Name:          "codeflock-" + strings.TrimPrefix(strings.TrimPrefix(m.old.Name, "cf-"), "com.codefabric."),
		Label:         strings.Replace(m.old.Label, "com.codefabric.", "com.codeflock.", 1),
		Description:   "Code Flock " + strings.ToUpper(m.kind[:1]) + m.kind[1:],
		Installer:     "cflk " + m.kind + " install-service",
		Argv:          append([]string{binary}, m.snap.Argv[1:]...),
		Env:           env,
		KeepProcesses: m.kind == "worker",
	}, nil
}

type step struct {
	name string
	do   func() error
	undo func() error
}

func (s Service) run(ctx context.Context, members []member, specs []svcunit.Spec, dirs Dirs, progress progressFile) (svcunit.Result, error) {
	var (
		actions []string
		result  svcunit.Result
		pairs   = dirs.pairs(s.Env)
		names   []string
	)
	back := make([]Pair, len(pairs))
	for i, p := range pairs {
		back[len(pairs)-1-i] = Pair{Old: p.New, New: p.Old}
	}
	record := func(r svcunit.Result) { actions = append(actions, r.Actions...) }
	var steps []step
	for _, m := range members {
		names = append(names, m.old.Name)
		steps = append(steps, step{"stopping " + m.old.Name, func() error {
			r, err := svcunit.Stop(ctx, s.Env, m.old)
			record(r)
			return err
		}, func() error {
			if !m.snap.Active {
				return nil
			}
			_, err := svcunit.Start(ctx, s.Env, m.old)
			return err
		}})
	}
	steps = append(steps, step{"moving the directories", func() error {
		moved, err := s.Mover.Move(pairs)
		for _, p := range moved.Moved {
			actions = append(actions, "move "+p.Old+" "+p.New)
		}
		return err
	}, func() error { _, err := s.Mover.Move(back); return err }})
	for i, m := range members {
		spec := specs[i]
		steps = append(steps, step{"installing " + spec.Name, func() error {
			r, err := svcunit.Install(ctx, s.Env, spec)
			if i == 0 {
				result = r
			}
			record(r)
			switch {
			case err != nil:
			case m.snap.Active:
				err = s.settled(ctx, spec)
			default: // it was stopped: it stays installed, and stopped
				var stopped svcunit.Result
				stopped, err = svcunit.Stop(ctx, s.Env, spec)
				record(stopped)
			}
			return err
		}, func() error { _, err := svcunit.Uninstall(ctx, s.Env, spec); return err }})
	}
	for i, st := range steps {
		if err := st.do(); err != nil {
			err = fmt.Errorf("%s: %w", st.name, err)
			if undone := s.undo(steps[:i+1]); undone != nil {
				return svcunit.Result{}, errors.Join(err, fmt.Errorf("putting the old service back failed: %w", undone),
					errors.New("run `install-service` again to go on, or put the old service back by hand"))
			}
			progress.remove()
			return svcunit.Result{}, fmt.Errorf("%w; nothing was changed: %s run as they did", err, strings.Join(names, ", "))
		}
	}
	// Every new service answers: the move is done, and what is left is tidying,
	// which cannot take it back.
	for _, m := range members {
		r, err := svcunit.Uninstall(ctx, s.Env, m.old)
		record(r)
		if err != nil {
			result.Warnings = append(result.Warnings, s.leftBehind(m.old, r.Path, err))
		}
	}
	progress.remove()
	result.Actions, result.Changed = actions, true
	return result, nil
}

// leftBehind says that an old service could not be removed once the new ones
// run: it stays stopped, and the command to take it away is the person's.
func (s Service) leftBehind(old svcunit.Spec, path string, err error) string {
	by := "systemctl --user disable --now " + old.Name + ".service && rm " + shellQuote(path)
	if s.Env.GOOS == "darwin" {
		by = "launchctl bootout gui/" + strconv.Itoa(s.Env.UID) + "/" + old.Label + "; rm " + shellQuote(path)
	}
	return fmt.Sprintf("the new services run, but the old %s could not be removed (%v); it stays stopped: remove it by hand with `%s`", old.Name, err, by)
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
// directory: the old one while it has not moved, the new one after. It is of
// the whole migration, so it names every service in it: one that does not
// name the service being moved is not its own.
type progressFile struct{ dirs Dirs }

const progressName = ".cflk-service-migration.json"

func (p progressFile) paths() []string {
	return []string{filepath.Join(p.dirs.OldData, progressName), filepath.Join(p.dirs.NewData, progressName)}
}

// read is what the cut migration saved, by service: nil if there is none.
func (p progressFile) read() (map[string]Snapshot, error) {
	for _, path := range p.paths() {
		b, err := os.ReadFile(path) //nolint:gosec // the progress file, at a path built from the old service's own data directory
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return nil, fmt.Errorf("migrate: %w", err)
		}
		var saved map[string]Snapshot
		if err := json.Unmarshal(b, &saved); err != nil {
			return nil, fmt.Errorf("migrate: reading %s: %w", path, err)
		}
		return saved, nil
	}
	return nil, nil
}

// write records the migration where the data is now: if the data directory
// does not exist yet there is nothing to record against, and nothing to resume.
func (p progressFile) write(members []member) error {
	saved := make(map[string]Snapshot, len(members))
	for _, m := range members {
		saved[m.old.Name] = m.snap
	}
	b, err := json.Marshal(saved)
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
