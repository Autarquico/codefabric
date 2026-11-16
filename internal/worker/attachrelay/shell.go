package attachrelay

import (
	"bufio"
	"context"
	"os"
	"os/user"
	"path/filepath"
	"strconv"
	"strings"

	cfruntime "github.com/Autarquico/codefabric/internal/adapters/runtime"
	v1 "github.com/Autarquico/codefabric/internal/api/v1"
	"github.com/Autarquico/codefabric/internal/domain"
)

// Defaults of the shell.
const (
	// FallbackShell is what runs when the account has no usable login shell.
	FallbackShell = "/bin/sh"
	// shellPath is the PATH of a shell before its own profile sets one: a fixed
	// list, not the one of this process.
	shellPath = "/usr/local/bin:/usr/bin:/bin"
	// shellLang is the LANG of a shell when this machine has none to give.
	shellLang = "C.UTF-8"
)

// Account is the OS user the Worker runs as, whose login shell a shell is.
type Account struct {
	Name, Home, Shell string
}

// ShellOptions is what the relay needs to open a shell. A shell runs only when
// both AllowShell and Worktree are set.
type ShellOptions struct {
	// Allow says whether this Worker's own settings (allow_shell) allow a shell
	// right now. It is read again at each request, and again right before the
	// shell starts, so a Worker that withdraws it stops the ones not yet started.
	Allow func() bool
	// Worktree is the worktree of the Run on this Worker, built from the Worker's
	// own state and never from what a request says.
	Worktree func(runID string, leaseEpoch uint64) string
	// Account is the OS user to run as; defaults to the user of this process.
	Account func() (Account, error)
	// Lang is the LANG a shell gets; defaults to the one of this process, or C.UTF-8.
	Lang func() string
}

// prepareShell checks a request for a shell against this machine and builds the
// command: the login shell of the Worker's user, in the worktree of the Run, with
// an environment of its own. Nothing of this process's environment is passed on.
func (r *Relay) prepareShell(ctx context.Context, req v1.AttachRequest) (cfruntime.Command, error) {
	if err := r.recheck(req); err != nil {
		return cfruntime.Command{}, err
	}
	if r.o.Shell.Worktree == nil {
		return cfruntime.Command{}, domain.NewError(domain.CodeNotImplemented, "this Worker does not open a shell", nil)
	}
	notRunning := func(msg string) error {
		return domain.NewError(domain.CodeRunNotAttachable, msg, map[string]any{"reason": v1.NotAttachableNotRunning})
	}
	if r.o.Live == nil {
		return cfruntime.Command{}, notRunning("this Worker cannot tell that the Run is on it")
	}
	live, err := r.o.Live(ctx, req.RunID, req.LeaseEpoch)
	switch {
	case err != nil:
		return cfruntime.Command{}, err
	case !live:
		return cfruntime.Command{}, notRunning("this Run is not running on this Worker")
	}
	dir := r.o.Shell.Worktree(req.RunID, req.LeaseEpoch)
	if st, err := os.Stat(dir); dir == "" || err != nil || !st.IsDir() {
		return cfruntime.Command{}, notRunning("the worktree of this Run is not on this Worker")
	}
	account := currentAccount
	if r.o.Shell.Account != nil {
		account = r.o.Shell.Account
	}
	acct, err := account()
	if err != nil {
		return cfruntime.Command{}, err
	}
	lang := os.Getenv("LANG")
	if r.o.Shell.Lang != nil {
		lang = r.o.Shell.Lang()
	}
	shell := usableShell(acct.Shell)
	return cfruntime.Command{
		Path: shell, Args: []string{"-l"}, Env: ShellEnv(acct, shell, lang), Dir: dir, KillGroup: true,
	}, nil
}

// recheck is the answer to "does this Worker allow this request": for a shell,
// only if its own setting is on now. Other modes have nothing to check.
func (r *Relay) recheck(req v1.AttachRequest) error {
	if req.Mode != v1.AttachModeShell || (r.o.Shell.Allow != nil && r.o.Shell.Allow()) {
		return nil
	}
	return domain.NewError(domain.CodePolicyDenied,
		"policy "+domain.PolicyWorkerAllowShell+": this Worker does not allow a shell (allow_shell is off)",
		map[string]any{"policy": domain.PolicyWorkerAllowShell})
}

// ShellEnv is the whole environment of a shell: what a login needs and nothing
// else. It is built from the account and fixed values, never from the
// environment of this process, which may hold tokens and secrets of the Worker.
func ShellEnv(a Account, shell, lang string) []string {
	if lang == "" {
		lang = shellLang
	}
	return []string{
		"HOME=" + a.Home,
		"USER=" + a.Name,
		"LOGNAME=" + a.Name,
		"SHELL=" + shell,
		"PATH=" + shellPath,
		"LANG=" + lang,
		// TERM is set by the pty.
	}
}

// usableShell is shell if it is an absolute path to an executable file, and
// FallbackShell otherwise.
func usableShell(shell string) string {
	if st, err := os.Stat(shell); err == nil && filepath.IsAbs(shell) && st.Mode().IsRegular() && st.Mode().Perm()&0o111 != 0 {
		return shell
	}
	return FallbackShell
}

// currentAccount is the user of this process, with the shell of the passwd file.
func currentAccount() (Account, error) {
	u, err := user.Current()
	if err != nil {
		return Account{}, domain.NewError(domain.CodeInternal, "this Worker could not tell which user it runs as", nil)
	}
	uid, _ := strconv.Atoi(u.Uid)
	return Account{Name: u.Username, Home: u.HomeDir, Shell: PasswdShell("/etc/passwd", uid)}, nil
}

// PasswdShell is the login shell the passwd file at path gives the user uid, or
// "" if there is none: no such user or file, or the field is empty.
func PasswdShell(path string, uid int) string {
	f, err := os.Open(path) //nolint:gosec // the passwd file, or a test's
	if err != nil {
		return ""
	}
	defer func() { _ = f.Close() }()
	want := strconv.Itoa(uid)
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		// name:password:uid:gid:gecos:home:shell
		fields := strings.Split(sc.Text(), ":")
		if len(fields) == 7 && fields[2] == want {
			return fields[6]
		}
	}
	return ""
}
