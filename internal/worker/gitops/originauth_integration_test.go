//go:build integration

package gitops_test

import (
	"bytes"
	"crypto/rand"
	"encoding/base64"
	"fmt"
	"log/slog"
	"net"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Autarquico/codefabric/internal/config"
	"github.com/Autarquico/codefabric/internal/support/gittest"
	"github.com/Autarquico/codefabric/internal/support/redact"
	"github.com/Autarquico/codefabric/internal/testutil/gitorigin"
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

// askpassRig prepares Runs of one Project from an origin that wants a credential.
type askpassRig struct {
	t      *testing.T
	paths  config.Paths
	home   string
	origin *gitorigin.Origin
	commit string
	src    string
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
	r := &askpassRig{t: t, src: src, home: home, logs: &bytes.Buffer{}, commit: gittest.Run(t, src, "rev-parse", "HEAD")}
	r.origin = gitorigin.New(t, map[string]string{"org/repo.git": src, "org/other.git": other}, originUser, originToken)
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
	r.g, err = gitops.New(t.Context(), gitops.Options{Paths: r.paths, Clock: gitorigin.NoWait{}, Log: slog.New(slog.NewTextHandler(r.logs, nil))})
	if err != nil {
		t.Fatal(err)
	}
	if trust {
		gitops.TrustCA(r.g, r.origin.CA)
	}
	return r
}

func (r *askpassRig) url(path string) string { return r.origin.URL + "/" + path }

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

func TestOriginAuth_RepoPrivado(t *testing.T) {
	r := newAskpassRig(t, true)
	// The hostile configuration of the user must change nothing.
	record := gitorigin.HostileHome(t, r.home, r.origin)

	if err := r.prepare(r.url("org/repo.git")); err != nil {
		t.Fatalf("Prepare: %v\n%s", err, r.logs)
	}
	if got := gittest.Run(t, r.paths.WorkerWorktreeDir(run, 1), "rev-parse", "HEAD"); got != r.commit {
		t.Errorf("HEAD = %s, want %s", got, r.commit)
	}
	if n := r.origin.AuthorizedFor("/org/repo.git"); n == 0 {
		t.Errorf("origin saw no authorized request: %v", r.origin.Saw())
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

func TestOriginAuth_HostileConfigDoesNotDisableTLSVerification(t *testing.T) {
	r := newAskpassRig(t, false) // the CA of the server is not trusted
	hostile := fmt.Sprintf("[http]\n\tsslVerify = false\n[http \"%s/\"]\n\tsslVerify = false\n", r.origin.URL)
	if err := os.WriteFile(filepath.Join(r.home, ".gitconfig"), []byte(hostile), 0o600); err != nil {
		t.Fatal(err)
	}
	err := r.prepare(r.url("org/repo.git"))
	if err == nil {
		t.Fatal("Prepare worked against a server nobody trusts")
	}
	if n := r.origin.AuthorizedFor("/"); n != 0 {
		t.Errorf("the credential reached an untrusted server %d times", n)
	}
	r.noLeak(err)
}

// plantHostileBare makes the bare repository of the Project, as the Worker
// does, and gives its own config what a harness of another Run could write
// there for the origin: no TLS verification, a CA of its own, a proxy, a header
// and an include. It returns how many connections the proxy took.
func (r *askpassRig) plantHostileBare(t *testing.T) (proxyHits func() int) {
	t.Helper()
	ln, err := (&net.ListenConfig{}).Listen(t.Context(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	var hits atomic.Int32
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			hits.Add(1)
			_ = c.Close()
		}
	}()
	t.Setenv("NO_PROXY", "") // the rig exempts the origin; a bare config must not need that to be ignored
	t.Setenv("no_proxy", "")
	bare := r.paths.WorkerRepoDir(project)
	init := exec.CommandContext(t.Context(), "git", "init", "--bare", "--quiet", bare) //nolint:gosec // a test
	init.Env = []string{"PATH=" + os.Getenv("PATH"), "HOME=" + r.home, "GIT_CONFIG_NOSYSTEM=1"}
	if out, err := init.CombinedOutput(); err != nil {
		t.Fatalf("git init: %v\n%s", err, out)
	}
	included := filepath.Join(t.TempDir(), "included")
	if err := os.WriteFile(included, []byte("[http]\n\tsslVerify = false\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	hostile := fmt.Sprintf("[http %q]\n\tsslVerify = false\n\tsslCAInfo = %s\n\tproxy = http://%s\n\textraHeader = Authorization: Basic %s\n"+
		"[url \"https://evil.invalid/\"]\n\tinsteadOf = %s/\n[include]\n\tpath = %s\n",
		r.origin.URL+"/", filepath.Join(t.TempDir(), "no-such-ca.pem"), ln.Addr(), base64.StdEncoding.EncodeToString([]byte("attacker:wrong")), r.origin.URL, included)
	f, err := os.OpenFile(filepath.Join(bare, "config"), os.O_APPEND|os.O_WRONLY, 0o600) //nolint:gosec // a test
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString(hostile); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	// The control: a git run inside that repository does take the hostile value.
	ctl := exec.CommandContext(t.Context(), "git", "config", "--get-urlmatch", "http.sslVerify", r.url("org/repo.git")) //nolint:gosec // a test
	ctl.Dir, ctl.Env = bare, []string{"PATH=" + os.Getenv("PATH"), "HOME=" + r.home, "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_COUNT=1", "GIT_CONFIG_KEY_0=http.sslVerify", "GIT_CONFIG_VALUE_0=true"}
	if out, _ := ctl.Output(); strings.TrimSpace(string(out)) != "false" {
		t.Fatalf("the planted config does not override the command line (%q): the test proves nothing", out)
	}
	return func() int { return int(hits.Load()) }
}

func TestOriginAuth_HostileBareConfigIsNotRead(t *testing.T) {
	t.Run("an untrusted server stays untrusted", func(t *testing.T) {
		r := newAskpassRig(t, false)
		proxied := r.plantHostileBare(t)
		err := r.prepare(r.url("org/repo.git"))
		if err == nil {
			t.Fatal("Prepare worked against a server nobody trusts")
		}
		if n := r.origin.AuthorizedFor("/"); n != 0 {
			t.Errorf("the credential reached an untrusted server %d times", n)
		}
		if n := proxied(); n != 0 {
			t.Errorf("the fetch went through the proxy of the bare config %d times", n)
		}
		r.noLeak(err)
	})
	t.Run("a trusted server is fetched as configured by the Worker", func(t *testing.T) {
		r := newAskpassRig(t, true)
		proxied := r.plantHostileBare(t)
		if err := r.prepare(r.url("org/repo.git")); err != nil {
			t.Fatalf("Prepare: %v\n%s", err, r.logs)
		}
		if n := proxied(); n != 0 {
			t.Errorf("the fetch went through the proxy of the bare config %d times", n)
		}
		if n := r.origin.AuthorizedFor("/org/repo.git"); n == 0 {
			t.Errorf("origin saw no authorized request: %v", r.origin.Saw())
		}
		if got := gittest.Run(t, r.paths.WorkerWorktreeDir(run, 1), "rev-parse", "HEAD"); got != r.commit {
			t.Errorf("HEAD = %s, want %s", got, r.commit)
		}
		r.noLeak(nil)
	})
}

func TestOriginAuth_DestinoNoAutorizado(t *testing.T) {
	t.Run("a redirect to another host", func(t *testing.T) {
		r := newAskpassRig(t, true)
		elsewhere := gitorigin.New(t, map[string]string{"org/repo.git": gittest.InitRepo(t)}, originUser, originToken)
		r.origin.RedirectTo(elsewhere.URL)
		err := r.prepare(r.url("org/repo.git"))
		if err == nil {
			t.Fatal("Prepare followed a redirect")
		}
		if got := elsewhere.Saw(); len(got) != 0 {
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
		if n := r.origin.AuthorizedFor("/org/other.git"); n != 0 {
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
		if got := r.origin.Saw(); len(got) != 0 {
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
		if n := r.origin.AuthorizedFor("/"); n != 0 {
			t.Errorf("the credential was used after it was revoked: %d", n)
		}
		r.noLeak(err)
	})
}

func TestOriginAuth_ErrorDeFetch(t *testing.T) {
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
	g, err := gitops.New(t.Context(), gitops.Options{Paths: r.paths, Clock: gitorigin.NoWait{}, Log: slog.New(slog.NewTextHandler(r.logs, nil)), GitPath: script})
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

// packs counts the packs of the bare's store.
func packs(t *testing.T, bare string) int {
	t.Helper()
	m, err := filepath.Glob(filepath.Join(bare, "objects", "pack", "*.pack"))
	if err != nil {
		t.Fatal(err)
	}
	return len(m)
}

// An authenticated fetch must not run maintenance against the shared bare:
// with every limit forced to its lowest, it leaves the refs, the worktree
// HEADs and the packs as they were, and nothing outlives it.
func TestOriginAuth_FetchDoesNotMaintainTheSharedBare(t *testing.T) {
	r := newAskpassRig(t, true)
	bare := r.paths.WorkerRepoDir(project)
	{
		init := exec.CommandContext(t.Context(), "git", "init", "--bare", "--quiet", bare) //nolint:gosec // a test
		if out, err := init.CombinedOutput(); err != nil {
			t.Fatalf("git init: %v\n%s", err, out)
		}
	}
	// Make the bare look like it needs gc: more packs than gc.autoPackLimit.
	work := t.TempDir() // unrelated to the origin: the fetch has to run
	gittest.Run(t, work, "init", "--quiet", "--initial-branch=main")
	for i := range 55 { // more than the default gc.autoPackLimit (50)
		gittest.Commit(t, work, "f", fmt.Sprintf("v%d", i), fmt.Sprintf("c%d", i))
		pack := exec.CommandContext(t.Context(), "git", "-C", work, "pack-objects", "--quiet", "--revs", filepath.Join(bare, "objects", "pack", "pack")) //nolint:gosec // a test
		pack.Stdin = strings.NewReader("HEAD\n")
		if out, err := pack.CombinedOutput(); err != nil {
			t.Fatalf("pack-objects: %v\n%s", err, out)
		}
	}
	for i := range 4 {
		gittest.Run(t, bare, "update-ref", "refs/heads/b"+strconv.Itoa(i), gittest.Run(t, work, "rev-parse", "HEAD~"+strconv.Itoa(i)))
	}
	gittest.Run(t, bare, "config", "gc.auto", "6700")
	gittest.Run(t, bare, "update-ref", "refs/cf/run1", "refs/heads/b0")
	other := filepath.Join(t.TempDir(), "other-wt")
	gittest.Run(t, bare, "worktree", "add", "--quiet", "--detach", other, "refs/heads/b1")
	wtHead := func() string {
		return gittest.Run(t, bare, "rev-parse", "refs/heads/b1") + gittest.Run(t, other, "rev-parse", "HEAD")
	}
	headsBefore := wtHead()
	before := gittest.Run(t, bare, "for-each-ref")
	nPacks := packs(t, bare)
	if nPacks <= 50 {
		t.Fatalf("only %d packs: the test would prove nothing", nPacks)
	}
	if err := r.prepare(r.url("org/repo.git")); err != nil {
		t.Fatalf("Prepare: %v\n%s", err, r.logs)
	}
	time.Sleep(2 * time.Second) // a detached maintenance would have run by now
	after := gittest.Run(t, bare, "for-each-ref")
	// Prepare adds its own private source ref; nothing else may differ.
	for _, line := range strings.Split(before, "\n") {
		if !strings.Contains(after, line) {
			t.Errorf("a ref of the bare changed or went away: %q\nafter:\n%s", line, after)
		}
	}
	if got := wtHead(); got != headsBefore {
		t.Errorf("the HEADs changed: %s, was %s", got, headsBefore)
	}
	if got := packs(t, bare); got < nPacks {
		t.Errorf("the packs of the bare shrank from %d to %d: something repacked it", nPacks, got)
	}
}

// dirSize is the bytes of the files under dir.
func dirSize(t *testing.T, dir string) int64 {
	t.Helper()
	var n int64
	err := filepath.WalkDir(dir, func(_ string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		info, err := d.Info()
		if err == nil {
			n += info.Size()
		}
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	return n
}

// With the refs of the bare as its negotiation snapshot, a fetch of a commit
// one change ahead brings the delta and not the history again, and it leaves
// the refs of the bare as they were.
func TestOriginAuth_SecondFetchBringsOnlyTheDelta(t *testing.T) {
	r := newAskpassRig(t, true)
	bare := r.paths.WorkerRepoDir(project)
	blob := make([]byte, 2<<20)
	if _, err := rand.Read(blob); err != nil { // incompressible, so a full fetch is 2 MiB
		t.Fatal(err)
	}
	pushToOrigin := func(file string, content []byte, msg string) string {
		if err := os.WriteFile(filepath.Join(r.src, file), content, 0o600); err != nil {
			t.Fatal(err)
		}
		// Above fetch.unpackLimit (100 objects) git keeps the received pack whole;
		// below it, objects the bare already has are dropped, which would hide the bug.
		for i := range 120 {
			if err := os.WriteFile(filepath.Join(r.src, fmt.Sprintf("n%03d", i)), []byte(fmt.Sprint(i, msg)), 0o600); err != nil {
				t.Fatal(err)
			}
		}
		oid := gittest.Commit(t, r.src, "f", msg, msg)
		gittest.Run(t, r.src, "push", "--quiet", filepath.Join(r.origin.Root, "org", "repo.git"), "main")
		return oid
	}
	r.commit = pushToOrigin("big.bin", blob, "add big")
	if err := r.prepare(r.url("org/repo.git")); err != nil {
		t.Fatalf("Prepare: %v\n%s", err, r.logs)
	}
	first := dirSize(t, filepath.Join(bare, "objects"))
	if first < 2<<20 {
		t.Fatalf("the first fetch stored only %d bytes: the test would prove nothing", first)
	}
	refsBefore := gittest.Run(t, bare, "for-each-ref")

	next := pushToOrigin("small", []byte("x\n"), "one change")
	err := r.g.EnsureSource(t.Context(), gitops.SourceSpec{
		Project: project, Run: run, Repo: r.url("org/repo.git"), SourceRef: "main", SourceCommit: next, Auth: r.auth,
	})
	if err != nil {
		t.Fatalf("EnsureSource: %v\n%s", err, r.logs)
	}
	if _, err := gittest.Try(t, bare, "cat-file", "-e", next+"^{commit}"); err != nil {
		t.Fatalf("the new commit did not arrive: %v", err)
	}
	if grown := dirSize(t, filepath.Join(bare, "objects")) - first; grown > 128<<10 {
		t.Errorf("the second fetch added %d bytes for a one-line change: it downloaded the history again", grown)
	}
	if got := gittest.Run(t, bare, "for-each-ref"); got != refsBefore {
		t.Errorf("the refs of the bare changed:\n%s\nwas:\n%s", got, refsBefore)
	}
}
