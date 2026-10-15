//go:build e2e && (linux || darwin)

package e2e

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/Autarquico/codefabric/internal/support/gittest"
)

// The knobs of a run of the suite, all read once by TestMain.
const (
	// envBin names a cf binary to use instead of building one.
	envBin = "CF_E2E_BIN"
	// envRace=1 builds cf with -race; every process of the cluster is then
	// instrumented. It cannot be combined with envBin: whether a binary given
	// from outside was built with -race cannot be told, and a race leg that ran
	// an uninstrumented cf would prove nothing.
	envRace = "CF_E2E_RACE"
	// envRoot is the directory the root of an invocation is made in.
	envRoot = "CF_E2E_ROOT"
	// envKeep=1 keeps the files of the invocation; its processes go anyway.
	envKeep = "CF_E2E_KEEP"
	// envArtifacts is where the logs of a failed invocation are copied.
	envArtifacts = "CF_E2E_ARTIFACTS"
)

// buildTimeout bounds `go build` of cf, with -race on a slow machine included.
const buildTimeout = 10 * time.Minute

// binary is the cf every process of the suite runs, and race whether it was
// built with -race. TestMain sets both before any test runs.
var (
	binary string
	race   bool
)

// resolveBinary returns the cf of the invocation: the one envBin names or,
// without it, one built into dir.
func resolveBinary(dir string) (path string, withRace bool, err error) {
	withRace = os.Getenv(envRace) == "1"
	if given := os.Getenv(envBin); given != "" {
		if withRace {
			return "", false, fmt.Errorf("e2e: %s=1 builds its own cf; unset %s", envRace, envBin)
		}
		path, err := filepath.Abs(given)
		if err != nil {
			return "", false, err
		}
		info, err := os.Stat(path) //nolint:gosec // the binary the developer named
		if err != nil {
			return "", false, fmt.Errorf("e2e: %s: %w", envBin, err)
		}
		if info.IsDir() || info.Mode()&0o111 == 0 {
			return "", false, fmt.Errorf("e2e: %s=%s is not an executable", envBin, path)
		}
		return path, false, nil
	}
	path, err = build(dir, withRace)
	return path, withRace, err
}

// build compiles cmd/cf of this module into dir.
func build(dir string, withRace bool) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), buildTimeout)
	defer cancel()
	out := filepath.Join(dir, "cf")
	args := []string{"build", "-o", out}
	env := os.Environ()
	if withRace {
		args = append(args, "-race")
		env = append(env, "CGO_ENABLED=1")
	}
	args = append(args, "github.com/Autarquico/codefabric/cmd/cf")
	cmd := exec.CommandContext(ctx, "go", args...) //nolint:gosec // go build of this module
	cmd.Env = env
	if msg, err := cmd.CombinedOutput(); err != nil {
		return "", fmt.Errorf("e2e: go %s: %w\n%s", strings.Join(args, " "), err, msg)
	}
	if _, err := os.Stat(out); err != nil {
		return "", errors.Join(errors.New("e2e: go build left no binary"), err)
	}
	return out, nil
}

// childEnv is the whole environment of a process of the cluster, built from
// nothing: what the developer's shell holds (CF_*, XDG_*, TMUX, a real HOME)
// never reaches it. dirs are the box's.
func childEnv(dirs boxDirs, extra ...string) []string {
	env := map[string]string{}
	for _, kv := range gittest.Env(dirs.home) {
		k, v, _ := strings.Cut(kv, "=")
		env[k] = v
	}
	set := func(k, v string) { env[k] = v }
	set("PATH", os.Getenv("PATH"))
	set("TMPDIR", dirs.tmp)
	set("LANG", "C")
	set("LC_ALL", "C")
	set("HOME", dirs.home)
	set("TERM", "xterm-256color")
	set("CF_CONFIG_DIR", dirs.config)
	set("CF_DATA_DIR", dirs.data)
	set("TMUX_TMPDIR", dirs.tmux)
	if race {
		// A binary built with -race waits a second before it exits; every
		// wrapper would end a second late.
		set("GORACE", "atexit_sleep_ms=0")
	}
	for _, kv := range extra {
		k, v, _ := strings.Cut(kv, "=")
		env[k] = v
	}
	out := make([]string, 0, len(env))
	for k, v := range env {
		out = append(out, k+"="+v)
	}
	slices.Sort(out)
	return out
}
