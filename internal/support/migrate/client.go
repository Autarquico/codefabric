package migrate

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"

	"golang.org/x/sys/unix"

	"github.com/Autarquico/codeflock/internal/config"
)

// ClientEnv is what the migration of the client reads from the machine. The
// zero value is the real one.
type ClientEnv struct {
	Getenv func(string) string
	Home   func() (string, error)
	GOOS   string
	// Args are the arguments of the command that runs, without the program.
	Args []string
	// Mover is where tests inject failures.
	Mover Mover
}

func (e ClientEnv) withDefaults() ClientEnv {
	if e.Getenv == nil {
		e.Getenv = os.Getenv
	}
	if e.Home == nil {
		e.Home = os.UserHomeDir
	}
	if e.GOOS == "" {
		e.GOOS = runtime.GOOS
	}
	return e
}

// Client moves the directories of the client of this user from the name
// CodeFabric to Code Flock, once, and says so on out. Only the default
// directories move: one given by CFLK_*_DIR or CF_*_DIR is the person's, and is
// not touched. With a service of the old name installed nothing moves: its
// install-service does it, with the service stopped, and config.Resolve says so.
// Nothing moves either for a command that only asks for help, while another
// migration holds its lock, or while another process holds the old data
// directory (see holdData).
//
// A pair that clashes is a warning, not an error: the command goes on with the
// new directories. Any other failure is an error and the command should not run.
//
// The release it returns is for the caller to call when the command ends: for
// `<kind> run` it keeps the data directory held for as long as the daemon lives,
// so that no other command moves it from under it.
func Client(e ClientEnv, out io.Writer) (release func(), err error) {
	e = e.withDefaults()
	if asksForHelp(e.Args) {
		return func() {}, nil
	}
	if err := moveClient(e, out); err != nil {
		return func() {}, err
	}
	if runKind(e.Args) == "" {
		return func() {}, nil
	}
	return holdDataInUse(e), nil
}

// holdDataInUse holds the data directory a daemon really uses, wherever it
// comes from: a default, a CFLK_DATA_DIR or CF_DATA_DIR, or the old directory
// kept for lack of a new one. A daemon that cannot take it runs all the same:
// the Worker and the Controller of one machine share the directory, and whoever
// holds it keeps it from moving.
func holdDataInUse(e ClientEnv) func() {
	paths, err := config.Resolve(e.Getenv, e.Home)
	if err != nil {
		return func() {}
	}
	hold, err := holdData(paths.DataDir)
	if err != nil {
		return func() {}
	}
	return hold
}

// moveClient moves what there is to move, holding the old data directory
// while it does.
func moveClient(e ClientEnv, out io.Writer) error {
	loc, err := config.Locate(e.Getenv, e.Home)
	if err != nil {
		return nil // the command reports it
	}
	pairs, err := clientPairs(e, loc)
	if err != nil || len(pairs) == 0 {
		return nil
	}
	if !anyOldExists(pairs) {
		return nil
	}
	home, err := e.Home()
	if err != nil {
		return nil
	}
	// With a service of the old name installed nothing moves: its install-service
	// does, and config.Resolve says so.
	if len(config.OldUnits(home, e.GOOS)) > 0 {
		return nil
	}
	running := runKind(e.Args)
	if held := daemonsOn(config.Paths{DataDir: dataPair(pairs, loc)}); len(held) > 0 {
		if len(held) > 1 || held[0] != running {
			for _, kind := range held {
				_, _ = fmt.Fprintf(out, "cflk: a %s uses %s; it moves when it starts as `cflk %s run`, or with `cflk %s install-service`\n",
					strings.ToUpper(kind[:1])+kind[1:], dataPair(pairs, loc), kind, kind)
			}
			return nil
		}
	}
	unlock, err := lockMigration(home)
	if err != nil {
		_, _ = fmt.Fprintln(out, "cflk: not moving the directories now: "+err.Error())
		return nil
	}
	defer unlock()
	hold, err := holdData(dataPair(pairs, loc))
	if err != nil {
		_, _ = fmt.Fprintln(out, "cflk: not moving the directories now: "+err.Error())
		return nil
	}
	defer hold()
	res, err := e.Mover.Move(pairs)
	var clash *ClashError
	switch {
	case errors.As(err, &clash):
		_, _ = fmt.Fprintln(out, "cflk: "+clash.Error())
		return nil
	case err != nil:
		return err
	}
	for _, p := range res.Moved {
		_, _ = fmt.Fprintf(out, "cflk: moved %s to %s\n", p.Old, p.New)
	}
	return nil
}

// asksForHelp says whether the command only prints help: `--help`, `--help=...`, `-h` or
// `help` among its arguments. Cobra answers it, and nothing should move first.
func asksForHelp(args []string) bool {
	return slices.ContainsFunc(args, func(a string) bool {
		return a == "--help" || a == "-h" || a == "help" || strings.HasPrefix(a, "--help=")
	})
}

const daemonLockName = ".cflk-daemon.lock"

// holdData takes, without waiting, the lock of the process that has the data
// directory dir open: a daemon holds it as long as it lives, and so does the
// process that moves the directory, whose lock goes with it (a rename keeps the
// file). A process that cannot take it leaves the directory where it is. A
// directory that is not there needs none.
func holdData(dir string) (release func(), err error) {
	if dir == "" {
		return func() {}, nil
	}
	if _, err := os.Stat(dir); err != nil {
		return func() {}, nil //nolint:nilerr // nothing to hold: the move finds out what is not there
	}
	path := filepath.Join(dir, daemonLockName)
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o600) //nolint:gosec // the lock file of the process that has the data directory open
	if err != nil {
		return nil, fmt.Errorf("migrate: %w", err)
	}
	if err := unix.Flock(int(f.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil { //nolint:gosec // a descriptor fits an int
		_ = f.Close()
		if errors.Is(err, unix.EWOULDBLOCK) {
			return nil, fmt.Errorf("another cflk has %s open (it holds %s)", dir, path)
		}
		return nil, fmt.Errorf("migrate: locking %s: %w", path, err)
	}
	return func() { _ = f.Close() }, nil
}

// dataPair is the old data directory among pairs: the one a daemon of the old
// name keeps its state in.
func dataPair(pairs []Pair, loc config.Located) string {
	for _, p := range pairs {
		if p.New == loc.DataDir {
			return p.Old
		}
	}
	return ""
}

// daemonsOn are the daemons whose state is in the old data directory: the
// Worker has its own subdirectory, the Controller its database. A daemon may
// be running under a supervisor of its own that cflk cannot see, so what moves
// the directory is that daemon starting under the new name.
func daemonsOn(old config.Paths) []string {
	if old.DataDir == "" {
		return nil
	}
	var kinds []string
	if _, err := os.Lstat(old.WorkerDataDir()); err == nil {
		kinds = append(kinds, "worker")
	}
	if _, err := os.Lstat(old.DatabaseFile()); err == nil {
		kinds = append(kinds, "controller")
	}
	return kinds
}

// runKind is "worker" or "controller" if args are `<kind> run`, whatever the
// global flags before it: --controller takes a value, the others do not.
func runKind(args []string) string {
	var words []string
	for i := 0; i < len(args) && len(words) < 2; i++ {
		switch a := args[i]; {
		case a == "--controller":
			i++
		case strings.HasPrefix(a, "-"):
		default:
			words = append(words, a)
		}
	}
	if len(words) == 2 && words[1] == "run" && (words[0] == "worker" || words[0] == "controller") {
		return words[0]
	}
	return ""
}

// clientPairs are the default directories of the client, and, on macOS, the
// logs of launchd, which sit with them.
func clientPairs(e ClientEnv, loc config.Located) ([]Pair, error) {
	var pairs []Pair
	if loc.ConfigIsDefault {
		pairs = append(pairs, Pair{Old: config.LegacyOf(loc.ConfigDir), New: loc.ConfigDir})
	}
	if loc.DataIsDefault {
		pairs = append(pairs, Pair{Old: config.LegacyOf(loc.DataDir), New: loc.DataDir})
	}
	if len(pairs) > 0 && e.GOOS == "darwin" {
		home, err := e.Home()
		if err != nil {
			return nil, err
		}
		logs := filepath.Join(home, "Library", "Logs")
		pairs = append(pairs, Pair{Old: filepath.Join(logs, "codefabric"), New: filepath.Join(logs, "codeflock")})
	}
	return pairs, nil
}

func anyOldExists(pairs []Pair) bool {
	for _, p := range pairs {
		if _, err := os.Lstat(p.Old); err == nil {
			return true
		}
	}
	return false
}
