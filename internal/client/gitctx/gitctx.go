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

// ErrUnsupportedOrigin means the origin is not one of the forms a Project can
// keep (domain.ValidRepoURL), a relative path among them: it identifies no
// repository, and it is never shown.
var ErrUnsupportedOrigin = errors.New("gitctx: the origin is not a supported repository form: use --project")

// Match returns the Projects that have origin among their URLs: the main repo
// or an alias. The comparison is literal, byte for byte: no case folding, no
// ".git", port or slash handling, no path cleaning. Two spellings of one
// repository are told apart by adding the second as an alias of the Project. An
// origin that is not a supported form matches nothing (ErrUnsupportedOrigin).
func Match(origin string, projects []v1.Project) ([]v1.Project, error) {
	origin = strings.TrimSpace(origin)
	if origin == "" {
		return nil, errors.New("gitctx: empty repository URL")
	}
	if !domain.ValidRepoURL(origin) {
		return nil, ErrUnsupportedOrigin
	}
	var out []v1.Project
	for _, p := range projects {
		if slices.Contains(p.RepoURLs(), origin) {
			out = append(out, p)
		}
	}
	return out, nil
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

// redact is a blunt defence in depth: git only sees URLs that passed
// domain.ValidRepoURL, or an origin that equals one. Every white-space
// separated word that has an '@', or a "://" with a '?' or '#', is replaced
// whole; the rest of s is untouched.
func redact(s string) string {
	return word.ReplaceAllStringFunc(s, func(w string) string {
		if strings.Contains(w, "@") || (strings.Contains(w, "://") && strings.ContainsAny(w, "?#")) {
			return "<redacted>"
		}
		return w
	})
}

var word = regexp.MustCompile(`\S+`)
