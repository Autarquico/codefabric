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
	"net/url"
	"os/exec"
	"path"
	"path/filepath"
	"regexp"
	"strings"

	v1 "github.com/Autarquico/codefabric/internal/api/v1"
	"github.com/Autarquico/codefabric/internal/domain"
)

// ErrNoOrigin means the directory has no "origin" remote to take a repository from.
var ErrNoOrigin = errors.New("gitctx: no origin remote")

// OriginURL is the URL of the "origin" remote of the repository at dir.
func OriginURL(ctx context.Context, dir string) (string, error) {
	out, err := git(ctx, dir, "config", "--get", "remote.origin.url")
	if err != nil || out == "" {
		return "", ErrNoOrigin
	}
	return out, nil
}

var scpLike = regexp.MustCompile(`^(?:[^@/:]+@)?([^/:]+):(.*)$`)

// Normalize reduces the many spellings of a repository URL to one key, so that
// the remote of a working copy can be matched with the repo of a Project:
//
//	git@GitHub.com:org/repo.git, ssh://git@github.com:22/org/repo, https://user:pw@github.com/org/repo/
//
// all become github.com/org/repo. The scheme, user, port, a trailing ".git" or
// slash and the case of the host do not tell repositories apart; the case of
// the path does. Local paths and file:// URLs become file:<absolute path>.
func Normalize(raw string) (string, error) {
	s := strings.TrimSpace(raw)
	var host, repoPath string
	switch {
	case s == "":
		return "", errors.New("gitctx: empty repository URL")
	case strings.Contains(s, "://"):
		u, err := url.Parse(s)
		if err != nil {
			return "", fmt.Errorf("gitctx: repository URL %q: %w", redact(s), err)
		}
		if u.Scheme == "file" {
			return localKey(u.Path), nil
		}
		host, repoPath = strings.ToLower(u.Hostname()), u.Path
	case scpLike.MatchString(s):
		m := scpLike.FindStringSubmatch(s)
		host, repoPath = strings.ToLower(m[1]), m[2]
	default:
		return localKey(s), nil
	}
	repoPath = strings.TrimSuffix(strings.Trim(path.Clean("/"+repoPath), "/"), ".git")
	if host == "" || repoPath == "" {
		return "", fmt.Errorf("gitctx: repository URL %q has no host or path", redact(s))
	}
	return host + "/" + repoPath, nil
}

func localKey(p string) string {
	return "file:" + strings.TrimSuffix(filepath.Clean(p), ".git")
}

// Match returns the Projects whose repository is the one at origin. A Project
// whose repo cannot be read as a URL matches nothing.
func Match(origin string, projects []v1.Project) ([]v1.Project, error) {
	want, err := Normalize(origin)
	if err != nil {
		return nil, err
	}
	var out []v1.Project
	for _, p := range projects {
		if got, err := Normalize(p.Repo); err == nil && got == want {
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
	if fullSHA.MatchString(ref) {
		return ref, nil
	}
	if ref == "" || strings.HasPrefix(ref, "-") || strings.HasPrefix(remote, "-") {
		return "", domain.NewError(domain.CodeInvalidRequest, "the ref and the remote must not start with '-'", nil)
	}
	// The second pattern asks for the peeled line of an annotated tag, which the first does not match.
	out, err := git(ctx, dir, "ls-remote", "--", remote, ref, ref+"^{}")
	if err != nil {
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

var userinfo = regexp.MustCompile(`(\w+://)[^/@\s]+@`)

// redact hides the credentials a URL may carry before it reaches a message.
func redact(s string) string { return userinfo.ReplaceAllString(s, "${1}***@") }
