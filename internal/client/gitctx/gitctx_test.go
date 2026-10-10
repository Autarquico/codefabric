package gitctx_test

import (
	"errors"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

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
			"ssh://git@github.com:22/org/repo/",
			"https://github.com/org/repo.git",
			"https://github.com:443/org/repo",
			"https://user:secret@GITHUB.COM/org/repo/",
			"http://github.com:80/org/repo",
			"git://github.com:9418/org/repo.git",
			"git://github.com/org/repo.git",
			"  https://github.com/org/repo.git\n",
		}, "github.com/org/repo"},
		{"an absolute scp path", []string{"git@host.example:/srv/git/repo.git", "ssh://host.example//srv/git/repo.git"}, "host.example/srv/git/repo"},
		{"a local repository", []string{"/srv/git/repo", "file:///srv/git/repo", "/srv/git//repo/"}, "file:/srv/git/repo"},
		{"a local bare repository", []string{"/srv/git/repo.git", "file:///srv/git/repo.git"}, "file:/srv/git/repo.git"},
	}
	for _, c := range same {
		for _, raw := range c.urls {
			if got, err := gitctx.Normalize(raw); err != nil || got != c.want {
				t.Errorf("%s: Normalize(%q) = %q, %v; want %q", c.name, raw, got, err, c.want)
			}
		}
	}

	// The case of the path, the host, a port that is not the default and a local ".git" tell repositories apart.
	distinct := []string{
		"git@github.com:Org/Repo.git", "git@github.com:org/other.git", "git@gitlab.com:org/repo.git", "git@github.com:org/repo.git",
		"ssh://git@github.com:2222/org/repo", "ssh://git@github.com:2223/org/repo", "http://github.com:8080/org/repo", "https://github.com:8443/org/repo",
		"/srv/git/repo", "/srv/git/repo.git",
	}
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

func TestOriginURL_RelativeLocalOriginsAreAbsoluteToTheRepository(t *testing.T) {
	r := newRepo(t)
	// git reports the repository under its real path, which a temp dir may only link to.
	parent, err := filepath.EvalSymlinks(filepath.Dir(r.dir))
	if err != nil {
		t.Fatal(err)
	}
	want := filepath.Join(parent, "sibling.git")
	subdir := filepath.Join(r.dir, "pkg")
	if err := os.Mkdir(subdir, 0o750); err != nil {
		t.Fatal(err)
	}
	for _, origin := range []string{"../sibling.git", "file://../sibling.git"} {
		r.git("config", "remote.origin.url", origin)
		for _, from := range []string{r.dir, subdir} {
			if got, err := gitctx.OriginURL(t.Context(), from); err != nil || got != want {
				t.Errorf("origin %q seen from %s: OriginURL = %q, %v; want %q", origin, from, got, err, want)
			}
		}
	}
	got, _ := gitctx.OriginURL(t.Context(), r.dir)
	if match, err := gitctx.Match(got, []v1.Project{{Repo: want}}); err != nil || len(match) != 1 {
		t.Errorf("the absolute origin should match a Project on that path: %v, %v", match, err)
	}
}

func TestStripCredentials(t *testing.T) {
	for _, c := range []struct {
		in, want string
		removed  bool
	}{
		{"https://user:s3cret@github.com/org/repo.git", "https://github.com/org/repo.git", true},
		{"https://ghp_token@github.com/org/repo.git", "https://github.com/org/repo.git", true},
		{"https://:ghp_token@github.com/org/repo.git", "https://github.com/org/repo.git", true},
		{"ssh://git:s3cret@github.com/org/repo.git", "ssh://git@github.com/org/repo.git", true},
		{"ssh://git@github.com/org/repo.git", "ssh://git@github.com/org/repo.git", false},
		{"git@github.com:org/repo.git", "git@github.com:org/repo.git", false},
		{"https://github.com/org/repo.git", "https://github.com/org/repo.git", false},
		{"/srv/repo", "/srv/repo", false},
	} {
		got, removed := gitctx.StripCredentials(c.in)
		if got != c.want || removed != c.removed {
			t.Errorf("gitctx.StripCredentials(%q) = (%q, %v), want (%q, %v)", c.in, got, removed, c.want, c.removed)
		}
	}
}

// A remote that takes the connection and never answers ends at the deadline.
func TestResolveRef_ARemoteThatNeverAnswersTimesOut(t *testing.T) {
	lc := net.ListenConfig{}
	l, err := lc.Listen(t.Context(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = l.Close() }()
	go func() {
		for {
			if _, err := l.Accept(); err != nil { // the connection is held open, unanswered
				return
			}
		}
	}()

	start := time.Now()
	_, err = gitctx.ResolveRefWithin(t.Context(), newRepo(t).dir, "git://"+l.Addr().String()+"/repo", "main", 300*time.Millisecond)
	if err == nil || !strings.Contains(err.Error(), "no answer within") {
		t.Fatalf("err = %v", err)
	}
	if took := time.Since(start); took > 10*time.Second {
		t.Errorf("took %s", took)
	}
}
