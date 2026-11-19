// Command cf-transition is published as `cf` in the 0.4 tarballs: it replaces the beta `cf` of Code Flock and
// hands over to `cflk`. It disappears in 0.5.
package main

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"

	"golang.org/x/term"
)

const target = "cflk"

// The installer asks a cf who it is by setting probeVar: the transition cf answers probeAnswer and exits, without
// running cflk. Any other cf ignores the variable, so the answer says "I am a transition cf" whatever my version.
const (
	probeVar    = "CF_TRANSITION_PROBE"
	probeAnswer = "code-flock-cf-transition"
)

func main() {
	if os.Getenv(probeVar) != "" {
		fmt.Println(probeAnswer)
		return
	}
	self, _ := os.Executable()
	path, err := locate(self)
	if err != nil {
		fmt.Fprintln(os.Stderr, "cf: this command is now `cflk`, and cflk was not found next to cf or on PATH; reinstall Code Flock")
		os.Exit(127)
	}
	if term.IsTerminal(int(os.Stderr.Fd())) {
		fmt.Fprintln(os.Stderr, "cf: this command is now `cflk` (cf is a temporary alias that goes away in 0.5); running cflk")
	}
	// exec replaces this process, so signals, stdio and the exit code are cflk's own.
	err = syscall.Exec(path, append([]string{target}, os.Args[1:]...), os.Environ()) //nolint:gosec // the whole job: path is the cflk beside cf or on PATH
	fmt.Fprintf(os.Stderr, "cf: cannot run %s: %v\n", path, err)
	os.Exit(126)
}

// locate finds cflk in the directory of the running executable (symlinks resolved), else on PATH.
func locate(self string) (string, error) {
	if self != "" {
		if resolved, err := filepath.EvalSymlinks(self); err == nil {
			self = resolved
		}
		sibling := filepath.Join(filepath.Dir(self), target)
		if info, err := os.Stat(sibling); err == nil && info.Mode().IsRegular() && info.Mode().Perm()&0o111 != 0 {
			return sibling, nil
		}
	}
	path, err := exec.LookPath(target)
	if err != nil {
		return "", errors.New("cflk not found")
	}
	return path, nil
}
