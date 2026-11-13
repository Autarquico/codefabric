package handoff

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	v1 "github.com/Autarquico/codefabric/internal/api/v1"
	"github.com/Autarquico/codefabric/internal/client"
	"github.com/Autarquico/codefabric/internal/client/gitctx"
	"github.com/Autarquico/codefabric/internal/client/session"
	"github.com/Autarquico/codefabric/internal/domain"
)

const (
	sessionID = "11111111-2222-4333-8444-555555555555"
	cwd       = "/work/repo"
)

// transcript is a session whose last tool call is the `cf handoff` of /handto, still
// running, followed by what Claude Code writes meanwhile, which must not travel.
func transcript(extra ...string) string {
	lines := []string{
		`{"type":"user","sessionId":"` + sessionID + `","cwd":"` + cwd + `","version":"2.1.319","message":{"role":"user","content":"fix the login redirect, please"}}`,
		`{"type":"assistant","sessionId":"` + sessionID + `","cwd":"` + cwd + `","message":{"role":"assistant","content":[{"type":"text","text":"on it"}]}}`,
		`{"type":"user","sessionId":"` + sessionID + `","cwd":"` + cwd + `","message":{"role":"user","content":"<command-name>/handto</command-name><command-args>w</command-args>"}}`,
		`{"type":"user","isMeta":true,"sessionId":"` + sessionID + `","cwd":"` + cwd + `","message":{"role":"user","content":[{"type":"text","text":"# /handto\n..."}]}}`,
	}
	lines = append(lines, extra...)
	lines = append(lines,
		`{"type":"assistant","sessionId":"`+sessionID+`","cwd":"`+cwd+`","message":{"role":"assistant","content":[{"type":"tool_use","id":"t1","name":"Bash","input":{"command":"cf handoff --session `+sessionID+` --to w"}}]}}`,
		`{"type":"progress","sessionId":"`+sessionID+`","cwd":"`+cwd+`","toolUseID":"t1","data":{"output":"AFTER_THE_CUT"}}`,
	)
	return strings.Join(lines, "\n") + "\n"
}

// machine is a home of Claude Code with the session in it, and the state of cf.
func machine(t *testing.T, content string) *session.Local {
	t.Helper()
	home := t.TempDir()
	dir := filepath.Join(home, "projects", "-work-repo")
	if err := os.MkdirAll(dir, 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, sessionID+".jsonl"), []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	local := session.New(filepath.Join(t.TempDir(), "sessions"))
	local.ConfigDirs = map[string]string{"claude-code": home}
	local.UseProcesses(100, func(pid int) (int, bool) { return map[int]int{100: 50, 50: 1}[pid], pid == 100 || pid == 50 }, func(int) bool { return false })
	return local
}

// openedBy marks the session as open in the Claude Code process pid, with the command
// running two processes below the one that is in parents.
func openedBy(t *testing.T, local *session.Local, pid int, parents map[int]int) {
	t.Helper()
	live := filepath.Join(local.ConfigDirs["claude-code"], "sessions")
	if err := os.MkdirAll(live, 0o750); err != nil {
		t.Fatal(err)
	}
	open := fmt.Sprintf(`{"pid":%d,"sessionId":"%s"}`, pid, sessionID)
	if err := os.WriteFile(filepath.Join(live, fmt.Sprint(pid)+".json"), []byte(open), 0o600); err != nil {
		t.Fatal(err)
	}
	local.UseProcesses(100, func(p int) (int, bool) { parent, ok := parents[p]; return parent, ok }, func(p int) bool { return p == pid })
}

// the Claude Code that runs the command: cf (100) under a shell (50) under claude (4242).
var ownTree = map[int]int{100: 50, 50: 4242, 4242: 1}

type fakeController struct {
	workers []v1.Worker
	tasks   map[string]v1.Task

	puts    map[string][]byte // by digest
	takes   int               // snapshots taken
	created []v1.CreateTaskRequest
	// sent is every creation asked for, replays included.
	sent []v1.CreateTaskRequest
	// byRequest is what a replayed request_id answers; loseResponse makes the next
	// NewTask commit the Task and then fail, as a lost response does.
	byRequest    map[string]v1.Task
	loseResponse bool
	// refuse is what the next NewTask answers instead of creating.
	refuse error
	// runs are the Runs of each Task, by id.
	runs    map[string][]v1.Run
	resumed []v1.ResumeTaskRequest
	// resumes counts the resumes the Controller committed (a replayed request_id is not one);
	// loseResume makes the next one commit and then fail.
	resumes    int
	resumeSeen map[string]bool
	loseResume bool
	// uploads counts PutNativeSeed calls.
	uploads int

	// tree is the tree of the working directory; pushed are the snapshot commits sent.
	tree   string
	pushed []string
}

func newController(workers ...v1.Worker) *fakeController {
	return &fakeController{workers: workers, tasks: map[string]v1.Task{}, puts: map[string][]byte{}}
}

func online(name string) v1.Worker { return v1.Worker{Name: name, State: domain.WorkerOnline} }

func (f *fakeController) Workers(context.Context, bool) ([]v1.Worker, error) {
	return f.workers, nil
}

func (f *fakeController) Runs(_ context.Context, q client.RunQuery) ([]v1.Run, error) {
	return f.runs[q.Task], nil
}

func (f *fakeController) Task(_ context.Context, ref, _ string) (v1.Task, error) {
	return f.tasks[ref], nil
}

func (f *fakeController) ProjectForDir(context.Context, string) (v1.Project, error) {
	return v1.Project{ID: "prj_1"}, nil
}

func (f *fakeController) PutNativeSeed(_ context.Context, _, digest string, r io.Reader, _ int64) (v1.NativeSeedUploaded, error) {
	b, err := io.ReadAll(r)
	f.puts[digest] = b
	f.uploads++
	return v1.NativeSeedUploaded{}, err
}

func (f *fakeController) NewTask(_ context.Context, _ string, req v1.CreateTaskRequest) (v1.CreateTaskResponse, error) {
	f.sent = append(f.sent, req)
	if t, ok := f.byRequest[req.RequestID]; ok && req.RequestID != "" {
		return v1.CreateTaskResponse{Task: t}, nil
	}
	if err := f.refuse; err != nil {
		f.refuse = nil
		return v1.CreateTaskResponse{}, err
	}
	f.created = append(f.created, req)
	if req.SnapshotReport != nil {
		req.SnapshotReport.Tree = f.tree
	}
	// A Task of the tests is one whose Run was deferred, the one a handoff may resume.
	t := v1.Task{ID: fmt.Sprintf("tsk_%d", len(f.created)), Name: req.Name, ProjectID: "prj_1", State: domain.TaskWaiting, WaitingReason: domain.WaitingDeferred}
	f.tasks[t.ID] = t
	if f.byRequest == nil {
		f.byRequest = map[string]v1.Task{}
	}
	f.byRequest[req.RequestID] = t
	if f.loseResponse {
		f.loseResponse = false
		return v1.CreateTaskResponse{}, errors.New("connection reset")
	}
	return v1.CreateTaskResponse{Task: t}, nil
}

func (f *fakeController) ResumeTaskWith(_ context.Context, ref, _ string, req v1.ResumeTaskRequest) (v1.ResumeTaskResponse, error) {
	f.resumed = append(f.resumed, req)
	if f.resumeSeen == nil {
		f.resumeSeen = map[string]bool{}
	}
	if !f.resumeSeen[req.RequestID] || req.RequestID == "" {
		f.resumes++
		f.resumeSeen[req.RequestID] = true
		if f.loseResume {
			f.loseResume = false
			return v1.ResumeTaskResponse{}, errors.New("connection reset")
		}
	}
	return v1.ResumeTaskResponse{Task: f.tasks[ref]}, nil
}

func (f *fakeController) TakeSnapshot(context.Context, string) (gitctx.Snapshot, error) {
	f.takes++
	return gitctx.Snapshot{Commit: strings.Repeat("c", 40), Tree: f.tree, Branch: "feature"}, nil
}

func (f *fakeController) PushSnapshot(_ context.Context, _, _, commit string) error {
	f.pushed = append(f.pushed, commit)
	return nil
}

func archiveText(t *testing.T, archive []byte) string {
	t.Helper()
	gz, err := gzip.NewReader(bytes.NewReader(archive))
	if err != nil {
		t.Fatal(err)
	}
	var all strings.Builder
	tr := tar.NewReader(gz)
	for {
		if _, err := tr.Next(); err != nil {
			return all.String()
		}
		b, _ := io.ReadAll(tr)
		all.Write(b)
	}
}

func request(worker string) Request {
	return Request{SessionID: sessionID, Worker: worker, Dir: "/work/repo"}
}

func TestScenario_Handto(t *testing.T) {
	local, c := machine(t, transcript()), newController(online("w"))
	openedBy(t, local, 4242, ownTree)

	res, err := Do(t.Context(), c, local, request("w"))
	if err != nil {
		t.Fatal(err)
	}

	if len(c.created) != 1 || len(c.resumed) != 0 {
		t.Fatalf("created %d, resumed %d: a session without a Task starts one", len(c.created), len(c.resumed))
	}
	req := c.created[0]
	if c.takes != 1 || req.Snapshot || req.SourceSnapshot != strings.Repeat("c", 40) || req.SourceBranch != "feature" || len(c.pushed) != 1 || req.Mode != domain.ModeInteractive || req.Requirements.Harness != "claude-code" ||
		len(req.Requirements.Workers.Require) != 1 || req.Requirements.Workers.Require[0] != "w" {
		t.Errorf("the Task is not an interactive snapshot Run pinned to w: %+v", req)
	}
	if !strings.Contains(req.Prompt, "fix the login redirect, please") || strings.Contains(req.Prompt, "handto") {
		t.Errorf("the prompt does not come from the last request of the user: %q", req.Prompt)
	}
	if !strings.HasPrefix(req.Name, "handoff-fix-the-login-redirect") || !strings.HasSuffix(req.Name, "-111111") {
		t.Errorf("name %q", req.Name)
	}
	body, ok := c.puts[req.NativeSeed.Digest]
	if !ok {
		t.Fatalf("the seed %v was not uploaded", req.NativeSeed)
	}
	text := archiveText(t, body)
	if !strings.Contains(text, "tool_use") || strings.Contains(text, "AFTER_THE_CUT") {
		t.Errorf("the export is not cut at the last tool_use")
	}
	if res.Worker != "w" || res.Resumed || res.Attach != "cf attach "+req.Name {
		t.Errorf("result %+v", res)
	}
}

// Claude Code had not written the call of cf handoff when it ran (2.1.319, seen
// in the lab): the session has no call, or only earlier ones with their
// results, and goes whole.
func TestScenario_HandtoBeforeTheCallIsOnDisk(t *testing.T) {
	notYet := strings.Join([]string{
		`{"type":"user","sessionId":"` + sessionID + `","cwd":"` + cwd + `","version":"2.1.319","message":{"role":"user","content":"remember ZAFIRO"}}`,
		`{"type":"assistant","sessionId":"` + sessionID + `","cwd":"` + cwd + `","message":{"role":"assistant","content":[{"type":"text","text":"noted"}]}}`,
		`{"type":"user","sessionId":"` + sessionID + `","cwd":"` + cwd + `","message":{"role":"user","content":"<command-name>/handto</command-name><command-args>w</command-args>"}}`,
	}, "\n") + "\n"
	local, c := machine(t, notYet), newController(online("w"))
	openedBy(t, local, 4242, ownTree)

	res, err := Do(t.Context(), c, local, request("w"))
	if err != nil {
		t.Fatal(err)
	}
	text := archiveText(t, c.puts[c.created[0].NativeSeed.Digest])
	if !strings.Contains(text, "ZAFIRO") || !strings.Contains(text, "noted") {
		t.Errorf("the session did not travel whole:\n%s", text)
	}
	if res.Worker != "w" {
		t.Errorf("result %+v", res)
	}
	rec, err := local.State.Get(sessionID)
	if err != nil || rec.HandedOff == nil || rec.HandedOff.Task != "tsk_1" || rec.Task != "tsk_1" {
		t.Errorf("marker and registry: %+v, %v", rec, err)
	}
}

func TestScenario_TurnoEnCurso_TheCallingSessionMayBeOpen(t *testing.T) {
	local := machine(t, transcript())
	openedBy(t, local, 4242, ownTree)
	if _, err := Do(t.Context(), newController(online("w")), local, request("w")); err != nil {
		t.Fatalf("an open session that calls is refused: %v", err)
	}
}

// --session of a session open in another window is not the session that calls: its Claude
// Code is not above the command, so it is refused until it is closed.
func TestScenario_SesionVivaDeOtraVentana(t *testing.T) {
	local, c := machine(t, transcript()), newController(online("w"))
	openedBy(t, local, 4242, map[int]int{100: 50, 50: 777, 777: 1, 4242: 1})

	_, err := Do(t.Context(), c, local, request("w"))
	var live *session.LiveError
	if !errors.As(err, &live) {
		t.Fatalf("a session open elsewhere: %v", err)
	}
	if len(c.created)+len(c.puts) != 0 {
		t.Fatal("something was created or uploaded")
	}
}

func TestScenario_WorkerDesconocido(t *testing.T) {
	local := machine(t, transcript())
	offline := v1.Worker{Name: "foo", State: domain.WorkerOffline}
	for _, name := range []string{"foo", "nobody"} {
		c := newController(online("w"), offline)
		_, err := Do(t.Context(), c, local, request(name))
		var de *domain.Error
		if !errors.As(err, &de) || de.Code != domain.CodeNotFound || !strings.Contains(de.Message, "w (") || !strings.Contains(de.Message, "foo (") {
			t.Errorf("%s: %v", name, err)
		}
		if len(c.created)+len(c.resumed)+len(c.puts) != 0 {
			t.Errorf("%s: something was created or uploaded", name)
		}
	}
	if rec, _ := local.State.Get(sessionID); rec.HandedOff != nil || rec.Task != "" {
		t.Errorf("a failed handoff left %+v", rec)
	}
}

// A Worker degraded by another harness (codex without a login) still takes a
// Claude Code session: the scheduler, not the client, weighs the failure.
func TestScenario_HandtoADegradedWorker(t *testing.T) {
	local, c := machine(t, transcript()), newController(v1.Worker{Name: "w", State: domain.WorkerDegraded})
	openedBy(t, local, 4242, ownTree)
	if _, err := Do(t.Context(), c, local, request("w")); err != nil || len(c.created) != 1 {
		t.Fatalf("created %d: %v", len(c.created), err)
	}
}

func TestScenario_SegundoHandto(t *testing.T) {
	local, c := machine(t, transcript()), newController(online("w"), online("other"))
	if _, err := Do(t.Context(), c, local, request("w")); err != nil {
		t.Fatal(err)
	}

	_, err := Do(t.Context(), c, local, request("other"))
	var handed *session.HandedOffError
	if !errors.As(err, &handed) || handed.Task != "tsk_1" {
		t.Fatalf("a second handoff without --force: %v", err)
	}
	if len(c.created) != 1 || len(c.resumed) != 0 {
		t.Fatal("the refusal created or resumed something")
	}

	forced := request("other")
	forced.Force = true
	res, err := Do(t.Context(), c, local, forced)
	if err != nil {
		t.Fatal(err)
	}
	if len(c.created) != 1 || len(c.resumed) != 1 || !res.Resumed || res.Worker != "other" {
		t.Errorf("with --force it resumes the Task on the other Worker: created %d, resumed %d, %+v", len(c.created), len(c.resumed), res)
	}
	if r := c.resumed[0]; r.Worker != "other" || !r.Force || r.NativeSeed == nil {
		t.Errorf("resume request %+v", r)
	}
}

// The Task the session went to was cancelled: nothing goes on there, and /handto
// starts a new Task without --force (seen in the lab).
func TestScenario_HandtoAfterTheTaskWasCancelled(t *testing.T) {
	local, c := machine(t, transcript()), newController(online("w"))
	if _, err := Do(t.Context(), c, local, request("w")); err != nil {
		t.Fatal(err)
	}
	cancelled := c.tasks["tsk_1"]
	cancelled.State = domain.TaskCancelled
	c.tasks["tsk_1"] = cancelled

	// Its Run is still being stopped: the harness may write, and the guard holds.
	c.runs = map[string][]v1.Run{"tsk_1": {{State: domain.RunRunning}}}
	var handed *session.HandedOffError
	if _, err := Do(t.Context(), c, local, request("w")); !errors.As(err, &handed) {
		t.Fatalf("a Task whose Run is still live freed the session: %v", err)
	}
	c.runs = map[string][]v1.Run{"tsk_1": {{State: domain.RunCancelled}}}

	res, err := Do(t.Context(), c, local, request("w"))
	if err != nil {
		t.Fatal(err)
	}
	if len(c.created) != 2 || len(c.resumed) != 0 || res.Resumed {
		t.Fatalf("created %d, resumed %d: a new Task should start", len(c.created), len(c.resumed))
	}
	if c.created[1].Name == c.created[0].Name {
		t.Errorf("the new Task takes the name the cancelled one keeps: %s", c.created[1].Name)
	}
	if rec, _ := local.State.Get(sessionID); rec.HandedOff == nil || rec.Task == "tsk_1" {
		t.Errorf("the session is not bound to the new Task: %+v", rec)
	}
}

// A creation the Controller refused is not replayed: the answer came.
func TestHandto_ARefusedCreationIsNotReplayed(t *testing.T) {
	local, c := machine(t, transcript()), newController(online("w"))
	openedBy(t, local, 4242, ownTree)
	c.refuse = domain.NewError(domain.CodeInvalidRequest, "a Task named \"x\" already exists in the Project", nil)
	if _, err := Do(t.Context(), c, local, request("w")); err == nil {
		t.Fatal("the refusal was not reported")
	}
	if rec, _ := local.State.Get(sessionID); rec.Pending != nil {
		t.Fatalf("the refused request is still pending: %+v", rec.Pending)
	}
	if _, err := Do(t.Context(), c, local, request("w")); err != nil {
		t.Fatal(err)
	}
	if len(c.sent) != 2 || c.sent[0].RequestID == c.sent[1].RequestID {
		t.Errorf("the retry replayed the refused request: %+v", c.sent)
	}
}

func TestHandto_ASessionWithATaskIsResumedNotRecreated(t *testing.T) {
	local, c := machine(t, transcript()), newController(online("w"))
	c.tasks["tsk_9"] = v1.Task{ID: "tsk_9", Name: "old", ProjectID: "prj_1", State: domain.TaskWaiting, WaitingReason: domain.WaitingDeferred}
	if err := local.State.Link(sessionID, "tsk_9"); err != nil {
		t.Fatal(err)
	}
	res, err := Do(t.Context(), c, local, request("w"))
	if err != nil || !res.Resumed || res.Task != "old" || len(c.created) != 0 || len(c.resumed) != 1 {
		t.Fatalf("%+v, %v, created %d, resumed %d", res, err, len(c.created), len(c.resumed))
	}
}

// A resume carries the tree only if it changed since the last handoff: unchanged, the Run
// continues from the last checkpoint of the Task, with what the Worker did.
func TestHandto_AResumeCarriesASnapshotOnlyIfTheTreeChanged(t *testing.T) {
	local, c := machine(t, transcript()), newController(online("w"))
	c.tree = "tree-1"
	first, err := Do(t.Context(), c, local, request("w"))
	if err != nil || first.Resumed {
		t.Fatalf("%+v, %v", first, err)
	}
	if rec, _ := local.State.Get(sessionID); rec.Tree != "tree-1" {
		t.Fatalf("the registry keeps tree %q", rec.Tree)
	}

	again := request("w")
	again.Force = true
	if _, err := Do(t.Context(), c, local, again); err != nil {
		t.Fatal(err)
	}
	if len(c.resumed) != 1 || c.resumed[0].SourceSnapshot != "" || len(c.pushed) != 1 {
		t.Fatalf("unchanged tree: resume %+v, pushed %v", c.resumed, c.pushed)
	}

	c.tree = "tree-2"
	if _, err := Do(t.Context(), c, local, again); err != nil {
		t.Fatal(err)
	}
	got := c.resumed[1]
	if want := strings.Repeat("c", 40); got.SourceSnapshot != want || got.SourceBranch != "feature" || len(c.pushed) != 2 || c.pushed[1] != want {
		t.Fatalf("changed tree: resume %+v, pushed %v", got, c.pushed)
	}
	if rec, _ := local.State.Get(sessionID); rec.Tree != "tree-2" {
		t.Errorf("the registry keeps tree %q, want tree-2", rec.Tree)
	}
}

func TestScenario_SecretoEnLaSesion(t *testing.T) {
	leak := `{"type":"user","sessionId":"` + sessionID + `","cwd":"` + cwd + `","message":{"role":"user","content":"key AKIAABCDEFGHIJKLMNOP"}}`
	local, c := machine(t, transcript(leak)), newController(online("w"))

	_, err := Do(t.Context(), c, local, request("w"))
	var sensitive *session.SensitiveError
	if !errors.As(err, &sensitive) || len(sensitive.Findings) == 0 {
		t.Fatalf("a session with a secret: %v", err)
	}
	if len(c.created)+len(c.puts) != 0 {
		t.Fatal("the secret was uploaded")
	}

	allowed := request("w")
	allowed.AllowSensitive = true
	if _, err := Do(t.Context(), c, local, allowed); err != nil || len(c.created) != 1 {
		t.Fatalf("--allow-sensitive-session: %v", err)
	}
}

func TestLastUserMessage_LeavesOutWhatTheHarnessWritesForTheUser(t *testing.T) {
	local := machine(t, transcript())
	ref, err := local.Find(t.Context(), sessionID, "")
	if err != nil {
		t.Fatal(err)
	}
	got, err := LastUserMessage("claude-code", ref.Transcript)
	if err != nil || got != "fix the login redirect, please" {
		t.Errorf("%q, %v", got, err)
	}

	rollout := filepath.Join(t.TempDir(), "rollout.jsonl")
	lines := []string{
		`{"type":"response_item","payload":{"type":"message","role":"user","content":[{"type":"input_text","text":"<environment_context>x</environment_context>"}]}}`,
		`{"type":"response_item","payload":{"type":"message","role":"user","content":[{"type":"input_text","text":"add a retry to the uploader"}]}}`,
		`{"type":"response_item","payload":{"type":"message","role":"assistant","content":[{"type":"output_text","text":"ok"}]}}`,
		`{"type":"response_item","payload":{"type":"message","role":"user","content":[{"type":"input_text","text":"$handto omarch02"}]}}`,
	}
	if err := os.WriteFile(rollout, []byte(strings.Join(lines, "\n")), 0o600); err != nil {
		t.Fatal(err)
	}
	if got, err := LastUserMessage("codex", rollout); err != nil || got != "add a retry to the uploader" {
		t.Errorf("codex: %q, %v", got, err)
	}
}

func TestTaskNameAndPrompt_AreValidAndShort(t *testing.T) {
	for _, msg := range []string{"", "¡¿?!", strings.Repeat("palabra ", 100), "Fix the LOGIN.redirect"} {
		name := TaskName(msg, sessionID)
		if !domain.ValidSlug(name) {
			t.Errorf("TaskName(%q) = %q is not a slug", msg, name)
		}
		if n := len([]rune(strings.TrimPrefix(Prompt(msg), "Continue the work of this session where it was left."))); n > 300 {
			t.Errorf("the prompt of %q is %d characters long", msg, n)
		}
	}
}

// A handoff whose response was lost and that is repeated is a replay, not a second Task.
func TestHandto_ARetryAfterALostResponseCreatesNoSecondTask(t *testing.T) {
	local, c := machine(t, transcript()), newController(online("w"))
	c.loseResponse = true
	c.tree = "tree-1"
	if _, err := Do(t.Context(), c, local, request("w")); err == nil {
		t.Fatal("the lost response was not an error")
	}
	c.tree = "tree-2" // a file was edited between the attempts
	// By the retry the session has grown: one more turn and a new tool_use.
	grown := transcript(`{"type":"user","sessionId":"` + sessionID + `","cwd":"` + cwd + `","message":{"role":"user","content":"try again"}}`)
	if err := os.WriteFile(filepath.Join(local.ConfigDirs["claude-code"], "projects", "-work-repo", sessionID+".jsonl"), []byte(grown), 0o600); err != nil {
		t.Fatal(err)
	}
	res, err := Do(t.Context(), c, local, request("w"))
	if err != nil {
		t.Fatal(err)
	}
	if len(c.created) != 1 || res.Task == "" || res.Resumed {
		t.Fatalf("created %d, %+v", len(c.created), res)
	}
	first, retry := c.sent[0], c.sent[len(c.sent)-1]
	if retry.RequestID != first.RequestID || *retry.NativeSeed != *first.NativeSeed || retry.Name != first.Name || retry.Prompt != first.Prompt ||
		retry.SourceSnapshot != first.SourceSnapshot || retry.SourceBranch != first.SourceBranch || c.takes != 1 {
		t.Fatalf("the retry is not the same request:\n%+v\n%+v", first, retry)
	}
	if rec, _ := local.State.Get(sessionID); rec.Task != "tsk_1" || rec.HandedOff == nil ||
		rec.Tree != "tree-1" || rec.Seed != first.NativeSeed.Digest || rec.HandedOff.Digest != first.NativeSeed.Digest || rec.Pending != nil {
		t.Fatalf("record %+v: it must name the seed that went, not the session as it is now", rec)
	}
}

// A handoff that resumes a Task is retried like a creation: the same request, seed and
// snapshot, nothing uploaded again, and the session ends marked.
func TestHandto_AResumeRetryAfterALostResponseIsAReplay(t *testing.T) {
	local, c := machine(t, transcript()), newController(online("w"), online("other"))
	c.tree = "tree-1"
	if _, err := Do(t.Context(), c, local, request("w")); err != nil {
		t.Fatal(err)
	}
	c.tree = "tree-2"
	forced := request("other")
	forced.Force = true
	c.loseResume = true
	if _, err := Do(t.Context(), c, local, forced); err == nil {
		t.Fatal("the lost response was not an error")
	}
	if rec, _ := local.State.Get(sessionID); rec.Pending == nil || rec.Pending.Tree != "tree-2" {
		t.Fatalf("the resume was not remembered before it was sent: %+v", rec.Pending)
	}
	uploads, pushed := c.uploads, len(c.pushed)
	c.tree = "tree-3" // edited between the attempts
	res, err := Do(t.Context(), c, local, forced)
	if err != nil || !res.Resumed {
		t.Fatalf("%+v, %v", res, err)
	}
	first, retry := c.resumed[0], c.resumed[len(c.resumed)-1]
	if first.RequestID == "" || retry.RequestID != first.RequestID || *retry.NativeSeed != *first.NativeSeed ||
		retry.SourceSnapshot != first.SourceSnapshot || retry.SourceBranch != first.SourceBranch {
		t.Fatalf("the retry is not the same request:\n%+v\n%+v", first, retry)
	}
	if c.uploads != uploads || len(c.pushed) != pushed || c.resumes != 1 {
		t.Errorf("a replay uploaded again: uploads %d->%d, pushed %d->%d, resumes %d", uploads, c.uploads, pushed, len(c.pushed), c.resumes)
	}
	if rec, _ := local.State.Get(sessionID); rec.HandedOff == nil || rec.Tree != "tree-2" || rec.Seed != first.NativeSeed.Digest || rec.Pending != nil {
		t.Fatalf("record %+v", rec)
	}
}

// A Task that still runs cannot be resumed: nothing goes up, and the user is pointed at move.
func TestHandto_ARunningTaskIsRefusedBeforeAnythingGoesUp(t *testing.T) {
	local, c := machine(t, transcript()), newController(online("w"), online("other"))
	if _, err := Do(t.Context(), c, local, request("w")); err != nil {
		t.Fatal(err)
	}
	running := c.tasks["tsk_1"]
	running.State, running.WaitingReason = domain.TaskRunning, ""
	c.tasks["tsk_1"] = running
	uploads, pushed := c.uploads, len(c.pushed)

	forced := request("other")
	forced.Force = true
	_, err := Do(t.Context(), c, local, forced)
	var de *domain.Error
	if !errors.As(err, &de) || de.Code != domain.CodeInvalidTransition || !strings.Contains(de.Message, "cf task move") {
		t.Fatalf("got %v", err)
	}
	if c.uploads != uploads || len(c.pushed) != pushed || len(c.resumed) != 0 {
		t.Error("something went up before the refusal")
	}
	if rec, _ := local.State.Get(sessionID); rec.Pending != nil {
		t.Errorf("a refusal left %+v pending", rec.Pending)
	}
}
