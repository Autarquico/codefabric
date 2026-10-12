//go:build integration

package relay

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Autarquico/codefabric/internal/domain"
	"github.com/Autarquico/codefabric/internal/support/gittest"
)

// rig is a relay served over plain HTTP to a real git, with the grant of every
// request chosen by the test. The API's own authorisation is not here: it has
// its own tests; what this proves is the transport and the hook.
type rig struct {
	t       *testing.T
	r       *Relay
	srv     *httptest.Server
	project domain.ID
	grant   atomic.Pointer[func(ep Endpoint) Grant]
}

func newRig(t *testing.T, tweak func(*Config)) *rig {
	t.Helper()
	gittest.Require(t)
	cfg := Config{
		Dir: t.TempDir(), Log: discard(),
		HookArgv: []string{"env", asCF + "=1", mustExecutable(t), "relay", "hook", "pre-receive"},
	}
	if tweak != nil {
		tweak(&cfg)
	}
	r, err := Open(t.Context(), cfg)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	g := &rig{t: t, r: r, project: domain.NewIDGenerator(time.Now, rand.Reader).New(domain.KindProject)}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /{repo}/info/refs", func(w http.ResponseWriter, req *http.Request) {
		ep := InfoRefsUpload
		if req.URL.Query().Get("service") == "git-receive-pack" {
			ep = InfoRefsReceive
		}
		g.serve(w, req, ep)
	})
	mux.HandleFunc("POST /{repo}/git-upload-pack", func(w http.ResponseWriter, req *http.Request) { g.serve(w, req, UploadPack) })
	mux.HandleFunc("POST /{repo}/git-receive-pack", func(w http.ResponseWriter, req *http.Request) { g.serve(w, req, ReceivePack) })
	g.srv = httptest.NewServer(mux)
	t.Cleanup(g.srv.Close)
	return g
}

func (g *rig) serve(w http.ResponseWriter, req *http.Request, ep Endpoint) {
	grant := (*g.grant.Load())(ep)
	req.Header.Del("Authorization")
	err := g.r.Serve(w, req, ep, grant)
	switch {
	case err == nil:
	case errors.Is(err, ErrBusy):
		http.Error(w, "busy", http.StatusTooManyRequests)
	case errors.Is(err, ErrTooLarge):
		http.Error(w, "too large", http.StatusRequestEntityTooLarge)
	case errors.Is(err, ErrBadBody):
		http.Error(w, "bad body", http.StatusBadRequest)
	default:
		http.Error(w, "internal", http.StatusInternalServerError)
	}
}

func (g *rig) url() string { return g.srv.URL + "/" + string(g.project) + ".git" }

// allow authorises pushes of oid to branch, and reads, by one Worker.
func (g *rig) allow(branch, oid string) {
	g.setGrant(func(Endpoint) Grant {
		return Grant{Project: g.project, Worker: "worker_1", Write: true, Branch: branch, ExpectedOID: oid}
	})
}

func (g *rig) setGrant(f func(Endpoint) Grant) { g.grant.Store(&f) }

func (g *rig) tip(branch string) (string, bool) {
	g.t.Helper()
	oid, found, err := g.r.BranchTip(g.t.Context(), g.project, branch)
	if err != nil {
		g.t.Fatalf("BranchTip: %v", err)
	}
	return oid, found
}

const branch1 = "cf/auth-race/run-1"

func mustExecutable(t *testing.T) string {
	t.Helper()
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	return self
}

func discard() *slog.Logger { return slog.New(slog.DiscardHandler) }

func TestRelay_PushesTheGrantedCommitAndServesIt(t *testing.T) {
	g := newRig(t, nil)
	work := gittest.InitRepo(t)
	head := gittest.Run(t, work, "rev-parse", "HEAD")
	g.allow(branch1, head)

	if out, err := gittest.Try(t, work, "push", g.url(), "HEAD:refs/heads/"+branch1); err != nil {
		t.Fatalf("push: %v\n%s", err, out)
	}
	if oid, found := g.tip(branch1); !found || oid != head {
		t.Fatalf("tip = %q (%v), want %s", oid, found, head)
	}
	clone := t.TempDir()
	if out, err := gittest.Try(t, clone, "clone", "--quiet", "--branch", branch1, g.url(), "out"); err != nil {
		t.Fatalf("clone: %v\n%s", err, out)
	}
	if got := gittest.Run(t, filepath.Join(clone, "out"), "rev-parse", "HEAD"); got != head {
		t.Errorf("clone is at %s, want %s", got, head)
	}
	if left := gittest.Descendants(t); len(left) != 0 {
		t.Errorf("processes left behind: %v", left)
	}
}

func TestRelay_RefusesWhatTheGrantDoesNotAllow(t *testing.T) {
	g := newRig(t, nil)
	work := gittest.InitRepo(t)
	first := gittest.Run(t, work, "rev-parse", "HEAD")
	second := gittest.Commit(t, work, "b.txt", "b\n", "second")
	gittest.Run(t, work, "tag", "-a", "-m", "annotated", "v1")
	tagObject := gittest.Run(t, work, "rev-parse", "v1")
	gittest.Run(t, work, "checkout", "--quiet", "-b", "diverged", first)
	diverged := gittest.Commit(t, work, "c.txt", "c\n", "diverged from second")

	// The branch of the Run is at second; what follows tries to move it, or others.
	g.allow(branch1, second)
	if out, err := gittest.Try(t, work, "push", g.url(), second+":refs/heads/"+branch1); err != nil {
		t.Fatalf("the granted push: %v\n%s", err, out)
	}

	for name, tc := range map[string]struct {
		expected string // the commit the grant allows
		refspec  []string
		branch   string // a branch whose tip is checked afterwards
		want     string // "" if it must not exist
	}{
		"main":                  {first, []string{first + ":refs/heads/main"}, "main", ""},
		"the branch of another": {first, []string{first + ":refs/heads/cf/other/run-2"}, "cf/other/run-2", ""},
		"a tag":                 {first, []string{"v1:refs/tags/v1"}, "", ""},
		"the private refs":      {first, []string{first + ":refs/cf/results/x"}, "", ""},
		"a deletion":            {second, []string{":refs/heads/" + branch1}, branch1, second},
		"a force-push":          {diverged, []string{"+" + diverged + ":refs/heads/" + branch1}, branch1, second},
		"another commit":        {first, []string{diverged + ":refs/heads/" + branch1}, branch1, second},
		"several refs":          {second, []string{second + ":refs/heads/" + branch1, second + ":refs/heads/cf/x/run-9"}, "cf/x/run-9", ""},
		"a tag object":          {tagObject, []string{tagObject + ":refs/heads/" + branch1}, branch1, second},
	} {
		t.Run(name, func(t *testing.T) {
			g.allow(branch1, tc.expected)
			args := append([]string{"push", g.url()}, tc.refspec...)
			out, err := gittest.Try(t, work, args...)
			if err == nil {
				t.Fatalf("the push was accepted:\n%s", out)
			}
			if tc.branch != "" {
				oid, err := gittest.Try(t, g.r.RepoDir(g.project), "rev-parse", "--verify", "--quiet", "refs/heads/"+tc.branch)
				if (err == nil) != (tc.want != "") || (err == nil && oid != tc.want) {
					t.Errorf("%s is at %q (%v), want %q", tc.branch, oid, err, tc.want)
				}
			}
		})
	}
}

func TestRelay_AcceptsAFastForwardOfTheSameBranch(t *testing.T) {
	g := newRig(t, nil)
	work := gittest.InitRepo(t)
	first := gittest.Run(t, work, "rev-parse", "HEAD")
	g.allow(branch1, first)
	gittest.Run(t, work, "push", g.url(), "HEAD:refs/heads/"+branch1)
	second := gittest.Commit(t, work, "b.txt", "b\n", "second")
	g.allow(branch1, second)
	gittest.Run(t, work, "push", g.url(), "HEAD:refs/heads/"+branch1)
	if oid, _ := g.tip(branch1); oid != second {
		t.Errorf("tip = %s, want %s", oid, second)
	}
}

// incompressible writes a file of random bytes: its pack is as big as it is.
func incompressible(t *testing.T, dir, name string, size int) {
	t.Helper()
	buf := make([]byte, size)
	if _, err := rand.Read(buf); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, name), buf, 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestRelay_ChunkedPushOfMoreThanOneMiB(t *testing.T) {
	g := newRig(t, nil)
	work := gittest.InitRepo(t)
	incompressible(t, work, "big.bin", 3<<20)
	gittest.Run(t, work, "add", "-A")
	gittest.Run(t, work, "commit", "--quiet", "-m", "big")
	head := gittest.Run(t, work, "rev-parse", "HEAD")
	g.allow(branch1, head)

	// A pack above http.postBuffer (1 MiB) is sent with chunked transfer encoding.
	if out, err := gittest.Try(t, work, "-c", "http.postBuffer=1048576", "push", g.url(), "HEAD:refs/heads/"+branch1); err != nil {
		t.Fatalf("push: %v\n%s", err, out)
	}
	if oid, found := g.tip(branch1); !found || oid != head {
		t.Errorf("tip = %q (%v)", oid, found)
	}
}

func TestRelay_PushAboveTheLimitIsRefusedAndLeavesNothing(t *testing.T) {
	g := newRig(t, func(c *Config) { c.MaxPushBytes = 1 << 20 })
	work := gittest.InitRepo(t)
	incompressible(t, work, "big.bin", 3<<20)
	gittest.Run(t, work, "add", "-A")
	gittest.Run(t, work, "commit", "--quiet", "-m", "big")
	g.allow(branch1, gittest.Run(t, work, "rev-parse", "HEAD"))

	if out, err := gittest.Try(t, work, "push", g.url(), "HEAD:refs/heads/"+branch1); err == nil {
		t.Fatalf("a push above the limit was accepted:\n%s", out)
	}
	if _, found := g.tip(branch1); found {
		t.Error("the branch exists")
	}
	if left := gittest.Descendants(t); len(left) != 0 {
		t.Errorf("processes left behind: %v", left)
	}
	if entries, _ := os.ReadDir(g.r.tmpDir()); len(entries) != 0 {
		t.Errorf("the scratch space keeps %d entries", len(entries))
	}
}

func TestRelay_AClientThatHangsUpMidBodyFreesEverything(t *testing.T) {
	g := newRig(t, nil)
	g.allow(branch1, strings.Repeat("a", 40))
	conn, err := (&net.Dialer{}).DialContext(t.Context(), "tcp", g.srv.Listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	fmt.Fprintf(conn, "POST /%s.git/git-receive-pack HTTP/1.1\r\nHost: x\r\nContent-Type: application/x-git-receive-pack-request\r\nContent-Length: %d\r\n\r\n", g.project, 10<<20)
	_, _ = conn.Write(make([]byte, 1<<20))
	_ = conn.Close()

	eventually(t, "the relay to free its slot", func() bool {
		g.r.mu.Lock()
		defer g.r.mu.Unlock()
		return len(g.r.pushing) == 0 && len(g.r.slots) == 0
	})
	if left := gittest.Descendants(t); len(left) != 0 {
		t.Errorf("processes left behind: %v", left)
	}
	if entries, _ := os.ReadDir(g.r.tmpDir()); len(entries) != 0 {
		t.Errorf("the scratch space keeps %d entries", len(entries))
	}
}

func eventually(t *testing.T, what string, ok func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for !ok() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// slowGit stands for git where http-backend answers its headers and then
// takes its time, leaving a child behind that only the group kill can reach.
// Anything else it hands to the real git. It dumps the environment of the
// backend to a file.
func slowGit(t *testing.T) (script, dump string) {
	t.Helper()
	realGit, err := exec.LookPath("git")
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	dump = filepath.Join(dir, "env.txt")
	script = filepath.Join(dir, "git")
	body := fmt.Sprintf("#!/bin/sh\ncase \"$1\" in\nhttp-backend)\n  env > '%s'\n  printf 'Status: 200 OK\\r\\nContent-Type: text/plain\\r\\n\\r\\nhello\\n'\n  sleep 300 &\n  wait\n  ;;\n*) exec '%s' \"$@\" ;;\nesac\n", dump, realGit)
	if err := os.WriteFile(script, []byte(body), 0o700); err != nil { //nolint:gosec // a test script
		t.Fatal(err)
	}
	return script, dump
}

func TestRelay_KillsTheWholeGroupWhenTheClientGoes(t *testing.T) {
	script, dump := slowGit(t)
	g := newRig(t, func(c *Config) { c.GitPath = script })
	g.setGrant(func(Endpoint) Grant { return Grant{Project: g.project, Worker: "worker_1"} })

	ctx, cancel := context.WithCancel(t.Context())
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, g.url()+"/info/refs?service=git-upload-pack", nil)
	req.Header.Set("Authorization", "Bearer very-secret-token")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, 5)
	if _, err := io.ReadFull(resp.Body, buf); err != nil || string(buf) != "hello" {
		t.Fatalf("body = %q (%v)", buf, err)
	}
	cancel()
	_ = resp.Body.Close()
	eventually(t, "the children of the backend to be gone", func() bool { return len(gittest.Descendants(t)) == 0 })

	raw, err := os.ReadFile(dump) //nolint:gosec // a path of this test
	if err != nil {
		t.Fatal(err)
	}
	env := string(raw)
	for _, forbidden := range []string{"very-secret-token", "HTTP_AUTHORIZATION", "Bearer"} {
		if strings.Contains(env, forbidden) {
			t.Errorf("the environment of the backend has %q", forbidden)
		}
	}
	for _, want := range []string{"GIT_CONFIG_NOSYSTEM=1", "REMOTE_USER=worker_1", "GIT_PROJECT_ROOT=" + g.r.cfg.Dir, "PATH_INFO=/" + string(g.project) + ".git/info/refs"} {
		if !strings.Contains(env, want) {
			t.Errorf("the environment of the backend lacks %q", want)
		}
	}
}

func TestRelay_BusyWhenTheSlotsOrTheWorkersPushAreTaken(t *testing.T) {
	script, _ := slowGit(t)
	g := newRig(t, func(c *Config) { c.GitPath = script; c.Concurrency = 2 })
	g.setGrant(func(Endpoint) Grant {
		return Grant{Project: g.project, Worker: "worker_1", Write: true, Branch: branch1, ExpectedOID: strings.Repeat("a", 40)}
	})
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	start := func(method, path, ctype string) *http.Response {
		req, _ := http.NewRequestWithContext(ctx, method, g.url()+path, strings.NewReader(""))
		if ctype != "" {
			req.Header.Set("Content-Type", ctype)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		return resp
	}
	pushType := "application/x-git-receive-pack-request"
	first := start(http.MethodPost, "/git-receive-pack", pushType)
	defer first.Body.Close()
	// The same Worker may not push twice at once, though a slot is free.
	if resp := start(http.MethodPost, "/git-receive-pack", pushType); resp.StatusCode != http.StatusTooManyRequests {
		t.Errorf("a second push of the same Worker got %d", resp.StatusCode)
	} else {
		_ = resp.Body.Close()
	}
	second := start(http.MethodGet, "/info/refs?service=git-upload-pack", "")
	defer second.Body.Close()
	// Both slots are taken.
	if resp := start(http.MethodGet, "/info/refs?service=git-upload-pack", ""); resp.StatusCode != http.StatusTooManyRequests {
		t.Errorf("a request with no slot got %d", resp.StatusCode)
	} else {
		_ = resp.Body.Close()
	}
}

func TestRelay_ARefusalOfContentEncodingIsABadBody(t *testing.T) {
	g := newRig(t, nil)
	g.allow(branch1, strings.Repeat("a", 40))
	req, _ := http.NewRequestWithContext(t.Context(), http.MethodPost, g.url()+"/git-receive-pack", strings.NewReader("0000"))
	req.Header.Set("Content-Type", "application/x-git-receive-pack-request")
	req.Header.Set("Content-Encoding", "gzip")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("status = %d", resp.StatusCode)
	}
}

func TestOpen_RefusesAGitThatIsTooOld(t *testing.T) {
	dir := t.TempDir()
	script := filepath.Join(dir, "git")
	if err := os.WriteFile(script, []byte("#!/bin/sh\necho 'git version 2.30.2'\n"), 0o700); err != nil { //nolint:gosec // a test script
		t.Fatal(err)
	}
	if _, err := Open(t.Context(), Config{Dir: filepath.Join(dir, "relay"), GitPath: script}); err == nil {
		t.Fatal("a git 2.30 was accepted")
	}
}

func TestEnsure_IsIdempotentAndConfiguresTheRepository(t *testing.T) {
	g := newRig(t, nil)
	var errs [4]error
	done := make(chan int)
	for i := range errs {
		go func() { _, errs[i] = g.r.Ensure(t.Context(), g.project); done <- i }()
	}
	for range errs {
		<-done
	}
	for i, err := range errs {
		if err != nil {
			t.Errorf("Ensure %d: %v", i, err)
		}
	}
	dir := g.r.RepoDir(g.project)
	for key, want := range map[string]string{
		"receive.denynonfastforwards": "true", "receive.denydeletes": "true", "receive.fsckobjects": "true", "gc.auto": "0",
	} {
		if got := gittest.Run(t, dir, "config", "--file", filepath.Join(dir, "config"), key); got != want {
			t.Errorf("%s = %q, want %q", key, got, want)
		}
	}
	if _, err := g.r.Ensure(t.Context(), "task_01ARZ3NDEKTSV4RRFFQ69G5FAV"); err == nil {
		t.Error("a Task ID named a repository")
	}
	if _, found, err := g.r.BranchTip(t.Context(), g.project, "main"); err == nil && found {
		t.Error("an empty relay has a branch")
	}
}
