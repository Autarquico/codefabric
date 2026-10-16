//go:build integration

package gitops_test

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"io/fs"
	"log/slog"
	"math/big"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/Autarquico/codefabric/internal/config"
	"github.com/Autarquico/codefabric/internal/support/gittest"
	"github.com/Autarquico/codefabric/internal/worker/gitops"
)

const (
	project = "project_01ARZ3NDEKTSV4RRFFQ69G5FAV"
	run     = "run_01ARZ3NDEKTSV4RRFFQ69G5FAW"
	branch  = "cf/auth-race/run-1"
)

type rig struct {
	t        *testing.T
	g        *gitops.Git
	paths    config.Paths
	origin   string
	source   string // the commit a Run starts from
	worktree string
	runDir   string
}

func newRig(t *testing.T) *rig {
	t.Helper()
	gittest.Require(t)
	// The home of the user is empty: nothing of the machine of whoever runs the tests changes them.
	t.Setenv("HOME", t.TempDir())
	t.Setenv("XDG_CONFIG_HOME", "")
	paths := config.Paths{ConfigDir: t.TempDir(), DataDir: t.TempDir()}
	g, err := gitops.New(t.Context(), gitops.Options{Paths: paths, Log: slog.New(slog.DiscardHandler)})
	if err != nil {
		t.Fatal(err)
	}
	origin := gittest.InitRepo(t)
	r := &rig{t: t, g: g, paths: paths, origin: origin, source: gittest.Run(t, origin, "rev-parse", "HEAD")}
	r.worktree = paths.WorkerWorktreeDir(run, 1)
	r.runDir = paths.WorkerRunDir(run, 1)
	return r
}

func (r *rig) spec() gitops.PrepareSpec {
	return gitops.PrepareSpec{
		Project: project, Run: run, LeaseEpoch: 1, Repo: r.origin, SourceRef: "main", SourceCommit: r.source,
		Branch: branch, RunDir: r.runDir,
	}
}

func (r *rig) prepare() {
	r.t.Helper()
	if err := r.g.Prepare(r.t.Context(), r.spec(), r.worktree); err != nil {
		r.t.Fatalf("Prepare: %v", err)
	}
}

func (r *rig) intent(parent string) gitops.Intent {
	return gitops.Intent{
		Project: project, Run: run, LeaseEpoch: 1, ParentOID: parent,
		At: time.Date(2026, 10, 10, 12, 0, 0, 0, time.UTC),
	}
}

func (r *rig) write(name, content string) {
	r.t.Helper()
	path := filepath.Join(r.worktree, name)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		r.t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		r.t.Fatal(err)
	}
}

func (r *rig) bare() string { return r.paths.WorkerRepoDir(project) }

func TestPrepare_MakesAWorktreeAtTheExactSourceCommit(t *testing.T) {
	r := newRig(t)
	r.prepare()
	if got := gittest.Run(t, r.worktree, "rev-parse", "HEAD"); got != r.source {
		t.Errorf("HEAD = %s, want %s", got, r.source)
	}
	if got := gittest.Run(t, r.worktree, "rev-parse", "--abbrev-ref", "HEAD"); got != gitops.LocalBranch(run, 1) {
		t.Errorf("branch = %s", got)
	}
	if got := gittest.Run(t, r.bare(), "rev-parse", "refs/cf/sources/"+run+"-e1"); got != r.source {
		t.Errorf("source pin = %s", got)
	}
	// Again, as after a restart that did not launch the harness: the same.
	r.write("scratch.txt", "x")
	r.prepare()
	if _, err := os.Stat(filepath.Join(r.worktree, "scratch.txt")); err == nil {
		t.Error("a worktree that was not launched was not made again")
	}
}

func TestPrepare_TheOriginMovedOnOrWasRewritten(t *testing.T) {
	r := newRig(t)
	// The branch is ahead of the source commit: the worktree is still at the source commit.
	gittest.Commit(t, r.origin, "later.txt", "later\n", "later")
	r.prepare()
	if got := gittest.Run(t, r.worktree, "rev-parse", "HEAD"); got != r.source {
		t.Errorf("HEAD = %s, want the source commit %s", got, r.source)
	}

	// A commit that origin no longer reaches: the Run cannot be prepared, and says why.
	lost := gittest.Commit(t, r.origin, "gone.txt", "gone\n", "to be rewritten")
	gittest.Run(t, r.origin, "reset", "--quiet", "--hard", r.source)
	gittest.Run(t, r.origin, "reflog", "expire", "--expire=now", "--all")
	gittest.Run(t, r.origin, "gc", "--quiet", "--prune=now")
	s := r.spec()
	s.Run, s.SourceCommit = "run_01ARZ3NDEKTSV4RRFFQ69G5FAX", lost
	wt := r.paths.WorkerWorktreeDir(s.Run, 1)
	err := r.g.Prepare(t.Context(), s, wt)
	if err == nil || !strings.Contains(err.Error(), "not available") {
		t.Fatalf("Prepare of a lost commit: %v", err)
	}
	if _, statErr := os.Stat(wt); statErr == nil {
		t.Error("a failed Prepare left a worktree")
	}
	// A commit that never existed.
	s.SourceCommit = strings.Repeat("a", 40)
	if err := r.g.Prepare(t.Context(), s, wt); err == nil {
		t.Error("an invented commit was prepared")
	}
}

func TestPrepare_NeverTouchesAWorktreeWhereTheHarnessRan(t *testing.T) {
	r := newRig(t)
	r.prepare()
	r.write("evidence.txt", "what the agent did")
	if err := os.MkdirAll(r.runDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(r.runDir, "wrapper.json"), []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := r.g.Prepare(t.Context(), r.spec(), r.worktree); err == nil {
		t.Fatal("Prepare went on over a worktree of a launched harness")
	}
	if _, err := os.Stat(filepath.Join(r.worktree, "evidence.txt")); err != nil {
		t.Errorf("the evidence is gone: %v", err)
	}
}

func TestPrepare_RefusesWhatItShouldNotRunGitWith(t *testing.T) {
	r := newRig(t)
	for name, mutate := range map[string]func(*gitops.PrepareSpec){
		"a project that is a path": func(s *gitops.PrepareSpec) { s.Project = "../x" },
		"a task as the project":    func(s *gitops.PrepareSpec) { s.Project = "task_01ARZ3NDEKTSV4RRFFQ69G5FAV" },
		"a bad commit":             func(s *gitops.PrepareSpec) { s.SourceCommit = "--upload-pack=x" },
		"a bad branch":             func(s *gitops.PrepareSpec) { s.Branch = "main" },
		"an option as the ref":     func(s *gitops.PrepareSpec) { s.SourceRef = "--upload-pack=touch /tmp/x" },
		"a refspec as the ref":     func(s *gitops.PrepareSpec) { s.SourceRef = "+main:refs/heads/other" },
		"a revision as the ref":    func(s *gitops.PrepareSpec) { s.SourceRef = "main~1" },
		"an option as the repo":    func(s *gitops.PrepareSpec) { s.Repo = "--upload-pack=x" },
		"no repo":                  func(s *gitops.PrepareSpec) { s.Repo = "" },
	} {
		s := r.spec()
		mutate(&s)
		if err := r.g.Prepare(t.Context(), s, r.worktree); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

// sealedTree lists the files of a commit.
func (r *rig) sealedTree(commit string) []string {
	r.t.Helper()
	out := gittest.Run(r.t, r.bare(), "ls-tree", "-r", "--name-only", commit)
	return strings.Split(out, "\n")
}

func TestSeal_CapturesTheWorktreeAndLeavesItAlone(t *testing.T) {
	r := newRig(t)
	r.prepare()
	r.write(".gitignore", "ignored.log\n")
	gittest.Run(t, r.worktree, "add", ".gitignore")
	gittest.Run(t, r.worktree, "commit", "--quiet", "-m", "ignore")
	parent := gittest.Run(t, r.worktree, "rev-parse", "HEAD")

	r.write("README", "changed\n")              // tracked, modified
	r.write("new/untracked.txt", "new\n")       // untracked
	r.write("ignored.log", "noise\n")           // ignored
	r.write(".env", "SECRET=1\n")               // excluded
	r.write("x.pem", "pem\n")                   // excluded
	r.write("deep/er/.env.local", "SECRET=2\n") // excluded, nested
	r.write("staged.txt", "staged\n")           // staged by the agent
	gittest.Run(t, r.worktree, "add", "staged.txt")
	untrackedBefore := gittest.Run(t, r.worktree, "ls-files", "--others", "--ignored", "--exclude-standard")
	indexBefore := gittest.Run(t, r.worktree, "ls-files", "--stage")

	in := r.intent(parent)
	got, err := r.g.Seal(t.Context(), r.worktree, in, gitops.Forbidden{})
	if err != nil {
		t.Fatalf("Seal: %v", err)
	}
	files := r.sealedTree(got.Commit)
	for _, want := range []string{"README", "new/untracked.txt", "staged.txt", ".gitignore"} {
		if !contains(files, want) {
			t.Errorf("%s is not in the commit: %v", want, files)
		}
	}
	for _, not := range []string{"ignored.log", ".env", "x.pem", "deep/er/.env.local"} {
		if contains(files, not) {
			t.Errorf("%s is in the commit", not)
		}
	}
	if want := []string{".env", "deep/er/.env.local", "x.pem"}; strings.Join(got.Excluded, ",") != strings.Join(want, ",") || got.Truncated {
		t.Errorf("excluded = %v (truncated %v), want %v", got.Excluded, got.Truncated, want)
	}
	if gittest.Run(t, r.bare(), "rev-parse", got.Commit+"^") != parent {
		t.Error("the commit is not on top of the parent")
	}
	if head := gittest.Run(t, r.worktree, "rev-parse", "HEAD"); head != got.Commit {
		t.Errorf("HEAD of the worktree = %s, want the result %s", head, got.Commit)
	}
	if tip := gittest.Run(t, r.bare(), "rev-parse", "refs/heads/"+gitops.LocalBranch(run, 1)); tip != got.Commit {
		t.Errorf("branch = %s", tip)
	}
	if ref := gittest.Run(t, r.bare(), "rev-parse", gitops.PrivateRef(run, 1)); ref != got.Commit {
		t.Errorf("private ref = %s", ref)
	}
	// The user's files and index are as they were: the sealing used an index of its own.
	if after := gittest.Run(t, r.worktree, "ls-files", "--others", "--ignored", "--exclude-standard"); after != untrackedBefore {
		t.Errorf("the files of the worktree changed:\n%s\n--\n%s", untrackedBefore, after)
	}
	if after := gittest.Run(t, r.worktree, "ls-files", "--stage"); after != indexBefore {
		t.Errorf("the index changed:\n%s\n--\n%s", indexBefore, after)
	}
	if b, _ := os.ReadFile(filepath.Join(r.worktree, "README")); string(b) != "changed\n" {
		t.Errorf("README = %q", b)
	}
	if b, err := os.ReadFile(filepath.Join(r.worktree, ".env")); err != nil || string(b) != "SECRET=1\n" {
		t.Errorf(".env = %q (%v)", b, err)
	}

	// Sealing again is the same commit: from the private ref, and without it (the date is the intent's).
	again, err := r.g.Seal(t.Context(), r.worktree, in, gitops.Forbidden{})
	if err != nil || again.Commit != got.Commit {
		t.Errorf("second seal = %s (%v), want %s", again.Commit, err, got.Commit)
	}
	gittest.Run(t, r.bare(), "update-ref", "-d", gitops.PrivateRef(run, 1))
	rebuilt, err := r.g.Seal(t.Context(), r.worktree, in, gitops.Forbidden{})
	if err != nil || rebuilt.Commit != got.Commit {
		t.Errorf("rebuilt seal = %s (%v), want %s", rebuilt.Commit, err, got.Commit)
	}
}

func contains(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}

func TestSeal_NothingToCaptureIsTheParent(t *testing.T) {
	r := newRig(t)
	r.prepare()
	r.write("secret.pem", "x") // matches an exclusion, untracked: nothing captured
	got, err := r.g.Seal(t.Context(), r.worktree, r.intent(r.source), gitops.Forbidden{})
	if err != nil || got.Commit != r.source {
		t.Fatalf("seal = %+v (%v), want the parent %s", got, err, r.source)
	}
	if len(got.Excluded) != 0 {
		t.Logf("excluded: %v", got.Excluded)
	}
	if tip := gittest.Run(t, r.bare(), "rev-parse", "refs/heads/"+gitops.LocalBranch(run, 1)); tip != r.source {
		t.Errorf("branch = %s", tip)
	}
}

func TestSeal_ATrackedFileThatMatchesAnExclusionKeepsItsVersionInHEAD(t *testing.T) {
	r := newRig(t)
	r.prepare()
	r.write("cert.pem", "v1\n")
	gittest.Run(t, r.worktree, "add", "-f", "cert.pem")
	gittest.Run(t, r.worktree, "commit", "--quiet", "-m", "track a pem")
	parent := gittest.Run(t, r.worktree, "rev-parse", "HEAD")
	r.write("cert.pem", "v2 changed by the agent\n")
	r.write("code.txt", "real work\n")
	got, err := r.g.Seal(t.Context(), r.worktree, r.intent(parent), gitops.Forbidden{})
	if err != nil {
		t.Fatal(err)
	}
	if content := gittest.Run(t, r.bare(), "show", got.Commit+":cert.pem"); content != "v1" {
		t.Errorf("cert.pem in the commit = %q, want the version of HEAD", content)
	}
	if !contains(r.sealedTree(got.Commit), "code.txt") {
		t.Error("the real work is not in the commit")
	}
	if b, _ := os.ReadFile(filepath.Join(r.worktree, "cert.pem")); string(b) != "v2 changed by the agent\n" {
		t.Error("the file of the user was touched")
	}
}

func TestSeal_AHeadThatLeftTheBranchIsSealedOnItsOwnParent(t *testing.T) {
	r := newRig(t)
	r.prepare()
	gittest.Run(t, r.worktree, "checkout", "--quiet", "--detach")
	r.write("work.txt", "w\n")
	gittest.Run(t, r.worktree, "add", "work.txt")
	gittest.Run(t, r.worktree, "commit", "--quiet", "-m", "detached work")
	parent := gittest.Run(t, r.worktree, "rev-parse", "HEAD")
	r.write("more.txt", "m\n")
	got, err := r.g.Seal(t.Context(), r.worktree, r.intent(parent), gitops.Forbidden{})
	if err != nil || gittest.Run(t, r.bare(), "rev-parse", got.Commit+"^") != parent {
		t.Fatalf("seal = %+v (%v)", got, err)
	}
	if tip := gittest.Run(t, r.bare(), "rev-parse", "refs/heads/"+gitops.LocalBranch(run, 1)); tip != got.Commit {
		t.Errorf("the branch of the Run is at %s, not at the result", tip)
	}
}

func TestScenario_GitRelayPublication_SelladoFallido_Git(t *testing.T) {
	if os.Getuid() == 0 {
		t.Skip("root writes where it is told not to")
	}
	r := newRig(t)
	r.prepare()
	r.write("work.txt", "w\n")
	statusBefore := gittest.Run(t, r.worktree, "status", "--porcelain=v1")
	branchBefore := gittest.Run(t, r.bare(), "rev-parse", "refs/heads/"+gitops.LocalBranch(run, 1))

	// The disk takes no more objects.
	objects := filepath.Join(r.bare(), "objects")
	var dirs []string
	_ = filepath.WalkDir(objects, func(path string, d fs.DirEntry, err error) error {
		if err == nil && d.IsDir() {
			dirs = append(dirs, path)
		}
		return nil
	})
	for _, d := range dirs {
		_ = os.Chmod(d, 0o500) //nolint:gosec // making a directory read-only for the test
	}
	t.Cleanup(func() {
		for _, d := range dirs {
			_ = os.Chmod(d, 0o700) //nolint:gosec // so the temporary directory can be removed
		}
	})
	_, err := r.g.Seal(t.Context(), r.worktree, r.intent(r.source), gitops.Forbidden{})
	if err == nil {
		t.Fatal("sealing succeeded on a repository that takes no objects")
	}
	if after := gittest.Run(t, r.worktree, "status", "--porcelain=v1"); after != statusBefore {
		t.Errorf("the worktree changed: %s", after)
	}
	if b, _ := os.ReadFile(filepath.Join(r.worktree, "work.txt")); string(b) != "w\n" {
		t.Error("the file of the user changed")
	}
	if tip := gittest.Run(t, r.bare(), "rev-parse", "refs/heads/"+gitops.LocalBranch(run, 1)); tip != branchBefore {
		t.Errorf("the branch moved to %s", tip)
	}
	if _, perr := gittest.Try(t, r.bare(), "rev-parse", "--verify", "--quiet", gitops.PrivateRef(run, 1)); perr == nil {
		t.Error("a private ref was made")
	}
}

func TestSeal_NoRepositoryIsAnError(t *testing.T) {
	r := newRig(t)
	if err := os.MkdirAll(r.worktree, 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := r.g.Head(t.Context(), r.worktree); err == nil {
		t.Error("an empty directory has a HEAD")
	}
	if _, err := r.g.Seal(t.Context(), r.worktree, r.intent(""), gitops.Forbidden{}); err == nil {
		t.Error("an empty directory was sealed")
	}
}

// --- Push ---

// relayStub is a TLS server that answers every request with a status and
// records the headers it saw.
type relayStub struct {
	srv     *httptest.Server
	mu      sync.Mutex
	headers []http.Header
	status  int
	ca      string
	cert    string
	key     string
}

func newStub(t *testing.T, status int) *relayStub {
	t.Helper()
	s := &relayStub{status: status}
	s.srv = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		s.mu.Lock()
		s.headers = append(s.headers, req.Header.Clone())
		code := s.status
		s.mu.Unlock()
		if code == 0 { // hang up
			hj, _ := w.(http.Hijacker)
			conn, _, _ := hj.Hijack()
			_ = conn.Close()
			return
		}
		http.Error(w, "x", code)
	}))
	t.Cleanup(s.srv.Close)
	dir := t.TempDir()
	s.ca = writePEM(t, filepath.Join(dir, "ca.pem"), "CERTIFICATE", s.srv.Certificate().Raw)
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	der, err := x509.CreateCertificate(rand.Reader, &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "w"}, NotAfter: time.Now().Add(time.Hour)}, &x509.Certificate{}, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	keyDER, _ := x509.MarshalECPrivateKey(key)
	s.cert = writePEM(t, filepath.Join(dir, "cert.pem"), "CERTIFICATE", der)
	s.key = writePEM(t, filepath.Join(dir, "key.pem"), "EC PRIVATE KEY", keyDER)
	return s
}

func writePEM(t *testing.T, path, typ string, der []byte) string {
	t.Helper()
	if err := os.WriteFile(path, pem.EncodeToMemory(&pem.Block{Type: typ, Bytes: der}), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestPush_ClassifiesWhatTheRelayDoesAndNeverLeaksTheToken(t *testing.T) {
	r := newRig(t)
	r.prepare()
	const token = "tok-0123456789abcdef-secret"
	// A local hook of the repository that tries to read the token must not run.
	hookOut := filepath.Join(t.TempDir(), "leak")
	hooks := filepath.Join(r.bare(), "hooks")
	_ = os.MkdirAll(hooks, 0o700)
	script := "#!/bin/sh\nenv > '" + hookOut + "'\n"
	if err := os.WriteFile(filepath.Join(hooks, "pre-push"), []byte(script), 0o700); err != nil { //nolint:gosec // a test hook
		t.Fatal(err)
	}

	for name, tc := range map[string]struct {
		status int
		want   gitops.PushClass
	}{
		"hang up":    {0, gitops.PushUnreachable},
		"overloaded": {http.StatusServiceUnavailable, gitops.PushUnreachable},
		"busy":       {http.StatusTooManyRequests, gitops.PushUnreachable},
		"forbidden":  {http.StatusForbidden, gitops.PushForbidden},
		"unauth":     {http.StatusUnauthorized, gitops.PushPermanent},
	} {
		stub := newStub(t, tc.status)
		err := r.g.Push(t.Context(), project, r.source, branch, gitops.PushAuth{
			URL: stub.srv.URL + "/v1/relay/" + project + ".git", Token: token, Cert: stub.cert, Key: stub.key, CA: stub.ca,
		}, gitops.Forbidden{})
		pe, ok := err.(*gitops.PushError) //nolint:errorlint // the concrete type is what is under test
		if !ok || pe.Class != tc.want {
			t.Errorf("%s: %v, want class %d", name, err, tc.want)
			continue
		}
		if strings.Contains(pe.Message, token) {
			t.Errorf("%s: the token is in the error", name)
		}
		stub.mu.Lock()
		sent := len(stub.headers) > 0 && stub.headers[0].Get("Authorization") == "Bearer "+token
		stub.mu.Unlock()
		if tc.status != 0 && !sent {
			t.Errorf("%s: the relay did not get the token in the header", name)
		}
	}
	if _, err := os.Stat(hookOut); err == nil {
		t.Error("a hook of the repository ran and could read the environment")
	}
	cfg, _ := os.ReadFile(filepath.Join(r.bare(), "config"))
	if strings.Contains(string(cfg), token) {
		t.Error("the token is in the configuration of the repository")
	}

	// A repository whose own configuration tries to turn verification off, to use a proxy or to run a credential helper.
	helperRan := filepath.Join(t.TempDir(), "helper-ran")
	hostile := "[http]\n\tsslVerify = false\n\tproxy = http://127.0.0.1:1\n[credential]\n\thelper = !sh -c 'echo ran > " + helperRan + "'\n"
	f, err := os.OpenFile(filepath.Join(r.bare(), "config"), os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = f.WriteString(hostile)
	_ = f.Close()
	untrusted := newStub(t, http.StatusUnauthorized)
	// ... and the same keys for the exact URL of the relay, which are as specific as ours.
	exact := "[http \"" + untrusted.srv.URL + "/p.git\"]\n\tsslVerify = false\n\tproxy = http://127.0.0.1:1\n\tsslCAInfo = /nonexistent/ca.pem\n"
	cfgFile, err := os.OpenFile(filepath.Join(r.bare(), "config"), os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = cfgFile.WriteString(exact)
	_ = cfgFile.Close()
	err = r.g.Push(t.Context(), project, r.source, branch, gitops.PushAuth{
		URL: untrusted.srv.URL + "/p.git", Token: token, Cert: untrusted.cert, Key: untrusted.key, CA: untrusted.cert,
	}, gitops.Forbidden{})
	if pe, ok := err.(*gitops.PushError); !ok || pe.Class != gitops.PushPermanent { //nolint:errorlint // the concrete type is under test
		t.Errorf("verification was turned off by the repository: %v", err)
	}
	// A refused certificate and a 401 are both permanent: what tells them apart is
	// that a verified TLS handshake never lets the request reach the server.
	untrusted.mu.Lock()
	reached := len(untrusted.headers)
	untrusted.mu.Unlock()
	if reached != 0 {
		t.Error("TLS verification was turned off by the repository: the request reached the server")
	}
	trusted := newStub(t, http.StatusUnauthorized)
	_ = r.g.Push(t.Context(), project, r.source, branch, gitops.PushAuth{
		URL: trusted.srv.URL + "/p.git", Token: token, Cert: trusted.cert, Key: trusted.key, CA: trusted.ca,
	}, gitops.Forbidden{})
	if _, err := os.Stat(helperRan); err == nil {
		t.Error("a credential helper of the repository ran")
	}
	trusted.mu.Lock()
	sawRequest := len(trusted.headers) > 0
	trusted.mu.Unlock()
	if !sawRequest {
		t.Error("the proxy of the repository was used")
	}

	// A certificate the Worker does not trust is permanent.
	stub := newStub(t, http.StatusOK)
	other := stub // a CA that did not sign the certificate of the server
	err = r.g.Push(t.Context(), project, r.source, branch, gitops.PushAuth{
		URL: stub.srv.URL + "/p.git", Token: token, Cert: stub.cert, Key: stub.key, CA: other.cert,
	}, gitops.Forbidden{})
	if pe, ok := err.(*gitops.PushError); !ok || pe.Class != gitops.PushPermanent { //nolint:errorlint // the concrete type is under test
		t.Errorf("an untrusted certificate: %v", err)
	}
	// Invalid input never reaches git.
	if err := r.g.Push(t.Context(), project, "--force", branch, gitops.PushAuth{URL: "https://x", Token: token, Cert: "c", Key: "k", CA: "a"}, gitops.Forbidden{}); err == nil {
		t.Error("a bad commit was pushed")
	}
}

func TestSeal_APrivateRefOfAnotherIntentIsNotTheResult(t *testing.T) {
	r := newRig(t)
	r.prepare()
	r.write("work.txt", "w\n")
	// A stale ref, whose commit is neither the parent nor on top of it.
	stray := gittest.Run(t, r.origin, "commit-tree", "-m", "stray", gittest.Run(t, r.origin, "rev-parse", "HEAD^{tree}"))
	gittest.Run(t, r.bare(), "fetch", "--quiet", r.origin, stray)
	gittest.Run(t, r.bare(), "update-ref", gitops.PrivateRef(run, 1), stray)
	got, err := r.g.Seal(t.Context(), r.worktree, r.intent(r.source), gitops.Forbidden{})
	if err != nil {
		t.Fatal(err)
	}
	if got.Commit == stray || gittest.Run(t, r.bare(), "rev-parse", got.Commit+"^") != r.source {
		t.Errorf("the result is %s: a private ref that is not of this intent was reused", got.Commit)
	}
	if ref := gittest.Run(t, r.bare(), "rev-parse", gitops.PrivateRef(run, 1)); ref != got.Commit {
		t.Errorf("private ref = %s", ref)
	}
}

func TestSeal_HonoursTheGlobalIgnoreFileOfTheUserAndNeverSigns(t *testing.T) {
	gittest.Require(t)
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", "")
	ignore := filepath.Join(home, "global-ignore")
	if err := os.WriteFile(ignore, []byte("*.scratch\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg := "[core]\n\texcludesFile = ~/global-ignore\n[commit]\n\tgpgSign = true\n"
	if err := os.WriteFile(filepath.Join(home, ".gitconfig"), []byte(cfg), 0o600); err != nil {
		t.Fatal(err)
	}
	paths := config.Paths{ConfigDir: t.TempDir(), DataDir: t.TempDir()}
	g, err := gitops.New(t.Context(), gitops.Options{Paths: paths, Log: slog.New(slog.DiscardHandler)})
	if err != nil {
		t.Fatal(err)
	}
	r := &rig{t: t, g: g, paths: paths}
	r.origin = gittest.InitRepo(t)
	r.source = gittest.Run(t, r.origin, "rev-parse", "HEAD")
	r.worktree, r.runDir = paths.WorkerWorktreeDir(run, 1), paths.WorkerRunDir(run, 1)
	r.prepare()
	r.write("notes.scratch", "ignored only globally\n")
	r.write("real.txt", "real\n")
	got, err := g.Seal(t.Context(), r.worktree, r.intent(r.source), gitops.Forbidden{})
	if err != nil {
		t.Fatalf("Seal: %v", err)
	}
	files := r.sealedTree(got.Commit)
	if contains(files, "notes.scratch") || !contains(files, "real.txt") {
		t.Errorf("files = %v", files)
	}
}

func TestPush_ADeadlineIsUnreachableSpendsNoAttemptAndKillsTheGroup(t *testing.T) {
	gittest.Require(t)
	t.Setenv("HOME", t.TempDir())
	realGit, err := exec.LookPath("git")
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	pgids := filepath.Join(dir, "pgids")
	script := filepath.Join(dir, "git")
	body := "#!/bin/sh\ncase \"$1\" in\npush)\n  ps -o pgid= -p $$ | tr -d ' ' >> '" + pgids + "'\n  sleep 300 &\n  wait\n  ;;\n*) exec '" + realGit + "' \"$@\" ;;\nesac\n"
	if err := os.WriteFile(script, []byte(body), 0o700); err != nil { //nolint:gosec // a test script
		t.Fatal(err)
	}
	paths := config.Paths{ConfigDir: t.TempDir(), DataDir: t.TempDir()}
	g, err := gitops.New(t.Context(), gitops.Options{Paths: paths, Log: slog.New(slog.DiscardHandler), GitPath: script, PushTimeout: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(paths.WorkerRepoDir(project), 0o700); err != nil {
		t.Fatal(err)
	}
	gittest.Run(t, paths.WorkerRepoDir(project), "init", "--bare", "--quiet")
	stub := newStub(t, http.StatusOK)
	start := time.Now()
	err = g.Push(t.Context(), project, strings.Repeat("a", 40), branch, gitops.PushAuth{
		URL: stub.srv.URL + "/p.git", Token: "tok-0123456789abcdef", Cert: stub.cert, Key: stub.key, CA: stub.ca,
	}, gitops.Forbidden{})
	pe, ok := err.(*gitops.PushError) //nolint:errorlint // the concrete type is under test
	if !ok || pe.Class != gitops.PushUnreachable || pe.Counts() {
		t.Fatalf("a push that hit its deadline: %v", err)
	}
	if took := time.Since(start); took > 10*time.Second {
		t.Errorf("the deadline of 1s took %s", took)
	}
	raw, err := os.ReadFile(pgids) //nolint:gosec // a path of this test
	if err != nil {
		t.Fatal(err)
	}
	pgid, err := strconv.Atoi(strings.TrimSpace(string(raw)))
	if err != nil || pgid <= 1 {
		t.Fatalf("pgid %q", raw)
	}
	waitFor(t, "the process group of the push to be gone", func() bool { return errors.Is(syscall.Kill(-pgid, 0), syscall.ESRCH) })
}

func waitFor(t *testing.T, what string, ok func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for !ok() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(20 * time.Millisecond)
	}
}
