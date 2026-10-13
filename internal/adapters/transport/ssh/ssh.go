// Package ssh is the Transport (§5.4) `cf attach` uses in v0.x: it opens a
// terminal on another machine with the OpenSSH client and runs one command in it.
//
// What it is given comes, in the end, from a Worker, and becomes a local process
// and a command line on a remote shell. So the destination is a typed
// domain.SSHTarget, checked again here; ssh gets it after `--`, which closes
// option injection (-oProxyCommand=... would run code on the client's machine);
// and the remote command has no "argv without a shell" to hope for, since OpenSSH
// joins the arguments and the remote shell reads them: every element is limited
// to a closed alphabet and quoted besides.
package ssh

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"regexp"
	"strconv"
	"strings"

	"github.com/Autarquico/codefabric/internal/domain"
)

const connectTimeoutSeconds = 15

// ErrRejected is returned for a destination or a command that this package
// refuses to hand to ssh. Nothing was run.
var ErrRejected = errors.New("ssh: refused to run")

// Options parameterise Attach. Binary is the OpenSSH client; it defaults to
// "ssh" on the PATH.
type Options struct {
	Binary string
}

// Stdio is the terminal of the person. Files are handed to ssh as they are, so
// that it can use their tty.
type Stdio struct {
	In  io.Reader
	Out io.Writer
	Err io.Writer
}

// remoteWord is what an element of the remote command may be made of.
var remoteWord = regexp.MustCompile(`^[A-Za-z0-9_./=:+-]+$`)

// Command is the remote command line for the argv: each element in single
// quotes, which a POSIX shell (sh, bash, zsh) reads back as the same words.
func Command(argv []string) (string, error) {
	if len(argv) == 0 {
		return "", fmt.Errorf("%w: an empty command", ErrRejected)
	}
	quoted := make([]string, len(argv))
	for i, a := range argv {
		if !remoteWord.MatchString(a) {
			return "", fmt.Errorf("%w: an element of the command has characters outside the allowed set", ErrRejected)
		}
		quoted[i] = "'" + a + "'"
	}
	return strings.Join(quoted, " "), nil
}

// Argv is the argument list of ssh for the target and the remote argv: a
// terminal (-t), a bounded connection time, the port if there is one, then `--`,
// the destination and the one remote command. The target is parsed again from
// its text, so one that was built by hand and is not a valid target is refused.
func Argv(target domain.SSHTarget, remote []string) ([]string, error) {
	again, err := domain.ParseSSHTarget(target.String())
	if err != nil || again != target {
		return nil, fmt.Errorf("%w: the destination is not a valid ssh target", ErrRejected)
	}
	command, err := Command(remote)
	if err != nil {
		return nil, err
	}
	args := []string{"-t", "-o", "ConnectTimeout=" + strconv.Itoa(connectTimeoutSeconds)}
	if target.Port != 0 {
		args = append(args, "-p", strconv.Itoa(target.Port))
	}
	return append(args, "--", target.Destination(), command), nil
}

// Attach runs the remote argv on the target in a terminal and returns the exit
// status of ssh: 0 when the person detached, 255 when ssh itself failed (it has
// said why on stderr), and whatever the remote command exited with otherwise. An
// error is only a failure to start ssh, or a refusal (ErrRejected).
//
// It does not put ssh in a process group of its own, as the Worker's supervisor
// does with what it starts: ssh has to be in the foreground group of the
// terminal. And it does not end ssh when ctx ends once it runs: the terminal's
// own interrupt reaches the group, the client's context is cancelled by that
// same interrupt, and the session must outlive the person's Ctrl-C of the local
// process; closing the window or detaching is what ends it.
func Attach(ctx context.Context, o Options, target domain.SSHTarget, remote []string, io Stdio) (int, error) {
	args, err := Argv(target, remote)
	if err != nil {
		return 0, err
	}
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	bin := o.Binary
	if bin == "" {
		bin = "ssh"
	}
	cmd := exec.Command(bin, args...) //nolint:gosec,noctx // the arguments are what Argv checked; ctx must not end an attached terminal
	cmd.Stdin, cmd.Stdout, cmd.Stderr = io.In, io.Out, io.Err
	err = cmd.Run()
	var exit *exec.ExitError
	switch {
	case err == nil:
		return 0, nil
	case errors.As(err, &exit):
		if code := exit.ExitCode(); code >= 0 {
			return code, nil
		}
		return 255, nil // ended by a signal
	}
	return 0, fmt.Errorf("ssh: starting %s: %w", bin, err)
}
