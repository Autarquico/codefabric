// Package gitbin finds the git executable and checks it is new enough for what
// CodeFabric asks of it. The Controller (git relay) and the Worker (gitops) both
// need git 2.31 or later: that is the first version that reads its configuration
// from GIT_CONFIG_COUNT, which is how credentials reach git without argv or files.
package gitbin

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// Minimum is the oldest git that CodeFabric supports.
const (
	MinMajor = 2
	MinMinor = 31
)

// versionTimeout is an infrastructure deadline on a child process, so it runs on real time.
const versionTimeout = 10 * time.Second

// ErrTooOld is returned for a git older than Minimum.
var ErrTooOld = errors.New("gitbin: git is older than 2.31")

var versionLine = regexp.MustCompile(`^git version (\d+)\.(\d+)`)

// Locate returns the path of the git executable and its version, or an error
// that says why this machine cannot serve git: not installed, or too old.
func Locate(ctx context.Context) (path string, major, minor int, err error) {
	path, err = exec.LookPath("git")
	if err != nil {
		return "", 0, 0, fmt.Errorf("gitbin: git is not installed: %w", err)
	}
	major, minor, err = Check(ctx, path)
	return path, major, minor, err
}

// Check runs the git at path and requires it to be new enough.
func Check(ctx context.Context, path string) (major, minor int, err error) {
	major, minor, err = Version(ctx, path)
	if err != nil {
		return 0, 0, err
	}
	if major < MinMajor || (major == MinMajor && minor < MinMinor) {
		return major, minor, fmt.Errorf("%w (found %d.%d)", ErrTooOld, major, minor)
	}
	return major, minor, nil
}

// Version runs `git version`.
func Version(ctx context.Context, path string) (major, minor int, err error) {
	ctx, cancel := context.WithTimeout(ctx, versionTimeout)
	defer cancel()
	out, err := exec.CommandContext(ctx, path, "version").Output() //nolint:gosec // path comes from LookPath
	if err != nil {
		return 0, 0, fmt.Errorf("gitbin: running git version: %w", err)
	}
	return ParseVersion(string(out))
}

// ParseVersion reads the output of `git version`.
func ParseVersion(out string) (major, minor int, err error) {
	m := versionLine.FindStringSubmatch(strings.TrimSpace(out))
	if m == nil {
		return 0, 0, fmt.Errorf("gitbin: cannot read the version from %q", strings.TrimSpace(out))
	}
	major, _ = strconv.Atoi(m[1])
	minor, _ = strconv.Atoi(m[2])
	return major, minor, nil
}
