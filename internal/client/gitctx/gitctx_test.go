package gitctx_test

import (
	"errors"
	"net"
	"os/exec"
	"slices"
	"strings"
	"testing"
	"time"

	v1 "github.com/Autarquico/codefabric/internal/api/v1"
	"github.com/Autarquico/codefabric/internal/client/gitctx"
	"github.com/Autarquico/codefabric/internal/domain"
)

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

func projects() []v1.Project {
	return []v1.Project{
		{Name: "backend", Repo: "git@github.com:org/backend.git", RepoAliases: []string{"https://github.com/org/backend.git", "/srv/git/backend"}},
		{Name: "web", Repo: "git@github.com:org/web.git"},
		{Name: "mirror", Repo: "ssh://git@mirror.example/org/backend", RepoAliases: []string{"git@github.com:org/backend.git"}},
	}
}

func names(ps []v1.Project) []string {
	out := make([]string, len(ps))
	for i, p := range ps {
		out[i] = p.Name
	}
	return out
}

func TestMatch_IsLiteralEqualityWithTheMainRepoOrAnAlias(t *testing.T) {
	for origin, want := range map[string][]string{
		"git@github.com:org/web.git":         {"web"},
		"https://github.com/org/backend.git": {"backend"}, // an alias
		"/srv/git/backend":                   {"backend"},
		"  git@github.com:org/web.git\n":     {"web"}, // only the white space around it goes
		"git@github.com:org/backend.git":     {"backend", "mirror"},
		"file:///srv/git/backend":            nil, // another spelling of an alias is not the alias
	} {
		got, err := gitctx.Match(origin, projects())
		if err != nil || !slices.Equal(names(got), want) {
			t.Errorf("Match(%q) = %v, %v; want %v", origin, names(got), err, want)
		}
	}
}

func TestMatch_NoFuzzyMatchWithoutAnAlias(t *testing.T) {
	// Other spellings of git@github.com:org/web.git: none of them is it.
	for _, origin := range []string{
		"https://github.com/org/web.git", "ssh://git@github.com/org/web.git", "git@GitHub.com:org/web.git",
		"git@github.com:org/Web.git", "git@github.com:org/web", "git@github.com:org/web.git/", "ssh://git@github.com:22/org/web.git",
		"alice@github.com:org/web.git", "github.com:org/web.git", "git@github.com:/org/web.git",
	} {
		if got, err := gitctx.Match(origin, projects()); err != nil || len(got) != 0 {
			t.Errorf("Match(%q) = %v, %v; want no match", origin, names(got), err)
		}
	}
	// Adding the spelling as an alias is how it matches.
	ps := projects()
	ps[1].RepoAliases = []string{"https://github.com/org/web"}
	if got, err := gitctx.Match("https://github.com/org/web", ps); err != nil || !slices.Equal(names(got), []string{"web"}) {
		t.Errorf("with the alias: %v, %v", names(got), err)
	}
}

func TestMatch_DifferentUsersAreDifferentRepositories(t *testing.T) {
	ps := []v1.Project{{Name: "a", Repo: "alice@host:repo"}, {Name: "b", Repo: "bob@host:repo"}}
	got, err := gitctx.Match("alice@host:repo", ps)
	if err != nil || !slices.Equal(names(got), []string{"a"}) {
		t.Errorf("Match = %v, %v", names(got), err)
	}
	if got, _ := gitctx.Match("carol@host:repo", ps); len(got) != 0 {
		t.Errorf("matched %v", names(got))
	}
}

func TestMatch_AnUnsupportedOriginIdentifiesNothing(t *testing.T) {
	ps := []v1.Project{{Name: "p", Repo: "../repo"}, {Name: "q", Repo: "file://../repo"}}
	for _, origin := range []string{"../repo", "repo", "./repo", "~/repo", "file://../repo", "file://repo", "https://u:p@h/r", "user:pw@host:path"} {
		if got, err := gitctx.Match(origin, ps); !errors.Is(err, gitctx.ErrUnsupportedOrigin) || len(got) != 0 {
			t.Errorf("Match(%q) = %v, %v; want ErrUnsupportedOrigin", origin, names(got), err)
		}
	}
	if _, err := gitctx.Match("", ps); err == nil {
		t.Error("an empty origin matched")
	}
}

func TestOriginURL_IsWhatGitHasNeverResolved(t *testing.T) {
	r := newRepo(t)
	for _, origin := range []string{"../sibling.git", "file://../sibling.git", "git@GitHub.com:Org/Repo.git/", "https://h/r?x=1"} {
		r.git("config", "remote.origin.url", origin)
		if got, err := gitctx.OriginURL(t.Context(), r.dir); err != nil || got != origin {
			t.Errorf("OriginURL = %q, %v; want %q", got, err, origin)
		}
	}
}

func TestRedact_ReplacesWholeWordsWithAnAtOrAQueryURL(t *testing.T) {
	for raw, want := range map[string]string{ //nolint:gosec // test data: fake secrets
		"user:SECRET@host:repo,https://h/other":               "<redacted>",
		"user:pre'SECRET@host:repo":                           "<redacted>",
		"https://h/r?access_token=prefix@SECRET":              "<redacted>",
		"https://h/r#SECRET":                                  "<redacted>",
		"git@github.com:org/repo.git":                         "<redacted>",
		"fatal: unable to access 'https://h/r?t=SECRET': 404": "fatal: unable to access <redacted> 404",
		"fatal: repository not found?  Did you mean?":         "fatal: repository not found?  Did you mean?",
		"ref main\tnot found on https://h/r":                  "ref main\tnot found on https://h/r",
		"a  word\nwithout at":                                 "a  word\nwithout at",
	} {
		if got := gitctx.Redact(raw); got != want {
			t.Errorf("Redact(%q) = %q, want %q", raw, got, want)
		}
	}
}

func TestResolveRef_FailuresDoNotLeakAQueryToken(t *testing.T) {
	_, err := gitctx.ResolveRef(t.Context(), newRepo(t).dir, "https://127.0.0.1:1/org/repo.git?access_token=SECRET", "main")
	if err == nil || strings.Contains(err.Error(), "SECRET") {
		t.Errorf("err = %v", err)
	}
}
