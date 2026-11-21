// Package attachwindow opens a new terminal window that runs a command, for the
// callers that have no terminal of their own (`cflk attach` without a TTY). It knows tmux, iTerm2 and Terminal.app.
//
// Nothing is interpreted: the command is written, quoted for sh, in a script of
// a private directory, and the window is only told to run `/bin/sh <script>`, a
// path of a controlled alphabet. So neither the shell of the person (fish,
// nushell) nor the parser of tmux or AppleScript ever sees the arguments.
package attachwindow

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/Autarquico/codeflock/internal/domain"
)

// Launcher is a terminal that can open a window.
type Launcher string

// The launchers, in the order Detect prefers them.
const (
	Tmux     Launcher = "tmux"
	ITerm2   Launcher = "iterm2"
	Terminal Launcher = "terminal"
)

// Label is how a person names the launcher: "tmux", "iTerm2" or "Terminal".
func (l Launcher) Label() string {
	switch l {
	case ITerm2:
		return "iTerm2"
	case Terminal:
		return "Terminal"
	}
	return string(l)
}

// appName is the name AppleScript knows the application by.
func (l Launcher) appName() string {
	if l == ITerm2 {
		return "iTerm2"
	}
	return "Terminal"
}

// Detect says which launcher the environment offers: tmux when TMUX is set,
// else, on macOS, the terminal named by TERM_PROGRAM.
func Detect(getenv func(string) string, goos string) (Launcher, bool) {
	if getenv("TMUX") != "" {
		return Tmux, true
	}
	if goos != "darwin" {
		return "", false
	}
	switch getenv("TERM_PROGRAM") {
	case "iTerm.app":
		return ITerm2, true
	case "Apple_Terminal":
		return Terminal, true
	}
	return "", false
}

// Runner runs a program to completion; a fake records its argv in tests.
type Runner interface {
	Run(ctx context.Context, name string, args ...string) error
}

// ExecRunner is the Runner that starts real processes.
type ExecRunner struct{}

// maxStderr bounds how much of a failed launcher's stderr reaches the error.
const maxStderr = 512

// Run runs name with args. When it fails, the error carries the program name and
// the start of its stderr (where an Automation denial or a dead tmux socket say why).
func (ExecRunner) Run(ctx context.Context, name string, args ...string) error {
	var stderr bytes.Buffer
	cmd := exec.CommandContext(ctx, name, args...) //nolint:gosec // the launcher programs are tmux and osascript with arguments this package built
	cmd.Stderr = &boundedWriter{buf: &stderr, room: maxStderr}
	err := cmd.Run()
	if err == nil {
		return nil
	}
	if reason := strings.TrimSpace(stderr.String()); reason != "" {
		return fmt.Errorf("%s: %w: %s", name, err, reason)
	}
	return fmt.Errorf("%s: %w", name, err)
}

// boundedWriter keeps the first room bytes and swallows the rest, so a chatty
// program is never blocked by a full buffer.
type boundedWriter struct {
	buf  *bytes.Buffer
	room int
}

func (w *boundedWriter) Write(p []byte) (int, error) {
	if n := min(len(p), w.room); n > 0 {
		w.buf.Write(p[:n])
		w.room -= n
	}
	return len(p), nil
}

// line is argv as one line for a POSIX shell, every element quoted. An element
// with a NUL byte cannot be quoted for a shell and is refused.
func line(argv []string) (string, error) {
	quoted := make([]string, len(argv))
	for i, arg := range argv {
		if strings.ContainsRune(arg, 0) {
			return "", domain.NewError(domain.CodeInvalidRequest, "the command to open in a terminal has a NUL byte", nil)
		}
		quoted[i] = "'" + strings.ReplaceAll(arg, "'", `'\''`) + "'"
	}
	return strings.Join(quoted, " "), nil
}

// Open opens a window of l that runs argv, titled title where the terminal has
// titles. The window is not waited for. When the launcher fails, nothing is left
// behind.
func (l Launcher) Open(ctx context.Context, run Runner, argv []string, title string) error {
	dir, script, err := writeScript(argv)
	if err != nil {
		return err
	}
	if err := run.Run(ctx, l.program(), l.args(script, title)...); err != nil {
		_ = os.RemoveAll(dir)
		return err
	}
	return nil
}

func (l Launcher) program() string {
	if l == Tmux {
		return "tmux"
	}
	return "osascript"
}

// args is the argv of the launcher program that opens a window running script.
func (l Launcher) args(script, title string) []string {
	if l == Tmux {
		// tmux expands formats (#{...}, #(...)) in a window name; "##" is a literal "#".
		return []string{"new-window", "-n", strings.ReplaceAll(title, "#", "##"), "/bin/sh", script}
	}
	// The command line travels as an argument of the AppleScript, never inside its source.
	cmdline, _ := line([]string{"/bin/sh", script}) // the script path has no NUL: it was just made
	open := `create window with default profile command (item 1 of argv)`
	if l == Terminal {
		open = `do script (item 1 of argv)`
	}
	return []string{
		"-e", "on run argv",
		"-e", `tell application "` + l.appName() + `"`,
		"-e", "activate",
		"-e", open,
		"-e", "end tell",
		"-e", "end run",
		"--", cmdline,
	}
}

// writeScript writes the script that removes itself and execs argv, in a new
// directory only this user can enter, and returns the directory and the script.
func writeScript(argv []string) (dir, script string, err error) {
	text, err := line(argv)
	if err != nil {
		return "", "", err
	}
	dir, err = os.MkdirTemp("", "cflk-attach-")
	if err != nil {
		return "", "", err
	}
	script = filepath.Join(dir, "attach")
	body := "#!/bin/sh\nrm -f -- \"$0\"\nrmdir -- \"${0%/*}\" 2>/dev/null\nexec " + text + "\n"
	if err := os.WriteFile(script, []byte(body), 0o700); err != nil { //nolint:gosec // it has to be executable by sh, and the directory is private
		_ = os.RemoveAll(dir)
		return "", "", err
	}
	return dir, script, nil
}
