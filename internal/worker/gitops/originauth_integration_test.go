//go:build integration

package gitops_test

import (
	"bytes"
	"encoding/base64"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/cgi" //nolint:gosec // the origin of the tests runs git http-backend; Go is far past 1.6.3
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Autarquico/codefabric/internal/config"
	"github.com/Autarquico/codefabric/internal/support/clock"
	"github.com/Autarquico/codefabric/internal/support/gittest"
	"github.com/Autarquico/codefabric/internal/support/redact"
	"github.com/Autarquico/codefabric/internal/worker/daemon"
	"github.com/Autarquico/codefabric/internal/worker/gitops"
	"github.com/Autarquico/codefabric/internal/worker/secretjit"
)

const (
	asCF       = "GITOPS_TEST_AS_CF"
	originUser = "x-access-token"
	// The token has characters that a URL or a header would encode.
	originToken = "tk/+=abc123XYZ_origin-secret" //nolint:gosec // a sentinel of the tests, not a credential
	originRef   = "github.personal"
)

// TestMain lets the test binary be the `cf` that the askpass script execs.
func TestMain(m *testing.M) {
	if os.Getenv(asCF) == "1" {
		cmd := daemon.Command()
		cmd.SetArgs(os.Args[1:][1:]) // without "worker"
		cmd.SilenceErrors, cmd.SilenceUsage = true, true
		if cmd.Execute() != nil {
			os.Exit(1)
		}
		os.Exit(0)
	}
	os.Exit(m.Run())
}

// noWait is a clock whose backoffs are over at once: the failures under test
// are retried without the seconds.
type noWait struct{ clock.Real }

type fired struct{ c chan time.Time }

func (f fired) C() <-chan time.Time { return f.c }
func (fired) Stop()                 {}

func (noWait) NewTimer(time.Duration) clock.Timer {
	c := make(chan time.Time, 1)
	c <- time.Now()
	return fired{c}
}

type seen struct {
	Path       string
	Authorized bool
}

// origin is a smart-HTTP TLS server with Basic auth over bare repositories.
type origin struct {
	srv      *httptest.Server
	ca       string
	mu       sync.Mutex
	requests []seen
	redirect string // after the auth, the answer is a redirect to this base URL
}

func newOrigin(t *testing.T, repos map[string]string) *origin {
	t.Helper()
	gitPath, err := exec.LookPath("git")
	if err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	for name, src := range repos {
		dst := filepath.Join(root, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(dst), 0o700); err != nil {
			t.Fatal(err)
		}
		gittest.Run(t, root, "clone", "--bare", "--quiet", src, dst)
	}
	backend := &cgi.Handler{
		Path: gitPath, Args: []string{"http-backend"},
		Env: []string{"GIT_PROJECT_ROOT=" + root, "GIT_HTTP_EXPORT_ALL=1", "HOME=" + root, "GIT_CONFIG_NOSYSTEM=1"},
	}
	o := &origin{}
	o.srv = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		user, pass, ok := req.BasicAuth()
		authorized := ok && user == originUser && pass == originToken
		o.mu.Lock()
		o.requests = append(o.requests, seen{Path: req.URL.Path, Authorized: authorized})
		redirect := o.redirect
		o.mu.Unlock()
		switch {
		case !authorized:
			w.Header().Set("WWW-Authenticate", `Basic realm="origin"`)
			http.Error(w, "authentication needed", http.StatusUnauthorized)
		case redirect != "":
			http.Redirect(w, req, redirect+req.URL.RequestURI(), http.StatusTemporaryRedirect) //nolint:gosec // the redirect under test
		default:
			backend.ServeHTTP(w, req)
		}
	}))
	t.Cleanup(o.srv.Close)
	o.ca = writePEM(t, filepath.Join(t.TempDir(), "ca.pem"), "CERTIFICATE", o.srv.Certificate().Raw)
	return o
}

func (o *origin) saw() []seen {
	o.mu.Lock()
	defer o.mu.Unlock()
	return append([]seen(nil), o.requests...)
}

func (o *origin) authorizedFor(path string) int {
	n := 0
	for _, r := range o.saw() {
		if r.Authorized && strings.HasPrefix(r.Path, path) {
			n++
		}
	}
	return n
}

// askpassRig prepares Runs of one Project from an origin that wants a credential.
type askpassRig struct {
	t      *testing.T
	paths  config.Paths
	home   string
	origin *origin
	commit string
	auth   *gitops.OriginAuth
	g      *gitops.Git
	logs   *bytes.Buffer
}

func newAskpassRig(t *testing.T, trust bool) *askpassRig {
	t.Helper()
	gittest.Require(t)
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", "")
	t.Setenv("NO_PROXY", "127.0.0.1")
	for _, k := range []string{"HTTPS_PROXY", "https_proxy"} {
		t.Setenv(k, "")
	}
	src := gittest.InitRepo(t)
	other := gittest.InitRepo(t)
	r := &askpassRig{t: t, home: home, logs: &bytes.Buffer{}, commit: gittest.Run(t, src, "rev-parse", "HEAD")}
	r.origin = newOrigin(t, map[string]string{"org/repo.git": src, "org/other.git": other})
	// Everything of the Worker is under a path with spaces and a quote.
	r.paths = config.Paths{ConfigDir: t.TempDir(), DataDir: filepath.Join(t.TempDir(), "data dir, it's here")}
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	script, err := daemon.EnsureAskpassScript(r.paths, []string{"/usr/bin/env", asCF + "=1", self, "worker", "git-askpass"})
	if err != nil {
		t.Fatal(err)
	}
	runDir := r.paths.WorkerRunDir(run, 1)
	if err := os.MkdirAll(runDir, 0o700); err != nil {
		t.Fatal(err)
	}
	needle := redact.Needle{Ref: originRef, Value: []byte(originToken), User: originUser}
	m := secretjit.New()
	if err := m.DeliverFor(runDir, 1, []secretjit.Consumer{secretjit.Askpass}, []redact.Needle{needle}, func() error { return nil }); err != nil {
		t.Fatal(err)
	}
	if err := m.Bind(runDir, originRef, secretjit.Binding{Origin: r.url("org/repo.git"), User: originUser}); err != nil {
		t.Fatal(err)
	}
	r.auth = &gitops.OriginAuth{Askpass: script, RunDir: runDir, Needles: []redact.Needle{needle}}
	r.g, err = gitops.New(t.Context(), gitops.Options{Paths: r.paths, Clock: noWait{}, Log: slog.New(slog.NewTextHandler(r.logs, nil))})
	if err != nil {
		t.Fatal(err)
	}
	if trust {
		gitops.TrustCA(r.g, r.origin.ca)
	}
	return r
}

func (r *askpassRig) url(path string) string { return r.origin.srv.URL + "/" + path }

func (r *askpassRig) spec(repo string) gitops.PrepareSpec {
	return gitops.PrepareSpec{
		Project: project, Run: run, LeaseEpoch: 1, Repo: repo, SourceRef: "main", SourceCommit: r.commit,
		Branch: branch, RunDir: r.auth.RunDir, Auth: r.auth,
	}
}

func (r *askpassRig) prepare(repo string) error {
	return r.g.Prepare(r.t.Context(), r.spec(repo), r.paths.WorkerWorktreeDir(run, 1))
}

func (r *askpassRig) noLeak(err error) {
	r.t.Helper()
	text := r.logs.String()
	if err != nil {
		text += err.Error()
	}
	user := originUser + ":" + originToken
	for _, form := range []string{
		originToken, url.QueryEscape(originToken), url.PathEscape(originToken),
		base64.StdEncoding.EncodeToString([]byte(user)), base64.RawStdEncoding.EncodeToString([]byte(user)),
	} {
		if strings.Contains(text, form) {
			r.t.Errorf("the credential (%q) is in the errors or the log:\n%s", form, text)
		}
	}
}

func TestScenario_OriginAskpass_RepoPrivado(t *testing.T) {
	r := newAskpassRig(t, true)
	// The hostile configuration of the user must change nothing.
	record := filepath.Join(t.TempDir(), "helper-calls")
	hostile := fmt.Sprintf("[credential]\n\thelper = \"!f() { echo called >> '%s'; echo username=x; echo password=y; }; f\"\n"+
		"[url \"https://evil.invalid/\"]\n\tinsteadOf = %s/\n[http]\n\tsslVerify = false\n"+
		"[http \"%s/\"]\n\tsslVerify = false\n\textraHeader = X-Hostile: 1\n[core]\n\taskPass = /bin/false\n",
		record, r.origin.srv.URL, r.origin.srv.URL)
	if err := os.WriteFile(filepath.Join(r.home, ".gitconfig"), []byte(hostile), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(r.home, ".config", "git"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(r.home, ".config", "git", "config"), []byte(hostile), 0o600); err != nil {
		t.Fatal(err)
	}

	// Control: the same hostile configuration does bite a git that is not isolated.
	ctl := exec.CommandContext(t.Context(), "git", "ls-remote", r.url("org/repo.git")) //nolint:gosec // a test
	helperOnly := filepath.Join(t.TempDir(), "helper-only")
	if err := os.WriteFile(helperOnly, []byte(strings.SplitN(hostile, "[url", 2)[0]), 0o600); err != nil {
		t.Fatal(err)
	}
	ctl.Env = []string{"PATH=" + os.Getenv("PATH"), "HOME=" + r.home, "GIT_CONFIG_GLOBAL=" + helperOnly, "GIT_TERMINAL_PROMPT=0", "GIT_SSL_CAINFO=" + r.origin.ca}
	_ = ctl.Run()
	if _, err := os.Stat(record); err != nil {
		t.Fatalf("the recording helper is not invoked by an unisolated git: %v", err)
	}
	if err := os.Remove(record); err != nil {
		t.Fatal(err)
	}

	if err := r.prepare(r.url("org/repo.git")); err != nil {
		t.Fatalf("Prepare: %v\n%s", err, r.logs)
	}
	if got := gittest.Run(t, r.paths.WorkerWorktreeDir(run, 1), "rev-parse", "HEAD"); got != r.commit {
		t.Errorf("HEAD = %s, want %s", got, r.commit)
	}
	if n := r.origin.authorizedFor("/org/repo.git"); n == 0 {
		t.Errorf("origin saw no authorized request: %v", r.origin.saw())
	}
	if _, err := os.Stat(record); err == nil {
		t.Error("a credential helper was invoked")
	}
	r.noLeak(nil)
	// Nothing stored the credential.
	for _, f := range []string{filepath.Join(r.home, ".git-credentials"), filepath.Join(r.paths.WorkerRepoDir(project), "config")} {
		if data, err := os.ReadFile(f); err == nil && bytes.Contains(data, []byte(originToken)) { //nolint:gosec // a test
			t.Errorf("%s holds the credential", f)
		}
	}
}

func TestScenario_OriginAskpass_HostileConfigDoesNotDisableTLSVerification(t *testing.T) {
	r := newAskpassRig(t, false) // the CA of the server is not trusted
	hostile := fmt.Sprintf("[http]\n\tsslVerify = false\n[http \"%s/\"]\n\tsslVerify = false\n", r.origin.srv.URL)
	if err := os.WriteFile(filepath.Join(r.home, ".gitconfig"), []byte(hostile), 0o600); err != nil {
		t.Fatal(err)
	}
	err := r.prepare(r.url("org/repo.git"))
	if err == nil {
		t.Fatal("Prepare worked against a server nobody trusts")
	}
	if n := r.origin.authorizedFor("/"); n != 0 {
		t.Errorf("the credential reached an untrusted server %d times", n)
	}
	r.noLeak(err)
}

func TestScenario_OriginAskpass_DestinoNoAutorizado(t *testing.T) {
	t.Run("a redirect to another host", func(t *testing.T) {
		r := newAskpassRig(t, true)
		elsewhere := newOrigin(t, map[string]string{"org/repo.git": gittest.InitRepo(t)})
		r.origin.mu.Lock()
		r.origin.redirect = elsewhere.srv.URL
		r.origin.mu.Unlock()
		err := r.prepare(r.url("org/repo.git"))
		if err == nil {
			t.Fatal("Prepare followed a redirect")
		}
		if got := elsewhere.saw(); len(got) != 0 {
			t.Errorf("the other host was reached: %v", got)
		}
		r.noLeak(err)
	})
	t.Run("another repository of the same host", func(t *testing.T) {
		r := newAskpassRig(t, true)
		err := r.prepare(r.url("org/other.git"))
		if err == nil {
			t.Fatal("Prepare fetched a repository the credential is not for")
		}
		if n := r.origin.authorizedFor("/org/other.git"); n != 0 {
			t.Errorf("the credential reached another repository %d times", n)
		}
		r.noLeak(err)
	})
	t.Run("a repository that is not https", func(t *testing.T) {
		r := newAskpassRig(t, true)
		err := r.prepare(strings.Replace(r.url("org/repo.git"), "https://", "http://", 1))
		if err == nil || strings.Contains(err.Error(), originToken) {
			t.Fatalf("Prepare of an http repository: %v", err)
		}
		if got := r.origin.saw(); len(got) != 0 {
			t.Errorf("git ran against an http repository: %v", got)
		}
	})
	t.Run("a revoked credential", func(t *testing.T) {
		r := newAskpassRig(t, true)
		if err := secretjit.New().Revoke(r.auth.RunDir, secretjit.Askpass, nil); err != nil {
			t.Fatal(err)
		}
		err := r.prepare(r.url("org/repo.git"))
		if err == nil {
			t.Fatal("Prepare worked with the credential revoked")
		}
		if n := r.origin.authorizedFor("/"); n != 0 {
			t.Errorf("the credential was used after it was revoked: %d", n)
		}
		r.noLeak(err)
	})
}

func TestScenario_OriginAskpass_ErrorDeFetch(t *testing.T) {
	gittest.Require(t)
	r := newAskpassRig(t, true)
	// A git that fails the way some do: its error shows the URL it was given
	// with the credential in it, and its header.
	realGit, err := exec.LookPath("git")
	if err != nil {
		t.Fatal(err)
	}
	user := originUser + ":" + originToken
	script := filepath.Join(t.TempDir(), "git")
	body := fmt.Sprintf(`#!/bin/sh
case "$1" in fetch)
echo "fatal: unable to access 'https://%s@example.com/o/r.git/': 403" >&2
echo "fatal: token %s encoded %s header Authorization: Basic %s" >&2
echo "out: %s"
exit 128;;
esac
exec '%s' "$@"
`, user, originToken, url.QueryEscape(originToken), base64.StdEncoding.EncodeToString([]byte(user)), originToken, realGit)
	if err := os.WriteFile(script, []byte(body), 0o700); err != nil { //nolint:gosec // a fake git of the test
		t.Fatal(err)
	}
	g, err := gitops.New(t.Context(), gitops.Options{Paths: r.paths, Clock: noWait{}, Log: slog.New(slog.NewTextHandler(r.logs, nil)), GitPath: script})
	if err != nil {
		t.Fatal(err)
	}
	err = g.Prepare(t.Context(), r.spec(r.url("org/repo.git")), r.paths.WorkerWorktreeDir(run, 1))
	if err == nil {
		t.Fatal("Prepare worked with a failing fetch")
	}
	r.noLeak(err)
	if !strings.Contains(r.logs.String(), "[REDACTED:"+originRef+"]") {
		t.Errorf("the log does not show the redaction marker:\n%s", r.logs)
	}
	if err := g.EnsureSource(t.Context(), gitops.SourceSpec{
		Project: project, Run: run, Repo: r.url("org/repo.git"), SourceRef: "main",
		SourceCommit: strings.Repeat("a", 40), Auth: r.auth,
	}); err == nil {
		t.Error("EnsureSource of an unknown commit worked")
	} else {
		r.noLeak(err)
	}
}
