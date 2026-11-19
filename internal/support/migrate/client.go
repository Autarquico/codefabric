package migrate

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"

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
//
// A pair that clashes is a warning, not an error: the command goes on with the
// new directories. Any other failure is an error and the command should not run.
func Client(e ClientEnv, out io.Writer) error {
	e = e.withDefaults()
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
	if held := daemonsOn(config.Paths{DataDir: dataPair(pairs, loc)}); len(held) > 0 {
		if running := runKind(e.Args); len(held) > 1 || held[0] != running {
			for _, kind := range held {
				_, _ = fmt.Fprintf(out, "cflk: a %s uses %s; it moves when it starts as `cflk %s run`, or with `cflk %s install-service`\n",
					strings.ToUpper(kind[:1])+kind[1:], dataPair(pairs, loc), kind, kind)
			}
			return nil
		}
	}
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
