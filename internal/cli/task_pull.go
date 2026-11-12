package cli

import (
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/spf13/cobra"

	"github.com/Autarquico/codefabric/internal/client/taskpull"
	"github.com/Autarquico/codefabric/internal/config"
	"github.com/Autarquico/codefabric/internal/domain"
)

// execResume replaces this process with the command that resumes the session, in dir. It
// is a variable so that the tests do not exec.
var execResume = func(dir string, argv []string) error {
	path, err := exec.LookPath(argv[0])
	if err != nil {
		return fmt.Errorf("%s is not installed here: run %s yourself where it is", argv[0], strings.Join(argv, " "))
	}
	if err := os.Chdir(dir); err != nil {
		return err
	}
	return syscall.Exec(path, argv, os.Environ()) //nolint:gosec // the command is built by the harness table of the session package
}

// newTaskPullCmd is `cf task pull <task> [--worktree p] [--no-session] [--defer] [--open]`
// (§11.7): it brings the work of the Task, and the session of its harness, here.
func newTaskPullCmd(sessions SessionsFactory) *cobra.Command {
	var (
		worktree           string
		noSession, deferIt bool
		open               bool
	)
	cmd := &cobra.Command{
		Use:   "pull <task>",
		Short: "Bring a Task's work and session to this checkout",
		Long: "Fetches the last checkpoint of the Task from the relay into this repository and puts it on a branch: the\n" +
			"branch the Task started from if the checkpoint contains it (it advances), otherwise cf/<task>, and\n" +
			"cf/<task>@<run> if cf/<task> has commits of its own. Nothing is reset, stashed or cleaned: with\n" +
			"uncommitted changes here the branch is made but not checked out (--worktree makes a new worktree for it).\n" +
			"What the Worker had not committed arrives as the sealing commit of the checkpoint. Unless --no-session,\n" +
			"the session of the harness is written to its home for this directory and the exact command to resume it is\n" +
			"printed; --open runs it. A Run that is live is refused unless --defer, which suspends it first.\n" +
			"After a pull, resuming the Task needs a session of this machine (--session) or --force.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := openClient(cmd)
			if err != nil {
				return err
			}
			dir, err := os.Getwd()
			if err != nil {
				return err
			}
			opts := taskpull.Options{Dir: dir, Project: refScope(cmd, c), Worktree: worktree, NoSession: noSession, Defer: deferIt}
			if sessions != nil && !noSession {
				paths, err := config.ResolveFromEnv()
				if err != nil {
					return err
				}
				opts.Sessions = sessions(filepath.Join(paths.ConfigDir, "sessions"))
			}
			res, err := taskpull.Pull(cmd.Context(), c, args[0], opts)
			if err != nil {
				return asUserError(err)
			}
			if err := emit(cmd, res, func(w io.Writer) error { return renderPull(w, res) }); err != nil {
				return err
			}
			if !open {
				return nil
			}
			if res.Session == nil || len(res.Session.Resume) == 0 {
				return domain.NewError(domain.CodeInvalidRequest, "--open has no session to open: the pull brought none", nil)
			}
			return execResume(res.Dir, res.Session.Resume)
		},
	}
	fl := cmd.Flags()
	fl.StringVar(&worktree, "worktree", "", "make a new git worktree at this path for the work, and install the session for it")
	fl.BoolVar(&noSession, "no-session", false, "bring the branch only: leave the sessions of the harness as they are")
	fl.BoolVar(&deferIt, "defer", false, "if the Task has a live Run, defer it first and pull the checkpoint it publishes")
	fl.BoolVar(&open, "open", false, "run the command that resumes the session, here, in the foreground")
	addProjectFlag(cmd, "Project that narrows a name more than one Project has (default: the one of this directory)")
	return cmd
}

func renderPull(w io.Writer, r taskpull.Result) error {
	p := func(format string, a ...any) { _, _ = fmt.Fprintf(w, format+"\n", a...) }
	short := r.Commit
	if len(short) > 12 {
		short = short[:12]
	}
	if r.Deferred {
		p("%s: the Run was deferred first.", r.Task)
	}
	switch {
	case r.Worktree:
		p("%s: branch %s (%s, %s) is checked out in the new worktree %s.", r.Task, r.Branch, r.Outcome, short, r.Dir)
	case r.CheckedOut:
		p("%s: branch %s (%s, %s) is checked out in %s.", r.Task, r.Branch, r.Outcome, short, r.Dir)
	default:
		p("%s: branch %s (%s, %s) is ready but NOT checked out: this tree has uncommitted changes and none was touched.", r.Task, r.Branch, r.Outcome, short)
		p("  To work on it without disturbing them: cf task pull %s --worktree <path>", r.Task)
	}
	if r.Sealed {
		p("  The last commit holds what the Worker had not committed (\"cf: uncommitted changes at exit\").")
	}
	if len(r.Excluded) > 0 {
		p("  Left out of the checkpoint (not copied; yours here are untouched): %s", strings.Join(r.Excluded, ", "))
	}
	for _, warn := range r.Warnings {
		p("  warning: %s", warn)
	}
	if s := r.Session; s != nil {
		switch {
		case s.Backup != "":
			p("  Session %s updated; the previous copy is %s.", s.ID, s.Backup)
		case s.NewID:
			p("  Session installed under a new id: %s.", s.ID)
		default:
			p("  Session %s installed.", s.ID)
		}
		p("  Resume it: %s", strings.Join(s.Resume, " "))
	}
	return nil
}
