package taskpull_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Autarquico/codefabric/internal/adapters/harness"
	"github.com/Autarquico/codefabric/internal/adapters/harness/claude"
	v1 "github.com/Autarquico/codefabric/internal/api/v1"
	"github.com/Autarquico/codefabric/internal/client/session"
	"github.com/Autarquico/codefabric/internal/client/taskpull"
	"github.com/Autarquico/codefabric/internal/domain"
)

const sessionID = "11111111-2222-4333-8444-555555555555"

func gitIn(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.CommandContext(t.Context(), "git", append([]string{"-C", dir, //nolint:gosec // test helper: fixed git options, arguments from the test
		"-c", "user.name=t", "-c", "user.email=t@example.com", "-c", "commit.gpgsign=false", "-c", "protocol.file.allow=always"}, args...)...)
	cmd.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_NOSYSTEM=1")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

func write(t *testing.T, path, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

func commitFile(t *testing.T, dir, name, body, msg string) string {
	t.Helper()
	write(t, filepath.Join(dir, name), body)
	gitIn(t, dir, "add", "-A")
	gitIn(t, dir, "commit", "-q", "-m", msg)
	return gitIn(t, dir, "rev-parse", "HEAD")
}

// world is the machine of the user (mac), the relay of the Controller and a Worker that
// took the work from the mac and went on.
type world struct {
	t      *testing.T
	mac    string
	relay  string
	worker string
	base   string
	local  *session.Local
	home   string
	remote *fakeRemote
}

func newWorld(t *testing.T) *world {
	t.Helper()
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	w := &world{t: t, mac: filepath.Join(root, "mac"), relay: filepath.Join(root, "relay.git"), worker: filepath.Join(root, "worker"), home: filepath.Join(root, "claude-home")}
	if err := os.MkdirAll(w.mac, 0o750); err != nil {
		t.Fatal(err)
	}
	gitIn(t, w.mac, "init", "-q", "-b", "main")
	w.base = commitFile(t, w.mac, "README", "base\n", "base")
	gitIn(t, root, "init", "-q", "--bare", w.relay)
	gitIn(t, root, "clone", "-q", w.mac, w.worker)
	w.local = session.New(filepath.Join(root, "cf-state"))
	w.local.ConfigDirs = map[string]string{claude.ID: w.home}
	w.remote = &fakeRemote{w: w, plan: v1.PullPlan{
		TaskID: "tsk_1", Task: "fix-login", Project: "demo", ProjectID: "prj_1", RunID: "run_1", RunShort: "r1", RunState: domain.RunSuspended,
	}}
	return w
}

// checkpoint publishes what the Worker has at HEAD as a checkpoint of the Task, with the
// session lines given (none: no native state).
func (w *world) checkpoint(sessionLines ...string) {
	w.t.Helper()
	commit := gitIn(w.t, w.worker, "rev-parse", "HEAD")
	ref := "refs/cf/checkpoints/ckp_1"
	gitIn(w.t, w.worker, "push", "-q", "-f", w.relay, commit+":"+ref)
	w.remote.plan.Commit = commit
	w.remote.plan.Checkpoint = &v1.Checkpoint{ID: "ckp_1", Ref: ref, Excluded: []string{}, Warnings: []string{}}
	if len(sessionLines) > 0 {
		w.remote.archive = sessionArchive(w.t, "/home/worker/proj", sessionID, sessionLines)
		sum := sha256.Sum256(w.remote.archive)
		w.remote.plan.Checkpoint.NativeState = &v1.NativeStateRef{Adapter: claude.ID, Digest: "sha256:" + hex.EncodeToString(sum[:])}
	}
}

func (w *world) pull(opts taskpull.Options) (taskpull.Result, error) {
	w.t.Helper()
	opts.Dir = w.mac
	if opts.Sessions == nil && !opts.NoSession {
		opts.Sessions = w.local
	}
	return taskpull.Pull(context.Background(), w.remote, "fix-login", opts)
}

// located is where Claude Code would look for the session of a checkout.
func (w *world) located(cwd, id string) (string, error) {
	files, err := claude.Adapter{}.Locate(context.Background(), harness.SessionRef{ConfigDir: w.home, Cwd: cwd, SessionID: id})
	if err != nil {
		return "", err
	}
	raw, err := os.ReadFile(files.Transcript)
	return string(raw), err
}

func line(cwd, text string) string {
	raw, _ := json.Marshal(map[string]any{"type": "user", "cwd": cwd, "sessionId": sessionID, "message": map[string]any{"content": text}})
	return string(raw)
}

func sessionArchive(t *testing.T, cwd, id string, lines []string) []byte {
	t.Helper()
	var buf bytes.Buffer
	body := strings.Join(lines, "\n") + "\n"
	m := harness.SessionManifest{Adapter: claude.ID, SessionID: id, Cwd: cwd, Lines: len(lines)}
	if err := harness.WriteSessionArchive(&buf, m, []byte(body), nil); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// fakeRemote is the Controller and its relay: it answers with the plan, and fetches from
// the bare repository the Worker pushed to.
type fakeRemote struct {
	w       *world
	mu      sync.Mutex
	plan    v1.PullPlan
	archive []byte
	// afterDefer is the plan once the Run was deferred.
	afterDefer func(*v1.PullPlan)
	deferred   int
	pulled     int
	// pulledErr, when set, is what MarkTaskPulled answers once.
	pulledErr error
	polls     int
}

func (f *fakeRemote) PullPlan(context.Context, string, string) (v1.PullPlan, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.polls++
	return f.plan, nil
}

func (f *fakeRemote) DeferTask(context.Context, string, string, string) (v1.Task, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.deferred++
	if f.afterDefer != nil {
		f.afterDefer(&f.plan)
	}
	return v1.Task{}, nil
}

func (f *fakeRemote) FetchRelayRef(_ context.Context, dir, _, ref string) (string, error) {
	gitIn(f.w.t, dir, "fetch", "-q", f.w.relay, ref)
	return gitIn(f.w.t, dir, "rev-parse", "FETCH_HEAD^{commit}"), nil
}

func (f *fakeRemote) OpenPullNativeState(context.Context, string, string) (io.ReadCloser, int64, error) {
	if f.archive == nil {
		return nil, 0, errors.New("no native state")
	}
	return io.NopCloser(bytes.NewReader(f.archive)), int64(len(f.archive)), nil
}

func (f *fakeRemote) MarkTaskPulled(context.Context, string, string, string) (v1.PulledTaskResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.pulled++
	if err := f.pulledErr; err != nil {
		f.pulledErr = nil
		return v1.PulledTaskResponse{}, err
	}
	return v1.PulledTaskResponse{}, nil
}

func (w *world) head() string        { return gitIn(w.t, w.mac, "rev-parse", "HEAD") }
func (w *world) branch() string      { return gitIn(w.t, w.mac, "branch", "--show-current") }
func (w *world) tip(b string) string { return gitIn(w.t, w.mac, "rev-parse", "refs/heads/"+b) }

func TestScenario_SessionPull_Clean(t *testing.T) {
	t.Run("the branch the Task started from advances", func(t *testing.T) {
		w := newWorld(t)
		gitIn(t, w.worker, "commit", "-q", "--allow-empty", "-m", "cf: uncommitted changes at exit")
		commitFile(t, w.worker, "feature.go", "package x\n", "feature")
		w.checkpoint(line("/home/worker/proj", "remember pineapple"))
		w.remote.plan.SourceBranch = "main"

		res, err := w.pull(taskpull.Options{})
		if err != nil {
			t.Fatal(err)
		}
		if res.Branch != "main" || res.Outcome != taskpull.Advanced || !res.CheckedOut {
			t.Fatalf("result = %+v", res)
		}
		if w.head() != w.remote.plan.Commit || w.branch() != "main" {
			t.Fatalf("main is at %s on %s, want %s", w.head(), w.branch(), w.remote.plan.Commit)
		}
		if _, err := os.Stat(filepath.Join(w.mac, "feature.go")); err != nil {
			t.Fatalf("the work is not in the tree: %v", err)
		}
		if res.Session == nil || len(res.Session.Resume) != 2 || res.Session.Resume[0] != "claude" || res.Session.Resume[1] != "--resume="+res.Session.ID {
			t.Fatalf("resume command = %+v", res.Session)
		}
		body, err := w.located(w.mac, res.Session.ID)
		if err != nil || !strings.Contains(body, "pineapple") || !strings.Contains(body, `"cwd":"`+w.mac+`"`) {
			t.Fatalf("the session was not written for %s: %v\n%s", w.mac, err, body)
		}
		if w.remote.pulled != 1 {
			t.Fatalf("TaskPulled sent %d times", w.remote.pulled)
		}
	})

	t.Run("without a branch of origin the work goes to cf/<task>", func(t *testing.T) {
		w := newWorld(t)
		work := commitFile(t, w.worker, "feature.go", "package x\n", "feature")
		w.checkpoint()
		res, err := w.pull(taskpull.Options{})
		if err != nil {
			t.Fatal(err)
		}
		if res.Branch != "cf/fix-login" || res.Outcome != taskpull.Created || w.branch() != "cf/fix-login" || w.head() != work {
			t.Fatalf("result = %+v, head %s on %s", res, w.head(), w.branch())
		}
		if got := w.tip("main"); got != w.base {
			t.Fatalf("main moved to %s", got)
		}
	})

	t.Run("the sealing commit is told", func(t *testing.T) {
		w := newWorld(t)
		commitFile(t, w.worker, "wip.txt", "unfinished\n", "cf: uncommitted changes at exit")
		w.checkpoint()
		res, err := w.pull(taskpull.Options{})
		if err != nil {
			t.Fatal(err)
		}
		if !res.Sealed {
			t.Fatalf("result = %+v", res)
		}
	})
}

func TestScenario_SessionPull_RunVivo(t *testing.T) {
	w := newWorld(t)
	w.remote.plan.RunState, w.remote.plan.Live = domain.RunRunning, true
	before := w.head()
	_, err := w.pull(taskpull.Options{})
	var coded *domain.Error
	if !errors.As(err, &coded) || coded.Code != domain.CodeInvalidTransition || !strings.Contains(err.Error(), "--defer") {
		t.Fatalf("err = %v", err)
	}
	if w.head() != before || w.remote.deferred != 0 || w.remote.pulled != 0 {
		t.Fatalf("something changed: head %s, deferred %d, pulled %d", w.head(), w.remote.deferred, w.remote.pulled)
	}
}

func TestScenario_SessionPull_RunVivoConDefer(t *testing.T) {
	w := newWorld(t)
	work := commitFile(t, w.worker, "feature.go", "package x\n", "feature")
	w.remote.plan.RunState, w.remote.plan.Live = domain.RunRunning, true
	w.remote.afterDefer = func(p *v1.PullPlan) {
		// The Worker publishes the checkpoint a moment after it was asked for.
		w.checkpoint()
		p.RunState, p.Live = domain.RunSuspended, false
	}
	res, err := w.pull(taskpull.Options{Defer: true, Poll: time.Millisecond, Wait: 10 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	if !res.Deferred || w.remote.deferred != 1 || w.head() != work {
		t.Fatalf("result = %+v, deferred %d, head %s", res, w.remote.deferred, w.head())
	}
}

func TestScenario_SessionPull_RamaDePartidaNoAncestro(t *testing.T) {
	w := newWorld(t)
	local := commitFile(t, w.mac, "mine.txt", "only here\n", "local work on main")
	commitFile(t, w.worker, "feature.go", "package x\n", "feature")
	w.checkpoint()
	w.remote.plan.SourceBranch = "main"
	res, err := w.pull(taskpull.Options{})
	if err != nil {
		t.Fatal(err)
	}
	if res.Branch != "cf/fix-login" || w.tip("main") != local {
		t.Fatalf("branch %s, main at %s want %s", res.Branch, w.tip("main"), local)
	}
}

func TestScenario_SessionPull_SinSesion(t *testing.T) {
	w := newWorld(t)
	commitFile(t, w.worker, "feature.go", "package x\n", "feature")
	w.checkpoint(line("/home/worker/proj", "remember pineapple"))
	res, err := w.pull(taskpull.Options{NoSession: true})
	if err != nil {
		t.Fatal(err)
	}
	if res.Session != nil {
		t.Fatalf("session = %+v", res.Session)
	}
	if _, err := os.Stat(w.home); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("the home of Claude was touched: %v", err)
	}
	if w.remote.pulled != 1 {
		t.Fatalf("TaskPulled sent %d times", w.remote.pulled)
	}
}

func TestScenario_SessionPull_VueltaDeUnaSemillaPropia(t *testing.T) {
	w := newWorld(t)
	// The session of the user, seeded to the Task and marked as handed off.
	early := sessionArchive(t, w.mac, sessionID, []string{line(w.mac, "one")})
	if _, err := (claude.Adapter{}).Import(t.Context(), harness.SessionImport{ConfigDir: w.home, Cwd: w.mac}, bytes.NewReader(early)); err != nil {
		t.Fatal(err)
	}
	if err := w.local.State.MarkHandedOff(sessionID, "fix-login", "sha256:seed", "", time.Now()); err != nil {
		t.Fatal(err)
	}
	commitFile(t, w.worker, "feature.go", "package x\n", "feature")
	w.checkpoint(line("/home/worker/proj", "one"), line("/home/worker/proj", "two"))
	w.remote.plan.SeedDigest = "sha256:seed"

	res, err := w.pull(taskpull.Options{})
	if err != nil {
		t.Fatal(err)
	}
	if res.Session == nil || res.Session.ID != sessionID || res.Session.NewID || !strings.HasSuffix(res.Session.Backup, ".bak") {
		t.Fatalf("session = %+v", res.Session)
	}
	if body, err := w.located(w.mac, sessionID); err != nil || !strings.Contains(body, "two") {
		t.Fatalf("the local session was not updated: %v\n%s", err, body)
	}
	if bak, err := os.ReadFile(res.Session.Backup); err != nil || strings.Contains(string(bak), "two") || !strings.Contains(string(bak), "one") {
		t.Fatalf("the backup is not the previous session: %v\n%s", err, bak)
	}
	rec, err := w.local.State.Get(sessionID)
	if err != nil || rec.HandedOff != nil || rec.Task != "fix-login" {
		t.Fatalf("record = %+v, %v", rec, err)
	}
}

func TestScenario_SessionPull_Dirty(t *testing.T) {
	w := newWorld(t)
	write(t, filepath.Join(w.mac, "README"), "edited here\n")
	write(t, filepath.Join(w.mac, "scratch.txt"), "untracked\n")
	statusBefore := gitIn(t, w.mac, "status", "--porcelain")
	work := commitFile(t, w.worker, "feature.go", "package x\n", "feature")
	w.checkpoint(`{"type":"user","cwd":"/home/worker/proj"}`)

	res, err := w.pull(taskpull.Options{})
	if err != nil {
		t.Fatal(err)
	}
	if !res.Dirty || res.CheckedOut || res.Branch != "cf/fix-login" || w.tip("cf/fix-login") != work {
		t.Fatalf("result = %+v", res)
	}
	if w.branch() != "main" || gitIn(t, w.mac, "status", "--porcelain") != statusBefore {
		t.Fatalf("the tree changed: on %s\n%s", w.branch(), gitIn(t, w.mac, "status", "--porcelain"))
	}
	if raw, _ := os.ReadFile(filepath.Join(w.mac, "README")); string(raw) != "edited here\n" {
		t.Fatalf("README = %q", raw)
	}
	if gitIn(t, w.mac, "stash", "list") != "" {
		t.Fatal("something was stashed")
	}
	if res.Session != nil || !slices.ContainsFunc(res.Warnings, func(s string) bool { return strings.Contains(s, "--worktree") }) {
		t.Fatalf("session = %+v, warnings = %q: without the branch checked out the session stays behind", res.Session, res.Warnings)
	}
}

func TestScenario_SessionPull_Worktree(t *testing.T) {
	w := newWorld(t)
	write(t, filepath.Join(w.mac, "README"), "edited here\n")
	work := commitFile(t, w.worker, "feature.go", "package x\n", "feature")
	w.checkpoint(line("/home/worker/proj", "remember pineapple"))
	target := filepath.Join(filepath.Dir(w.mac), "x")

	res, err := w.pull(taskpull.Options{Worktree: "../x"})
	if err != nil {
		t.Fatal(err)
	}
	if !res.Worktree || res.Dir != target || gitIn(t, target, "rev-parse", "HEAD") != work {
		t.Fatalf("result = %+v", res)
	}
	if raw, _ := os.ReadFile(filepath.Join(w.mac, "README")); string(raw) != "edited here\n" {
		t.Fatalf("README = %q", raw)
	}
	if body, err := w.located(target, res.Session.ID); err != nil || !strings.Contains(body, `"cwd":"`+target+`"`) {
		t.Fatalf("the session is not registered for %s: %v\n%s", target, err, body)
	}
}

func TestScenario_SessionPull_RamaLocalDivergente(t *testing.T) {
	w := newWorld(t)
	gitIn(t, w.mac, "switch", "-q", "-c", "cf/fix-login")
	mine := commitFile(t, w.mac, "mine.txt", "mine\n", "my commit")
	gitIn(t, w.mac, "switch", "-q", "main")
	work := commitFile(t, w.worker, "feature.go", "package x\n", "feature")
	w.checkpoint()

	res, err := w.pull(taskpull.Options{})
	if err != nil {
		t.Fatal(err)
	}
	if res.Branch != "cf/fix-login@r1" || w.tip("cf/fix-login@r1") != work || w.tip("cf/fix-login") != mine {
		t.Fatalf("result = %+v", res)
	}
	if !strings.Contains(strings.Join(res.Warnings, "|"), "local branch cf/fix-login has commits") {
		t.Fatalf("no warning: %+v", res.Warnings)
	}
}

func TestScenario_SessionPull_SesionLocalPrefijo(t *testing.T) {
	w := newWorld(t)
	early := sessionArchive(t, w.mac, sessionID, []string{line(w.mac, "one")})
	if _, err := (claude.Adapter{}).Import(t.Context(), harness.SessionImport{ConfigDir: w.home, Cwd: w.mac}, bytes.NewReader(early)); err != nil {
		t.Fatal(err)
	}
	if err := w.local.State.MarkHandedOff(sessionID, "fix-login", "sha256:seed", "", time.Now()); err != nil {
		t.Fatal(err)
	}
	commitFile(t, w.worker, "feature.go", "package x\n", "feature")
	w.checkpoint(line("/home/worker/proj", "one"), line("/home/worker/proj", "two"))
	w.remote.plan.SeedDigest = "sha256:seed"
	res, err := w.pull(taskpull.Options{})
	if err != nil {
		t.Fatal(err)
	}
	if res.Session.NewID || res.Session.Backup == "" {
		t.Fatalf("session = %+v", res.Session)
	}
	if _, err := os.Stat(res.Session.Backup); err != nil {
		t.Fatal(err)
	}
}

func TestScenario_SessionPull_SesionDivergente(t *testing.T) {
	w := newWorld(t)
	local := sessionArchive(t, w.mac, sessionID, []string{line(w.mac, "went on elsewhere")})
	if _, err := (claude.Adapter{}).Import(t.Context(), harness.SessionImport{ConfigDir: w.home, Cwd: w.mac}, bytes.NewReader(local)); err != nil {
		t.Fatal(err)
	}
	before, _ := w.located(w.mac, sessionID)
	if err := w.local.State.MarkHandedOff(sessionID, "fix-login", "sha256:seed", "", time.Now()); err != nil {
		t.Fatal(err)
	}
	commitFile(t, w.worker, "feature.go", "package x\n", "feature")
	w.checkpoint(line("/home/worker/proj", "one"), line("/home/worker/proj", "two"))
	w.remote.plan.SeedDigest = "sha256:seed"

	res, err := w.pull(taskpull.Options{})
	if err != nil {
		t.Fatal(err)
	}
	if res.Session == nil || !res.Session.NewID || res.Session.ID == sessionID || len(res.Warnings) == 0 {
		t.Fatalf("session = %+v, warnings %v", res.Session, res.Warnings)
	}
	if after, _ := w.located(w.mac, sessionID); after != before {
		t.Fatalf("the local session was overwritten:\n%s", after)
	}
	if res.Session.Resume[1] != "--resume="+res.Session.ID {
		t.Fatalf("resume = %v", res.Session.Resume)
	}
}

func TestScenario_SessionPull_SesionAbierta(t *testing.T) {
	w := newWorld(t)
	early := sessionArchive(t, w.mac, sessionID, []string{line(w.mac, "one")})
	if _, err := (claude.Adapter{}).Import(t.Context(), harness.SessionImport{ConfigDir: w.home, Cwd: w.mac}, bytes.NewReader(early)); err != nil {
		t.Fatal(err)
	}
	if err := w.local.State.MarkHandedOff(sessionID, "fix-login", "sha256:seed", "", time.Now()); err != nil {
		t.Fatal(err)
	}
	live, _ := json.Marshal(map[string]any{"pid": os.Getpid(), "sessionId": sessionID})
	write(t, filepath.Join(w.home, "sessions", "1.json"), string(live))
	commitFile(t, w.worker, "feature.go", "package x\n", "feature")
	w.checkpoint(line("/home/worker/proj", "one"), line("/home/worker/proj", "two"))
	w.remote.plan.SeedDigest = "sha256:seed"
	before, _ := w.located(w.mac, sessionID)
	head := w.head()

	_, err := w.pull(taskpull.Options{})
	var liveErr *session.LiveError
	if !errors.As(err, &liveErr) {
		t.Fatalf("err = %v", err)
	}
	if after, _ := w.located(w.mac, sessionID); after != before || w.head() != head || w.branch() != "main" || w.remote.pulled != 0 {
		t.Fatal("something changed")
	}
	if out := gitIn(t, w.mac, "branch", "--list", "cf/*"); out != "" {
		t.Fatalf("a branch was made: %s", out)
	}
}

func TestScenario_SessionPull_Excluidos(t *testing.T) {
	w := newWorld(t)
	write(t, filepath.Join(w.mac, ".env"), "TOKEN=local\n")
	write(t, filepath.Join(w.mac, ".gitignore"), ".env\n")
	gitIn(t, w.mac, "add", ".gitignore")
	gitIn(t, w.mac, "commit", "-q", "-m", "ignore .env")
	gitIn(t, w.worker, "pull", "-q", w.mac, "main")
	commitFile(t, w.worker, "feature.go", "package x\n", "feature")
	w.checkpoint()
	w.remote.plan.Checkpoint.Excluded = []string{".env", "secrets/prod.key"}

	res, err := w.pull(taskpull.Options{})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(res.Excluded, ",") != ".env,secrets/prod.key" {
		t.Fatalf("excluded = %v", res.Excluded)
	}
	if raw, _ := os.ReadFile(filepath.Join(w.mac, ".env")); string(raw) != "TOKEN=local\n" {
		t.Fatalf(".env = %q", raw)
	}
}

func TestPull_NotAGitRepository(t *testing.T) {
	w := newWorld(t)
	_, err := taskpull.Pull(context.Background(), w.remote, "fix-login", taskpull.Options{Dir: t.TempDir(), NoSession: true})
	if err == nil || !strings.Contains(fmt.Sprint(err), "git repository") {
		t.Fatalf("err = %v", err)
	}
}

// seededWorld is a world whose Task was seeded from the session of the user.
func seededWorld(t *testing.T) *world {
	t.Helper()
	w := newWorld(t)
	early := sessionArchive(t, w.mac, sessionID, []string{line(w.mac, "one")})
	if _, err := (claude.Adapter{}).Import(t.Context(), harness.SessionImport{ConfigDir: w.home, Cwd: w.mac}, bytes.NewReader(early)); err != nil {
		t.Fatal(err)
	}
	if err := w.local.State.MarkHandedOff(sessionID, "fix-login", "sha256:seed", "", time.Now()); err != nil {
		t.Fatal(err)
	}
	commitFile(t, w.worker, "feature.go", "package x\n", "feature")
	w.checkpoint(line("/home/worker/proj", "one"), line("/home/worker/proj", "two"))
	w.remote.plan.SeedDigest = "sha256:seed"
	return w
}

func sessionDirs(t *testing.T, w *world) int {
	t.Helper()
	files, err := filepath.Glob(filepath.Join(w.home, "projects", "*", "*.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	return len(files)
}

func TestPull_ASecondPullOfTheSameTaskKeepsTheSessionId(t *testing.T) {
	w := seededWorld(t)
	if _, err := w.pull(taskpull.Options{}); err != nil {
		t.Fatal(err)
	}
	res, err := w.pull(taskpull.Options{})
	if err != nil {
		t.Fatal(err)
	}
	if res.Session == nil || res.Session.ID != sessionID || res.Session.NewID {
		t.Fatalf("the second pull gave %+v", res.Session)
	}
	if n := sessionDirs(t, w); n != 1 {
		t.Fatalf("%d transcripts after two pulls, want 1", n)
	}
}

func TestPull_ARetryAfterTheControllerWasNotToldKeepsTheSessionId(t *testing.T) {
	w := seededWorld(t)
	w.remote.pulledErr = errors.New("connection reset")
	if _, err := w.pull(taskpull.Options{}); err == nil {
		t.Fatal("the failure of MarkTaskPulled was swallowed")
	}
	res, err := w.pull(taskpull.Options{})
	if err != nil {
		t.Fatal(err)
	}
	if res.Session == nil || res.Session.ID != sessionID || res.Session.NewID {
		t.Fatalf("the retry gave %+v", res.Session)
	}
	if n := sessionDirs(t, w); n != 1 {
		t.Fatalf("%d transcripts after the retry, want 1", n)
	}
}

// What a Run commits cannot run code here: a tracked core.hooksPath is not honoured.
func TestPull_AHookOfTheCheckedOutTreeDoesNotRun(t *testing.T) {
	for name, opts := range map[string]taskpull.Options{"switch": {}, "worktree": {Worktree: "../hooked"}} {
		t.Run(name, func(t *testing.T) {
			w := newWorld(t)
			marker := filepath.Join(t.TempDir(), "ran")
			gitIn(t, w.mac, "config", "core.hooksPath", ".hooks")
			hook := filepath.Join(w.worker, ".hooks", "post-checkout")
			write(t, hook, "#!/bin/sh\ntouch '"+marker+"'\n")
			if err := os.Chmod(hook, 0o700); err != nil { //nolint:gosec // a hook must be executable
				t.Fatal(err)
			}
			gitIn(t, w.worker, "add", "-A")
			gitIn(t, w.worker, "commit", "-q", "-m", "hook")
			w.checkpoint()
			if _, err := w.pull(opts); err != nil {
				t.Fatal(err)
			}
			if _, err := os.Stat(marker); err == nil {
				t.Fatal("the post-checkout hook of the pulled tree ran")
			}
		})
	}
}

// A branch that is at the commit but checked out in another worktree is not switched to:
// the work goes to the next candidate.
func TestPull_ABranchAtTheCommitThatAnotherWorktreeHasCheckedOutIsSkipped(t *testing.T) {
	w := newWorld(t)
	work := commitFile(t, w.worker, "feature.go", "package x\n", "feature")
	w.checkpoint()
	gitIn(t, w.mac, "fetch", "-q", w.worker, "HEAD")
	gitIn(t, w.mac, "branch", "cf/fix-login", work)
	gitIn(t, w.mac, "worktree", "add", "-q", filepath.Join(filepath.Dir(w.mac), "other"), "cf/fix-login")

	res, err := w.pull(taskpull.Options{})
	if err != nil {
		t.Fatal(err)
	}
	if res.Branch != "cf/fix-login@r1" || w.head() != work {
		t.Fatalf("branch %q, head %s", res.Branch, w.head())
	}
}
