package gitctx_test

import (
	"errors"
	"os/exec"
	"strings"
	"testing"

	v1 "github.com/Autarquico/codefabric/internal/api/v1"
	"github.com/Autarquico/codefabric/internal/client/gitctx"
	"github.com/Autarquico/codefabric/internal/domain"
)

func TestNormalize(t *testing.T) {
	same := []struct {
		name string
		urls []string
		want string
	}{
		{"one repository, many spellings", []string{
			"git@github.com:org/repo.git",
			"git@GitHub.com:org/repo",
			"github.com:org/repo.git",
			"ssh://git@github.com/org/repo.git",
			"ssh://git@github.com:2222/org/repo/",
			"https://github.com/org/repo.git",
			"https://user:secret@GITHUB.COM/org/repo/",
			"http://github.com:8080/org/repo",
			"git://github.com/org/repo.git",
			"  https://github.com/org/repo.git\n",
		}, "github.com/org/repo"},
		{"an absolute scp path", []string{"git@host.example:/srv/git/repo.git", "ssh://host.example//srv/git/repo.git"}, "host.example/srv/git/repo"},
		{"a local repository", []string{"/srv/git/repo.git", "/srv/git/repo", "file:///srv/git/repo.git", "/srv/git//repo/"}, "file:/srv/git/repo"},
	}
	for _, c := range same {
		for _, raw := range c.urls {
			if got, err := gitctx.Normalize(raw); err != nil || got != c.want {
				t.Errorf("%s: Normalize(%q) = %q, %v; want %q", c.name, raw, got, err, c.want)
			}
		}
	}

	// The case of the path tells repositories apart; the host and the path do too.
	distinct := []string{"git@github.com:Org/Repo.git", "git@github.com:org/other.git", "git@gitlab.com:org/repo.git"}
	seen := map[string]string{}
	for _, raw := range distinct {
		got, err := gitctx.Normalize(raw)
		if err != nil {
			t.Fatal(err)
		}
		if prev, dup := seen[got]; dup {
			t.Errorf("%q and %q collide as %q", raw, prev, got)
		}
		seen[got] = raw
	}

	for _, bad := range []string{"", "  ", "https://", "https://github.com", "ssh://git@host/"} {
		if got, err := gitctx.Normalize(bad); err == nil {
			t.Errorf("Normalize(%q) = %q, want an error", bad, got)
		}
	}
}

func TestMatch(t *testing.T) {
	projects := []v1.Project{
		{Name: "backend", Repo: "git@github.com:org/backend.git"},
		{Name: "backend-mirror", Repo: "https://github.com/org/backend"},
		{Name: "web", Repo: "git@github.com:org/web.git"},
		{Name: "broken", Repo: "https://"},
	}
	got, err := gitctx.Match("ssh://git@GitHub.com/org/backend.git", projects)
	if err != nil || len(got) != 2 || got[0].Name != "backend" || got[1].Name != "backend-mirror" {
		t.Errorf("Match = %+v, %v", got, err)
	}
	if got, _ := gitctx.Match("git@github.com:org/none.git", projects); len(got) != 0 {
		t.Errorf("matched %+v", got)
	}
	if _, err := gitctx.Match("", projects); err == nil {
		t.Error("an empty origin matched")
	}
}

// repo is a real git repository with history, and the commits it made.
type repo struct {
	t   *testing.T
	dir string
}

func newRepo(t *testing.T) *repo {
	t.Helper()
	t.Setenv("GIT_CONFIG_GLOBAL", "/dev/null")
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	r := &repo{t: t, dir: t.TempDir()}
	r.git("init", "-q", "-b", "main")
	return r
}

func (r *repo) git(args ...string) string {
	r.t.Helper()
	cmd := exec.CommandContext(r.t.Context(), "git", append([]string{"-C", r.dir, //nolint:gosec // test helper: fixed git options, arguments from the test
		"-c", "user.name=t", "-c", "user.email=t@example.com", "-c", "commit.gpgsign=false", "-c", "tag.gpgsign=false"}, args...)...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		r.t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

func (r *repo) commit(msg string) string {
	r.t.Helper()
	r.git("commit", "-q", "--allow-empty", "-m", msg)
	return r.git("rev-parse", "HEAD")
}

func TestResolveRef_AgainstARealOrigin(t *testing.T) {
	origin := newRepo(t)
	first := origin.commit("first")
	origin.git("tag", "light")
	origin.git("tag", "-a", "-m", "annotated", "annotated")
	origin.git("tag", "dual") // a tag and a branch of one name: the branch wins
	main := origin.commit("second")
	origin.git("checkout", "-q", "-b", "dev")
	dev := origin.commit("on dev")
	origin.git("branch", "dual")
	origin.git("checkout", "-q", "main")
	url := "file://" + origin.dir

	clone := newRepo(t)
	clone.git("remote", "add", "origin", url)

	for _, c := range []struct{ ref, want string }{
		{"main", main},
		{"dev", dev},
		{"refs/heads/dev", dev},
		{"light", first},
		{"annotated", first}, // the commit, not the tag object
		{"refs/tags/annotated", first},
		{"dual", dev},
		{"HEAD", main},
		{dev, dev}, // a full commit ID needs no remote
	} {
		for _, remote := range []string{"origin", url} {
			got, err := gitctx.ResolveRef(t.Context(), clone.dir, remote, c.ref)
			if err != nil || got != c.want {
				t.Errorf("ResolveRef(%q, %q) = %q, %v; want %q", remote, c.ref, got, err, c.want)
			}
		}
	}

	_, err := gitctx.ResolveRef(t.Context(), clone.dir, "origin", "nope")
	var de *domain.Error
	if !errors.As(err, &de) || de.Code != domain.CodeNotFound {
		t.Errorf("a missing ref: %v", err)
	}
	for _, ref := range []string{"", "--upload-pack=x", "-h"} {
		if _, err := gitctx.ResolveRef(t.Context(), clone.dir, "origin", ref); !errors.As(err, &de) || de.Code != domain.CodeInvalidRequest {
			t.Errorf("ref %q: %v", ref, err)
		}
	}
	if _, err := gitctx.ResolveRef(t.Context(), clone.dir, "--upload-pack=x", "main"); !errors.As(err, &de) || de.Code != domain.CodeInvalidRequest {
		t.Errorf("a remote that is an option: %v", err)
	}
}

func TestResolveRef_FailuresDoNotLeakCredentials(t *testing.T) {
	dir := newRepo(t).dir
	_, err := gitctx.ResolveRef(t.Context(), dir, "https://user:hunter2@127.0.0.1:1/org/repo.git", "main")
	if err == nil {
		t.Fatal("an unreachable origin resolved")
	}
	if strings.Contains(err.Error(), "hunter2") {
		t.Errorf("the error leaks the password: %v", err)
	}
	if _, err := gitctx.ResolveRef(t.Context(), dir, "origin", "main"); err == nil {
		t.Error("a repository with no origin resolved")
	}
}

func TestOriginURL(t *testing.T) {
	r := newRepo(t)
	if _, err := gitctx.OriginURL(t.Context(), r.dir); !errors.Is(err, gitctx.ErrNoOrigin) {
		t.Errorf("no remote: %v", err)
	}
	r.git("remote", "add", "origin", "git@github.com:org/repo.git")
	r.git("remote", "add", "upstream", "git@github.com:other/repo.git")
	got, err := gitctx.OriginURL(t.Context(), r.dir)
	if err != nil || got != "git@github.com:org/repo.git" {
		t.Errorf("OriginURL = %q, %v", got, err)
	}
	if _, err := gitctx.OriginURL(t.Context(), t.TempDir()); !errors.Is(err, gitctx.ErrNoOrigin) {
		t.Errorf("not a repository: %v", err)
	}
}
