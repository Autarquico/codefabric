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
	"net"
	"net/url"
	"os/exec"
	"path"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	v1 "github.com/Autarquico/codefabric/internal/api/v1"
	"github.com/Autarquico/codefabric/internal/domain"
)

// ErrNoOrigin means the directory has no "origin" remote to take a repository from.
var ErrNoOrigin = errors.New("gitctx: no origin remote")

// OriginURL is the URL of the "origin" remote of the repository at dir. A
// local path or file:// origin that is relative is made absolute against the
// directory of the repository, which is what git resolves it against, so that
// it can be compared with other paths.
func OriginURL(ctx context.Context, dir string) (string, error) {
	out, err := git(ctx, dir, "config", "--get", "remote.origin.url")
	if err != nil || out == "" {
		return "", ErrNoOrigin
	}
	top, err := git(ctx, dir, "rev-parse", "--show-toplevel")
	if err != nil {
		top = dir
	}
	return absoluteOrigin(out, top), nil
}

func absoluteOrigin(origin, base string) string {
	local := origin
	switch {
	case strings.HasPrefix(origin, "file://"):
		local = strings.TrimPrefix(origin, "file://")
	case strings.Contains(origin, "://"), scpLike.MatchString(origin):
		return origin
	}
	if filepath.IsAbs(local) {
		return origin
	}
	return filepath.Join(base, local)
}

var scpLike = regexp.MustCompile(`^(?:[^@/:]+@)?([^/:]+):(.*)$`)

// Normalize reduces the many spellings of a repository URL to one key, so that
// the remote of a working copy can be matched with the repo of a Project:
//
//	git@GitHub.com:org/repo.git, ssh://git@github.com:22/org/repo, https://user:pw@github.com/org/repo/
//
// all become github.com/org/repo. The scheme, user, a trailing ".git" or slash,
// the case of the host and the default port of the scheme (ssh 22, https 443,
// http 80, git 9418) do not tell repositories apart; the case of the path and
// any other port do. Local paths and file:// URLs become file:<path>, cleaned
// but not resolved: a relative path stays relative, and OriginURL already
// makes the origin of a working copy absolute. They keep their ".git": on a
// filesystem /srv/repo and /srv/repo.git are two repositories.
func Normalize(raw string) (string, error) {
	s := strings.TrimSpace(raw)
	var host, repoPath string
	var absolute bool
	switch {
	case s == "":
		return "", errors.New("gitctx: empty repository URL")
	case strings.Contains(s, "://"):
		u, err := url.Parse(s)
		if err != nil {
			// url.Error repeats the raw URL, credentials included: never wrap it.
			return "", fmt.Errorf("gitctx: repository URL %q is not a valid URL", redact(s))
		}
		if u.Scheme == "file" {
			return localKey(u.Path), nil
		}
		host, repoPath = strings.ToLower(u.Hostname()), u.Path
		if port := u.Port(); port != "" && port != defaultPorts[strings.ToLower(u.Scheme)] {
			host = net.JoinHostPort(host, port)
		}
		scheme := strings.ToLower(u.Scheme)
		absolute = scheme == "ssh" || scheme == "git+ssh" || scheme == "ssh+git"
	case scpLike.MatchString(s):
		m := scpLike.FindStringSubmatch(s)
		host, repoPath = strings.ToLower(m[1]), m[2]
		absolute = strings.HasPrefix(repoPath, "/")
	default:
		return localKey(s), nil
	}
	repoPath = strings.TrimSuffix(strings.Trim(path.Clean("/"+repoPath), "/"), ".git")
	if host == "" || repoPath == "" {
		return "", fmt.Errorf("gitctx: repository URL %q has no host or path", redact(s))
	}
	if absolute {
		// Over ssh a path is a filesystem path: host:/srv/x and host:srv/x
		// (relative to the home of the user) are two repositories, so the
		// absolute form gets its own key. Forges treat both alike, but a
		// failed match is an error; a wrong one would pick another Project.
		return host + "//" + repoPath, nil
	}
	return host + "/" + repoPath, nil
}

// defaultPorts are the ports a scheme implies, which a URL may spell or omit.
var defaultPorts = map[string]string{"ssh": "22", "https": "443", "http": "80", "git": "9418"}

func localKey(p string) string { return "file:" + filepath.Clean(p) }

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

var (
	userinfo = regexp.MustCompile(`(\w+://)[^/@\s]+@`)
	// authority splits scheme://[userinfo@]rest; the userinfo ends at the last '@' of the authority.
	authority = regexp.MustCompile(`^([A-Za-z][A-Za-z0-9+.-]*://)([^/?#]*)@(.*)$`)
)

// StripCredentials takes the secrets out of a repository URL before it is
// stored or sent: Projects keep clean URLs, and git finds the credentials in
// the user's own configuration. It reports whether it removed anything that may
// have been a secret. A password goes from every URL, keeping the user name; an
// http(s) URL loses its whole userinfo, since there the name may be a token.
// The user of an ssh URL, like git@, is no secret and stays.
func StripCredentials(raw string) (clean string, removed bool) {
	m := authority.FindStringSubmatch(raw)
	if m == nil {
		return raw, false
	}
	scheme, info, rest := m[1], m[2], m[3]
	name, _, hasPassword := strings.Cut(info, ":")
	switch {
	case strings.EqualFold(scheme, "http://") || strings.EqualFold(scheme, "https://"):
		return scheme + rest, true
	case hasPassword && name == "":
		return scheme + rest, true
	case hasPassword:
		return scheme + name + "@" + rest, true
	}
	return raw, false
}

// redact hides the credentials a URL may carry before it reaches a message.
func redact(s string) string { return userinfo.ReplaceAllString(s, "${1}***@") }
