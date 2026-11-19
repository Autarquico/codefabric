package migrate

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"

	"github.com/Autarquico/codeflock/internal/config"
)

// ClientEnv is what the migration of the client reads from the machine. The
// zero value is the real one.
type ClientEnv struct {
	Getenv func(string) string
	Home   func() (string, error)
	GOOS   string
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
// install-service does it, with the service stopped, and Client says so.
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
	if units := OldUnits(home, e.GOOS); len(units) > 0 {
		for _, u := range units {
			_, _ = fmt.Fprintf(out, "cflk: the service %s still uses the old codefabric directories, so they were not moved; "+
				"run `cflk %s install-service` to move them with the service\n", u.Name, u.Kind)
		}
		return nil
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
