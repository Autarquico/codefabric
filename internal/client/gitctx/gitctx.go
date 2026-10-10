// Package gitctx reads the git context of the client's working directory: the
// repository it belongs to, and the commit a ref points to on the remote. It
// runs on the client because the Controller never holds credentials to the
// origin (ADR 0002 D1); git is run with the user's own.
package gitctx

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os/exec"
	"regexp"
	"slices"
	"strings"
	"time"

	v1 "github.com/Autarquico/codefabric/internal/api/v1"
	"github.com/Autarquico/codefabric/internal/domain"
)

// ErrNoOrigin means the directory has no "origin" remote to take a repository from.
var ErrNoOrigin = errors.New("gitctx: no origin remote")

// OriginURL is the URL of the "origin" remote of the repository at dir, as git
// has it but for the surrounding white space.
func OriginURL(ctx context.Context, dir string) (string, error) {
	out, err := git(ctx, dir, "config", "--get", "remote.origin.url")
	if err != nil || out == "" {
		return "", ErrNoOrigin
	}
	return strings.TrimSpace(out), nil
}

// ErrRelativeOrigin means the origin is a path relative to the working copy.
// What it names depends on where git is run, so it identifies no repository.
var ErrRelativeOrigin = errors.New("gitctx: the origin is a relative path: use --project or set an absolute origin")

// scpLike is the "[user@]host:path" form of an ssh remote.
var scpLike = regexp.MustCompile(`^(?:[^@/:]+@)?[^/:]+:`)

// Match returns the Projects that have origin among their URLs: the main repo
// or an alias. The comparison is literal, byte for byte: no case folding, no
// ".git", port or slash handling, no path cleaning. Two spellings of one
// repository are told apart by adding the second as an alias of the Project. An
// origin that is a relative path matches nothing (ErrRelativeOrigin): git
// resolves it against the working copy, and guessing a path would trade
// repositories.
func Match(origin string, projects []v1.Project) ([]v1.Project, error) {
	origin = strings.TrimSpace(origin)
	if origin == "" {
		return nil, errors.New("gitctx: empty repository URL")
	}
	if isRelativePath(origin) {
		return nil, ErrRelativeOrigin
	}
	var out []v1.Project
	for _, p := range projects {
		if slices.Contains(p.RepoURLs(), origin) {
			out = append(out, p)
		}
	}
	return out, nil
}

func isRelativePath(s string) bool {
	switch {
	case strings.HasPrefix(s, "file://"):
		return !strings.HasPrefix(s, "file:///")
	case strings.Contains(s, "://"), scpLike.MatchString(s):
		return false
	}
	return !strings.HasPrefix(s, "/")
}

var fullSHA = regexp.MustCompile(`^(?:[0-9a-f]{40}|[0-9a-f]{64})$`)

// ResolveRef asks the remote which commit ref points to, with git ls-remote.
// remote is a remote name of the repository at dir ("origin") or a URL. A full
// commit ID is returned as it is. A branch wins over a tag of the same name,
// and an annotated tag gives the commit it points to.
func ResolveRef(ctx context.Context, dir, remote, ref string) (string, error) {
	return resolveRef(ctx, dir, remote, ref, lsRemoteTimeout)
}

// lsRemoteTimeout bounds one git ls-remote: a remote that accepts the
// connection and never answers must not hang the command. It is an
// infrastructure deadline on a context, so it runs on real time.
const lsRemoteTimeout = 30 * time.Second

func resolveRef(ctx context.Context, dir, remote, ref string, timeout time.Duration) (string, error) {
	if fullSHA.MatchString(ref) {
		return ref, nil
	}
	if ref == "" || strings.HasPrefix(ref, "-") || strings.HasPrefix(remote, "-") {
		return "", domain.NewError(domain.CodeInvalidRequest, "the ref and the remote must not start with '-'", nil)
	}
	// The second pattern asks for the peeled line of an annotated tag, which the first does not match.
	lsCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	out, err := git(lsCtx, dir, "ls-remote", "--", remote, ref, ref+"^{}")
	if err != nil {
		if ctx.Err() == nil && errors.Is(lsCtx.Err(), context.DeadlineExceeded) {
			return "", fmt.Errorf("resolving %q on %s: no answer within %s", ref, redact(remote), timeout)
		}
		return "", fmt.Errorf("resolving %q on %s: %w", ref, redact(remote), err)
	}
	found := map[string]string{}
	for _, line := range strings.Split(out, "\n") {
		if sha, name, ok := strings.Cut(line, "\t"); ok {
			found[name] = sha
		}
	}
	for _, name := range candidates(ref) {
		if sha, ok := found[name]; ok && fullSHA.MatchString(sha) {
			return sha, nil
		}
	}
	return "", domain.NewError(domain.CodeNotFound,
		fmt.Sprintf("ref %q was not found on %s", ref, redact(remote)), map[string]any{"ref": ref})
}

// candidates are the ref names that ref may stand for, best first.
func candidates(ref string) []string {
	if ref == "HEAD" || strings.HasPrefix(ref, "refs/") {
		return []string{ref + "^{}", ref}
	}
	return []string{"refs/heads/" + ref, "refs/tags/" + ref + "^{}", "refs/tags/" + ref}
}

// git runs one git command in dir and returns its trimmed stdout. Its failure
// carries git's own message, which is local to the user's machine.
func git(ctx context.Context, dir string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "git", append([]string{"-C", dir}, args...)...) //nolint:gosec // fixed git subcommands; user values follow "--"
	cmd.Env = append(cmd.Environ(), "GIT_TERMINAL_PROMPT=0", "LC_ALL=C")
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		if msg := strings.TrimSpace(stderr.String()); msg != "" {
			return "", fmt.Errorf("git %s: %s", args[0], redact(msg))
		}
		return "", fmt.Errorf("git %s: %w", args[0], err)
	}
	return strings.TrimSpace(stdout.String()), nil
}

// urlToken is a URL with a scheme inside a longer text, such as git's stderr.
var urlToken = regexp.MustCompile(`[A-Za-z][A-Za-z0-9+.-]*://[^\s'"<>]*`)

// redact hides what a URL in s may carry as a secret: the whole userinfo, up to
// the last '@', and any query or fragment (https://h/r?token=x becomes
// https://h/r?***). It errs on the side of hiding too much.
func redact(s string) string {
	s = urlToken.ReplaceAllStringFunc(s, func(u string) string {
		scheme, rest, _ := strings.Cut(u, "://")
		// The query and fragment go first: an '@' inside them must not be
		// taken for the end of a userinfo.
		suffix := ""
		if i := strings.IndexAny(rest, "?#"); i >= 0 {
			rest, suffix = rest[:i], rest[i:i+1]+"***"
		}
		authority, path, hasPath := strings.Cut(rest, "/")
		if i := strings.LastIndex(authority, "@"); i >= 0 {
			authority = "***@" + authority[i+1:]
		}
		if hasPath {
			authority += "/" + path
		}
		return scheme + "://" + authority + suffix
	})
	// scp-like user:password@host:path has no scheme to anchor on: split each
	// word the way a remote is split and hide a userinfo that holds a password.
	return textWord.ReplaceAllStringFunc(s, func(w string) string {
		if strings.Contains(w, "://") {
			return w
		}
		user, host, path, ok := domain.SplitScp(w)
		if !ok || !strings.Contains(user, ":") {
			return w
		}
		return "***@" + host + ":" + path
	})
}

// textWord is a run of characters that may form a remote inside a text.
var textWord = regexp.MustCompile(`[^\s'"<>]+`)
