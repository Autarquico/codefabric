package taskpull

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os/exec"
	"path"
	"strings"

	"github.com/Autarquico/codefabric/internal/support/redact"
)

// repo is the git repository the work is brought to. Nothing here resets, stashes or
// cleans: the commands only add refs, move a branch that is behind, and switch to a
// branch when the tree is clean.
type repo struct{ dir string }

// gitArgs turns off the hooks and the fsmonitor of the repository: what is checked out
// comes from a Run, and a tracked core.hooksPath or core.fsmonitor must not run code
// on this machine.
func gitArgs(dir string, args []string) []string {
	return append([]string{"-c", "core.hooksPath=/dev/null", "-c", "core.fsmonitor=", "-C", dir}, args...)
}

func (r repo) run(ctx context.Context, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "git", gitArgs(r.dir, args)...) //nolint:gosec // fixed git subcommands; user values follow "--" or are validated refs
	cmd.Env = append(cmd.Environ(), "GIT_TERMINAL_PROMPT=0", "LC_ALL=C")
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		var exit *exec.ExitError
		if msg := strings.TrimSpace(stderr.String()); msg != "" {
			err = fmt.Errorf("git %s: %s", args[0], redact.Text(msg))
		} else if errors.As(err, &exit) {
			err = fmt.Errorf("git %s: exit %d", args[0], exit.ExitCode())
		}
		return "", err
	}
	return strings.TrimSpace(stdout.String()), nil
}

// ok runs git for its exit status alone: true for 0, false for 1, an error otherwise.
func (r repo) ok(ctx context.Context, args ...string) (bool, error) {
	cmd := exec.CommandContext(ctx, "git", gitArgs(r.dir, args)...) //nolint:gosec // as run
	cmd.Env = append(cmd.Environ(), "GIT_TERMINAL_PROMPT=0", "LC_ALL=C")
	err := cmd.Run()
	var exit *exec.ExitError
	switch {
	case err == nil:
		return true, nil
	case errors.As(err, &exit) && exit.ExitCode() == 1:
		return false, nil
	}
	return false, fmt.Errorf("git %s: %w", args[0], err)
}

// toplevel is the root of the working tree that dir is in.
func (r repo) toplevel(ctx context.Context) (string, error) {
	out, err := r.run(ctx, "rev-parse", "--show-toplevel")
	if err != nil {
		return "", fmt.Errorf("this directory is not in a git repository: %w", err)
	}
	return out, nil
}

// branchOID is the commit a local branch points to.
func (r repo) branchOID(ctx context.Context, branch string) (string, bool) {
	out, err := r.run(ctx, "rev-parse", "--verify", "--quiet", "refs/heads/"+branch+"^{commit}")
	return out, err == nil && out != ""
}

func (r repo) isAncestor(ctx context.Context, ancestor, of string) (bool, error) {
	return r.ok(ctx, "merge-base", "--is-ancestor", ancestor, of)
}

func (r repo) currentBranch(ctx context.Context) string {
	out, err := r.run(ctx, "symbolic-ref", "--quiet", "--short", "HEAD")
	if err != nil {
		return ""
	}
	return out
}

// dirty says whether the working tree has changes git can see: tracked or untracked.
func (r repo) dirty(ctx context.Context) (bool, error) {
	out, err := r.run(ctx, "status", "--porcelain", "--untracked-files=normal")
	return out != "", err
}

// checkedOut lists the branches that a working tree of the repository has checked out.
func (r repo) checkedOut(ctx context.Context) (map[string]bool, error) {
	out, err := r.run(ctx, "worktree", "list", "--porcelain")
	if err != nil {
		return nil, err
	}
	branches := map[string]bool{}
	for _, line := range strings.Split(out, "\n") {
		if b, ok := strings.CutPrefix(line, "branch refs/heads/"); ok {
			branches[b] = true
		}
	}
	return branches, nil
}

func (r repo) validBranch(ctx context.Context, name string) bool {
	ok, err := r.ok(ctx, "check-ref-format", "--branch", name)
	return err == nil && ok
}

// subject is the first line of the message of a commit.
func (r repo) subject(ctx context.Context, commit string) string {
	out, _ := r.run(ctx, "log", "-1", "--format=%s", commit)
	return out
}

// ignoredClash lists the ignored files of the working tree that a switch to a branch at
// commit would overwrite without a word: those commit versions as a file, those under a
// path commit versions as a file, and those that are a directory path of commit.
func (r repo) ignoredClash(ctx context.Context, commit string) ([]string, error) {
	ignored, err := r.run(ctx, "ls-files", "-z", "--others", "--ignored", "--exclude-standard")
	if err != nil || ignored == "" {
		return nil, err
	}
	versioned, err := r.run(ctx, "ls-tree", "-r", "-z", "--name-only", commit)
	if err != nil {
		return nil, err
	}
	files, dirs := map[string]bool{}, map[string]bool{}
	for _, f := range strings.Split(versioned, "\x00") {
		files[f] = true
		for d := path.Dir(f); d != "."; d = path.Dir(d) {
			dirs[d] = true
		}
	}
	var clash []string
	for _, f := range strings.Split(ignored, "\x00") {
		if f != "" && (files[f] || dirs[f] || underFile(f, files)) {
			clash = append(clash, f)
		}
	}
	return clash, nil
}

// underFile says whether an ancestor directory of f is a file in files.
func underFile(f string, files map[string]bool) bool {
	for d := path.Dir(f); d != "."; d = path.Dir(d) {
		if files[d] {
			return true
		}
	}
	return false
}
