package cli

import (
	"bytes"
	"crypto/sha256"
	"crypto/tls"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/http/cgi"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"

	v1 "github.com/Autarquico/codefabric/internal/api/v1"
	csession "github.com/Autarquico/codefabric/internal/client/session"
	"github.com/Autarquico/codefabric/internal/config"
	"github.com/Autarquico/codefabric/internal/domain"
)

const seedSessionID = "11111111-2222-4333-8444-555555555555"

// seedWorld is a Controller that answers what `cf do` and `cf task resume` ask of it, with
// a git relay behind it (the git of the machine, through its http-backend), a working
// directory that has a repository and a Claude Code session of this machine.
type seedWorld struct {
	t          *testing.T
	work       string
	bare       string // the repository of the Project, and its relay
	claudeHome string
	stateDir   string

	// exclude is checkpoint.exclude of the fake Controller.
	exclude []string

	mu       sync.Mutex
	requests []seenRequest
}

type seenRequest struct {
	Method, Path string
	Body         []byte
}

func gitIn(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.CommandContext(t.Context(), "git", append([]string{"-C", dir}, args...)...) //nolint:gosec // a test
	cmd.Env = append(os.Environ(), "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

func newSeedWorld(t *testing.T) *seedWorld {
	t.Helper()
	isolatedMachine(t)
	w := &seedWorld{t: t, work: t.TempDir(), bare: filepath.Join(t.TempDir(), "proj_1"), claudeHome: t.TempDir(), exclude: config.DefaultCheckpointExclude()}
	gitIn(t, filepath.Dir(w.bare), "init", "--quiet", "--bare", "--initial-branch=main", w.bare)
	gitIn(t, w.bare, "config", "http.receivepack", "true")
	gitIn(t, w.work, "init", "--quiet", "--initial-branch=main")
	write(t, filepath.Join(w.work, "a.txt"), "a\n")
	gitIn(t, w.work, "add", "-A")
	gitIn(t, w.work, "commit", "--quiet", "-m", "first")
	gitIn(t, w.work, "push", "--quiet", w.bare, "main")
	t.Chdir(w.work)

	paths, err := config.ResolveFromEnv()
	if err != nil {
		t.Fatal(err)
	}
	w.stateDir = filepath.Join(paths.ConfigDir, "sessions")
	ca := newTestCA(t)
	srv := httptest.NewUnstartedServer(http.HandlerFunc(w.serve))
	srv.TLS = &tls.Config{Certificates: []tls.Certificate{ca.serverCert(t)}, MinVersion: tls.VersionTLS13}
	srv.StartTLS()
	t.Cleanup(srv.Close)
	storeBundle(t, paths.CredentialsDir(), ca, srv.Listener.Addr().String())
	return w
}

func write(t *testing.T, p, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(p), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

// session writes a Claude Code session of this machine; secret makes it hold a credential.
func (w *seedWorld) session(secret bool) {
	lines := []string{
		`{"type":"user","sessionId":"` + seedSessionID + `","cwd":"` + w.work + `","version":"2.1.319","message":{"role":"user","content":"hi"}}`,
		`{"type":"assistant","sessionId":"` + seedSessionID + `","cwd":"` + w.work + `","message":{"role":"assistant","content":[{"type":"text","text":"ok"}]}}`,
	}
	if secret {
		lines = append(lines, `{"type":"user","sessionId":"`+seedSessionID+`","cwd":"`+w.work+`","message":{"role":"user","content":"AKIAABCDEFGHIJKLMNOP"}}`)
	}
	real, err := filepath.EvalSymlinks(w.work)
	if err != nil {
		w.t.Fatal(err)
	}
	dir := regexp.MustCompile(`[^A-Za-z0-9]`).ReplaceAllString(real, "-")
	write(w.t, filepath.Join(w.claudeHome, "projects", dir, seedSessionID+".jsonl"), strings.Join(lines, "\n")+"\n")
}

func (w *seedWorld) serve(rw http.ResponseWriter, r *http.Request) {
	if strings.HasPrefix(r.URL.Path, "/v1/relay/") {
		w.relay(rw, r)
		return
	}
	body, _ := io.ReadAll(r.Body)
	w.mu.Lock()
	w.requests = append(w.requests, seenRequest{r.Method, r.URL.Path, body})
	w.mu.Unlock()
	reply := func(v any) { _ = json.NewEncoder(rw).Encode(v) }
	switch {
	case r.Method == http.MethodGet && r.URL.Path == "/v1/checkpoint/exclude":
		reply(v1.CheckpointExclude{Exclude: w.exclude})
	case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/v1/projects/"):
		reply(v1.Project{ID: "proj_1", Name: "p", Repo: w.bare, DefaultBranch: "main", NativeState: true})
	case r.Method == http.MethodPut && strings.Contains(r.URL.Path, "/native-seed/"):
		reply(v1.NativeSeedUploaded{})
	case r.Method == http.MethodPost && r.URL.Path == "/v1/relay-token":
		reply(v1.RelayTokenResponse{Token: "t", Path: "/v1/relay/proj_1", Write: true})
	case r.Method == http.MethodPost && r.URL.Path == "/v1/tasks":
		reply(v1.CreateTaskResponse{Task: v1.Task{ID: "task_1", Short: "T-1", Name: "fix", ProjectID: "proj_1"}})
	case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/v1/tasks/"):
		reply(v1.Task{ID: "task_1", Short: "T-1", Name: "fix", ProjectID: "proj_1"})
	case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/v1/runs/"):
		reply(v1.Run{
			ID: "run_1", Short: "R-1", TaskID: "task_1", Lineage: domain.LineageInitial,
			LineageNativeSeed: &v1.NativeStateRef{Adapter: "claude-code", Digest: "sha256:abc"},
		})
	case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/resume"):
		reply(v1.ResumeTaskResponse{Task: v1.Task{ID: "task_1", Name: "fix"}, RunID: "run_2", RunShort: "R-2", CheckpointID: "cp_1"})
	default:
		http.NotFound(rw, r)
	}
}

// relay is the git of the machine serving the repository of the Project.
func (w *seedWorld) relay(rw http.ResponseWriter, r *http.Request) {
	out, err := exec.CommandContext(r.Context(), "git", "--exec-path").Output() //nolint:gosec // a test
	if err != nil {
		http.Error(rw, err.Error(), http.StatusInternalServerError)
		return
	}
	h := cgi.Handler{
		Path: filepath.Join(strings.TrimSpace(string(out)), "git-http-backend"), Root: "/v1/relay",
		Env: []string{"GIT_PROJECT_ROOT=" + filepath.Dir(w.bare), "GIT_HTTP_EXPORT_ALL=1"},
	}
	h.ServeHTTP(rw, r)
}

func (w *seedWorld) seen(method, pathContains string) []seenRequest {
	w.mu.Lock()
	defer w.mu.Unlock()
	var out []seenRequest
	for _, r := range w.requests {
		if r.Method == method && strings.Contains(r.Path, pathContains) {
			out = append(out, r)
		}
	}
	return out
}

// cf runs the CLI with the sessions of this world's Claude Code.
func (w *seedWorld) cf(args ...string) (code int, stdout, stderr string) {
	w.t.Helper()
	opts := Options{Sessions: func(stateDir string) *csession.Local {
		l := csession.New(stateDir)
		l.ConfigDirs = map[string]string{"claude-code": w.claudeHome, "codex": w.t.TempDir()}
		return l
	}}
	var out, errOut bytes.Buffer
	code = RunContextWith(w.t.Context(), opts, append(args, "--json"), strings.NewReader(""), &out, &errOut)
	return code, out.String(), errOut.String()
}

func TestScenario_SessionSeed_DoUploadsTheSessionAndLeavesTheMarker(t *testing.T) {
	w := newSeedWorld(t)
	w.session(false)

	code, out, errOut := w.cf("do", "--project", "p", "--session", seedSessionID, "fix it")
	if code != 0 {
		t.Fatalf("exit %d: %s %s", code, out, errOut)
	}
	puts := w.seen(http.MethodPut, "/native-seed/")
	if len(puts) != 1 {
		t.Fatalf("uploads: %d", len(puts))
	}
	sum := sha256.Sum256(puts[0].Body)
	digest := "sha256:" + hex.EncodeToString(sum[:])
	if !strings.HasSuffix(puts[0].Path, digest) {
		t.Errorf("uploaded to %s, the archive is %s", puts[0].Path, digest)
	}
	var req v1.CreateTaskRequest
	posts := w.seen(http.MethodPost, "/v1/tasks")
	if len(posts) != 1 || json.Unmarshal(posts[0].Body, &req) != nil {
		t.Fatalf("task requests: %+v", posts)
	}
	if req.NativeSeed == nil || req.NativeSeed.Adapter != "claude-code" || req.NativeSeed.Digest != digest || req.NativeSeed.AdapterVersion != "2.1.319" || req.Project != "proj_1" {
		t.Errorf("native_seed = %+v, project %q", req.NativeSeed, req.Project)
	}
	rec, err := csession.NewState(w.stateDir).Get(seedSessionID)
	if err != nil || rec.Task != "task_1" || rec.HandedOff == nil || rec.HandedOff.Digest != digest {
		t.Errorf("marker: %+v %v", rec, err)
	}
}

func TestScenario_SessionSeed_ASecretBlocksTheUploadUnlessAllowed(t *testing.T) {
	w := newSeedWorld(t)
	w.session(true)

	code, out, _ := w.cf("do", "--project", "p", "--session", seedSessionID, "fix it")
	if env := errorOf(t, out); code == 0 || env["code"] != string(domain.CodePolicyDenied) {
		t.Fatalf("exit %d: %v", code, env)
	}
	if n := len(w.seen(http.MethodPut, "/native-seed/")) + len(w.seen(http.MethodPost, "/v1/tasks")); n != 0 {
		t.Fatalf("%d requests went out after a block", n)
	}
	if rec, _ := csession.NewState(w.stateDir).Get(seedSessionID); rec.HandedOff != nil {
		t.Error("a marker was left for a session that was not sent")
	}

	if code, out, errOut := w.cf("do", "--project", "p", "--session", seedSessionID, "--allow-sensitive-session", "fix it"); code != 0 {
		t.Fatalf("exit %d: %s %s", code, out, errOut)
	}
	if len(w.seen(http.MethodPut, "/native-seed/")) != 1 {
		t.Error("the session was not uploaded with --allow-sensitive-session")
	}
}

func TestScenario_SessionSeed_SessionFlagsAreCheckedBeforeAnythingIsSent(t *testing.T) {
	w := newSeedWorld(t)
	w.session(false)
	for _, args := range [][]string{
		{"do", "--project", "p", "--allow-sensitive-session", "fix it"},
		{"task", "resume", "fix", "--allow-sensitive-session"},
		{"task", "resume", "fix", "--snapshot"},
	} {
		code, out, _ := w.cf(args...)
		if env := errorOf(t, out); code != 2 || env["code"] != string(domain.CodeInvalidRequest) {
			t.Errorf("%v: exit %d %v", args, code, env)
		}
	}
	code, out, _ := w.cf("do", "--project", "p", "--session", "99999999-2222-4333-8444-555555555555", "fix it")
	if env := errorOf(t, out); code == 0 || env["code"] != string(domain.CodeNotFound) {
		t.Errorf("an unknown session: exit %d %v", code, env)
	}
	if len(w.seen(http.MethodPost, "/v1/tasks")) != 0 {
		t.Error("a Task was created")
	}
}

func TestScenario_SessionSeed_AHandedOffSessionNeedsForce(t *testing.T) {
	w := newSeedWorld(t)
	w.session(false)
	if code, out, errOut := w.cf("do", "--project", "p", "--session", seedSessionID, "fix it"); code != 0 {
		t.Fatalf("exit %d: %s %s", code, out, errOut)
	}

	code, out, _ := w.cf("do", "--project", "p", "--session", seedSessionID, "again")
	if env := errorOf(t, out); code == 0 || env["code"] != string(domain.CodeConfirmationRequired) {
		t.Fatalf("the second writer: exit %d %v", code, env)
	}
	if code, out, errOut := w.cf("do", "--project", "p", "--session", seedSessionID, "--force", "again"); code != 0 {
		t.Fatalf("--force: exit %d: %s %s", code, out, errOut)
	}
	if n := len(w.seen(http.MethodPost, "/v1/tasks")); n != 2 {
		t.Errorf("Tasks created: %d", n)
	}
}

func TestScenario_SessionSeed_ResumeSendsTheSeedAndForce(t *testing.T) {
	w := newSeedWorld(t)
	w.session(false)

	if code, out, errOut := w.cf("task", "resume", "fix", "--session", seedSessionID, "--force"); code != 0 {
		t.Fatalf("exit %d: %s %s", code, out, errOut)
	}
	resumes := w.seen(http.MethodPost, "/resume")
	var req v1.ResumeTaskRequest
	if len(resumes) != 1 || json.Unmarshal(resumes[0].Body, &req) != nil {
		t.Fatalf("resumes: %+v", resumes)
	}
	if req.NativeSeed == nil || req.NativeSeed.Adapter != "claude-code" || !req.Force || req.SourceSnapshot != "" {
		t.Errorf("request: %+v", req)
	}
	if len(w.seen(http.MethodPut, "/proj_1/native-seed/")) != 1 {
		t.Error("the seed went to the Project of the Task")
	}
	if rec, _ := csession.NewState(w.stateDir).Get(seedSessionID); rec.HandedOff == nil || rec.HandedOff.Task != "task_1" {
		t.Errorf("marker: %+v", rec)
	}
}

func TestScenario_SessionSeed_ResumeWithoutSessionSendsNoSeed(t *testing.T) {
	w := newSeedWorld(t)
	if code, out, errOut := w.cf("task", "resume", "fix"); code != 0 {
		t.Fatalf("exit %d: %s %s", code, out, errOut)
	}
	var req v1.ResumeTaskRequest
	resumes := w.seen(http.MethodPost, "/resume")
	if len(resumes) != 1 || json.Unmarshal(resumes[0].Body, &req) != nil || req.NativeSeed != nil || req.Force {
		t.Errorf("%+v %+v", resumes, req)
	}
}

func TestScenario_SessionSeed_SessionCombinesWithSnapshotAndTheExcludedFilesStayHome(t *testing.T) {
	w := newSeedWorld(t)
	w.session(false)
	write(t, filepath.Join(w.work, "wip.txt"), "work in progress\n")
	write(t, filepath.Join(w.work, ".env"), "TOKEN=secret\n")
	write(t, filepath.Join(w.work, "cert.pem"), "pem\n")

	code, out, errOut := w.cf("do", "--project", "p", "--session", seedSessionID, "--snapshot", "fix it")
	if code != 0 {
		t.Fatalf("exit %d: %s %s", code, out, errOut)
	}
	var req v1.CreateTaskRequest
	posts := w.seen(http.MethodPost, "/v1/tasks")
	if len(posts) != 1 || json.Unmarshal(posts[0].Body, &req) != nil {
		t.Fatalf("task requests: %+v", posts)
	}
	if req.NativeSeed == nil || req.SourceSnapshot == "" || req.SourceSnapshot != req.SourceCommit || req.SourceBranch != "main" {
		t.Fatalf("seed and snapshot together: %+v", req)
	}
	ref := domain.SnapshotRef(req.SourceSnapshot)
	if got := gitIn(t, w.bare, "rev-parse", ref); got != req.SourceSnapshot {
		t.Fatalf("the relay has %s at %s", ref, got)
	}
	files := gitIn(t, w.bare, "ls-tree", "-r", "--name-only", ref)
	if !strings.Contains(files, "wip.txt") || strings.Contains(files, ".env") || strings.Contains(files, "cert.pem") {
		t.Errorf("the snapshot holds:\n%s", files)
	}
	var listed struct {
		Task v1.Task `json:"task"`
	}
	if err := json.Unmarshal([]byte(out), &listed); err != nil {
		t.Fatalf("stdout: %v %q", err, out)
	}
	if _, err := os.Stat(filepath.Join(w.work, ".env")); err != nil {
		t.Error("the working tree was touched")
	}
}

func TestScenario_SessionSeed_ASnapshotNamesWhatItLeftOut(t *testing.T) {
	w := newSeedWorld(t)
	write(t, filepath.Join(w.work, ".env"), "TOKEN=secret\n")
	var out, errOut bytes.Buffer
	code := RunContextWith(t.Context(), Options{}, []string{"do", "--project", "p", "--snapshot", "fix it"}, strings.NewReader(""), &out, &errOut)
	if code != 0 {
		t.Fatalf("exit %d: %s %s", code, out.String(), errOut.String())
	}
	if !strings.Contains(out.String(), "Left out of the snapshot") || !strings.Contains(out.String(), ".env") {
		t.Errorf("the output does not say what stayed home:\n%s", out.String())
	}
}

func TestScenario_SessionSeed_RunShowSaysWhichSessionSeededTheRun(t *testing.T) {
	newSeedWorld(t)
	var out, errOut bytes.Buffer
	if code := RunContext(t.Context(), []string{"run", "show", "run_1"}, strings.NewReader(""), &out, &errOut); code != 0 {
		t.Fatalf("exit %d: %s %s", code, out.String(), errOut.String())
	}
	if !strings.Contains(out.String(), "native seed: claude-code session sha256:abc") {
		t.Errorf("%s", out.String())
	}
	out.Reset()
	if code := RunContext(t.Context(), []string{"run", "show", "run_1", "--json"}, strings.NewReader(""), &out, &errOut); code != 0 || !strings.Contains(out.String(), `"lineage_native_seed":{"adapter":"claude-code"`) {
		t.Errorf("exit %d: %s", code, out.String())
	}
}

func TestScenario_SessionSeed_ASnapshotLeavesOutWhatTheControllerExcludes(t *testing.T) {
	w := newSeedWorld(t)
	w.exclude = []string{"secrets/**", "**/*.pem"}
	write(t, filepath.Join(w.work, "secrets", "a.txt"), "a\n")
	write(t, filepath.Join(w.work, ".env"), "TOKEN=x\n")
	write(t, filepath.Join(w.work, "wip.txt"), "wip\n")

	code, out, errOut := w.cf("do", "--project", "p", "--snapshot", "fix it")
	if code != 0 {
		t.Fatalf("exit %d: %s %s", code, out, errOut)
	}
	var req v1.CreateTaskRequest
	posts := w.seen(http.MethodPost, "/v1/tasks")
	if len(posts) != 1 || json.Unmarshal(posts[0].Body, &req) != nil {
		t.Fatalf("task requests: %+v", posts)
	}
	files := gitIn(t, w.bare, "ls-tree", "-r", "--name-only", domain.SnapshotRef(req.SourceSnapshot))
	if strings.Contains(files, "secrets/") || !strings.Contains(files, ".env") || !strings.Contains(files, "wip.txt") {
		t.Errorf("the snapshot follows the exclusions of the Controller, not the defaults:\n%s", files)
	}
}
