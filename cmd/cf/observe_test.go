package main

import (
	"bufio"
	"context"
	"database/sql"
	"encoding/json"
	"os"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	_ "modernc.org/sqlite" // the driver, to write the Controller's database the way retention will

	v1 "github.com/Autarquico/codefabric/internal/api/v1"
	"github.com/Autarquico/codefabric/internal/buildinfo"
	"github.com/Autarquico/codefabric/internal/cli"
	"github.com/Autarquico/codefabric/internal/client"
	"github.com/Autarquico/codefabric/internal/config"
	"github.com/Autarquico/codefabric/internal/controller/daemon"
	"github.com/Autarquico/codefabric/internal/domain"
	workerdaemon "github.com/Autarquico/codefabric/internal/worker/daemon"
)

// spyTerminal records that it was reached.
type spyTerminal struct{ calls atomic.Int32 }

func (s *spyTerminal) Attach(context.Context, cli.AttachRequest, cli.TerminalIO) (int, error) {
	s.calls.Add(1)
	return 0, nil
}

// liveCF is a cf command running under a context, whose output a test reads as it comes.
type liveCF struct {
	out, err *syncBuffer
	done     chan int
	cancel   context.CancelFunc
}

// cfLive starts cf with the environment as it is now: the command reads its
// credentials as it starts, so the test may point the environment elsewhere after.
func cfLive(t *testing.T, args ...string) *liveCF {
	t.Helper()
	ctx, cancel := context.WithCancel(t.Context())
	l := &liveCF{out: &syncBuffer{}, err: &syncBuffer{}, done: make(chan int, 1), cancel: cancel}
	t.Cleanup(cancel)
	go func() {
		l.done <- cli.RunContextWith(ctx, options(), args, strings.NewReader(""), l.out, l.err, daemon.Command(), workerdaemon.Command())
	}()
	return l
}

// stop ends the command as Ctrl-C does, and returns its exit status.
func (l *liveCF) stop(t *testing.T) int {
	t.Helper()
	l.cancel()
	select {
	case code := <-l.done:
		return code
	case <-time.After(10 * time.Second):
		t.Fatal("the command did not stop")
		return -1
	}
}

func (l *liveCF) waitFor(t *testing.T, what string, ok func(out string) bool) {
	t.Helper()
	soon(t, what, func() bool { return ok(l.out.String()) })
}

// workerRun is a Run with an accepted lease on a Worker that this test plays by
// hand, through the SDK with the Worker's own credentials: what the Worker sends
// is exactly what a test says.
type workerRun struct {
	host   config.Paths
	worker *client.Client
	run    v1.Run
	assign v1.AssignCommand
}

func newWorkerRun(t *testing.T, host config.Paths) *workerRun {
	t.Helper()
	invite := workerInvite(t, host)
	box := machine(t)
	ok[map[string]any](t, "worker", "join", invite, "--name", "alpha")
	heartbeatOnce(t, box)
	w, err := client.Open(box.WorkerCredentialsDir())
	if err != nil {
		t.Fatal(err)
	}
	use(t, host)
	newOrigin(t).clone(t)
	ok[v1.Project](t, "project", "create", "backend")
	created := ok[v1.CreateTaskResponse](t, "do", "x", "--name", "auth-race")
	r := &workerRun{host: host, worker: w, run: *created.Run}
	soon(t, "the Run to be assigned", func() bool {
		page, err := w.Assignments(t.Context(), 0, time.Second, 0)
		if err != nil || len(page.Commands) == 0 || page.Commands[0].Assign == nil {
			return false
		}
		r.assign = *page.Commands[0].Assign
		return true
	})
	if _, err := w.AcceptLease(t.Context(), r.assign.LeaseID, r.assign.Epoch); err != nil {
		t.Fatal(err)
	}
	return r
}

func (r *workerRun) upload(t *testing.T, offset uint64, text string) {
	t.Helper()
	out, err := r.worker.UploadOutput(t.Context(), v1.OutputBatch{Entries: []v1.OutputEntry{
		{RunID: r.run.ID, LeaseEpoch: r.assign.Epoch, Offset: offset, Data: []byte(text)},
	}})
	if err != nil || out.Acks[0].Code != "" {
		t.Fatalf("upload: %v %+v", err, out)
	}
}

func (r *workerRun) eof(t *testing.T, final uint64) {
	t.Helper()
	out, err := r.worker.UploadOutput(t.Context(), v1.OutputBatch{Entries: []v1.OutputEntry{
		{RunID: r.run.ID, LeaseEpoch: r.assign.Epoch, EOF: true, FinalOffset: &final},
	}})
	if err != nil || out.Acks[0].Code != "" {
		t.Fatalf("eof: %v %+v", err, out)
	}
}

func TestScenario_ObserveAttachEvents_Headless(t *testing.T) {
	host, _ := bringUpController(t)
	use(t, host)
	newOrigin(t).clone(t)
	ok[v1.Project](t, "project", "create", "backend")
	ok[v1.CreateTaskResponse](t, "do", "echo hi", "--name", "auth-race")

	spy := &spyTerminal{}
	var out, errOut strings.Builder
	code := cli.RunContextWith(t.Context(), cli.Options{Terminal: spy}, []string{"attach", "auth-race", "--json"},
		strings.NewReader(""), &out, &errOut, daemon.Command())
	env := errorOf(t, out.String())
	details, _ := env["details"].(map[string]any)
	if code != 8 || env["code"] != string(domain.CodeRunNotAttachable) || details["reason"] != "headless" || spy.calls.Load() != 0 || errOut.String() != "" {
		t.Fatalf("exit %d, %v, terminal reached %d times, stderr %q", code, env, spy.calls.Load(), errOut.String())
	}
	fails(t, 8, domain.CodeRunNotAttachable, "attach", "run-1")
	fails(t, 3, domain.CodeNotFound, "attach", "no-such-task")
}

func TestObserveAttachEvents_AttachChecksTheTargetTheOverrideAndTheTerminalBeforeRunningAnything(t *testing.T) {
	host, _ := bringUpController(t)
	r := newWorkerRun(t, host)

	// The Worker announces how to reach it, and the Run has a live session.
	_, err := r.worker.Heartbeat(t.Context(), v1.HeartbeatRequest{
		Epoch: "01J00000000000000000000000", DaemonVersion: buildinfo.Get().Version, Hostname: "box", OS: "linux", Arch: "amd64",
		Total: v1.Capacity{CPU: 16, MemoryMB: 65536, DiskMB: 500000}, Health: v1.Health{Status: v1.HealthOK},
		SSHTarget: "tester@worker-box:2222", AttachBinaries: map[string]string{"tmux": "/usr/bin/tmux"},
	})
	if err != nil {
		t.Fatal(err)
	}
	handle, _ := json.Marshal(v1.RuntimeHandle{Runtime: "tmux", Ref: "cf-0123456789ab/" + domain.SessionName(r.run.ID, r.assign.Epoch)})
	db, err := sql.Open("sqlite", host.DatabaseFile())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	if _, err := db.ExecContext(t.Context(), `PRAGMA busy_timeout = 5000`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(t.Context(), `UPDATE runs SET runtime = 'active', runtime_handle = ?, spec = json_set(spec, '$.mode', 'interactive') WHERE id = ?`, string(handle), r.run.ID); err != nil {
		t.Fatal(err)
	}

	use(t, host)
	spy := &spyTerminal{}
	attach := func(args ...string) (int, map[string]any) {
		var out, errOut strings.Builder
		code := cli.RunContextWith(t.Context(), cli.Options{Terminal: spy}, append([]string{"attach"}, append(args, "--json")...), strings.NewReader(""), &out, &errOut, daemon.Command())
		return code, errorOf(t, out.String())
	}
	// Everything checks out, and then there is no terminal on stdin: nothing is run.
	if code, env := attach("auth-race"); code != 2 || env["code"] != string(domain.CodeInvalidRequest) || !strings.Contains(env["message"].(string), "terminal") { //nolint:forcetypeassert // a message
		t.Fatalf("no tty: exit %d %v", code, env)
	}
	// The person's override must itself be a target; the announcement of the Worker never reaches a command line unchecked.
	for _, bad := range []string{"-oProxyCommand=touch pwned", "a;b", "u@h name"} {
		code, env := attach("auth-race", "--ssh-target", bad)
		details, _ := env["details"].(map[string]any)
		if code != 8 || env["code"] != string(domain.CodeRunNotAttachable) || details["reason"] != "invalid_ssh_target" {
			t.Errorf("--ssh-target %q: exit %d %v", bad, code, env)
		}
	}
	// So must the map in client.json.
	clientJSON := host.ClientConfigFile()
	if err := os.WriteFile(clientJSON, []byte(`{"attach":{"ssh_targets":{"alpha":"-oProxyCommand=x"}}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if code, env := attach("auth-race"); code != 2 || env["code"] != string(domain.CodeInvalidRequest) {
		t.Errorf("a bad client.json: exit %d %v", code, env)
	}
	if err := os.WriteFile(clientJSON, []byte(`{"attach":{"ssh_targets":{"alpha":"me@trusted:22"}}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if code, env := attach("auth-race"); code != 2 || env["code"] != string(domain.CodeInvalidRequest) { // reaches the terminal check with the override in force
		t.Errorf("client.json override: exit %d %v", code, env)
	}
	if spy.calls.Load() != 0 {
		t.Fatalf("the Terminal was reached %d times without a terminal", spy.calls.Load())
	}
	// A Run that is not the one asked for.
	fails(t, 3, domain.CodeNotFound, "attach", "run-99")
}

func TestScenario_ObserveAttachEvents_Ps(t *testing.T) {
	host, _, _ := initController(t)
	cfg, err := config.LoadController(host)
	if err != nil {
		t.Fatal(err)
	}
	cfg.WorkerOfflineTTL = config.Duration(time.Second)
	if err := config.SaveController(host, cfg); err != nil {
		t.Fatal(err)
	}
	serve(t, host, daemon.Options{})
	r := newWorkerRun(t, host)

	use(t, host)
	ps := ok[v1.PsResponse](t, "ps")
	if len(ps.Runs) != 1 {
		t.Fatalf("ps = %+v", ps)
	}
	entry := ps.Runs[0]
	if entry.ID != r.run.ID || entry.WorkerReachable == nil || !*entry.WorkerReachable || entry.Runtime != domain.RuntimeNone ||
		entry.Worker == nil || entry.Worker.Name != "alpha" || entry.Task.Name != "auth-race" || entry.Attachable {
		t.Fatalf("entry = %+v", entry)
	}
	raw := cfJSON(t, "ps")
	for _, field := range []string{`"worker_reachable":true`, `"runtime":"none"`, `"mode":"headless"`} {
		if !strings.Contains(raw, field) {
			t.Errorf("cf ps --json lacks %s:\n%s", field, raw)
		}
	}
	code, human, _ := cf(t, "", "ps")
	if code != 0 || !strings.Contains(human, "REACHABLE") || !strings.Contains(human, "alpha") || !strings.Contains(human, "auth-race") {
		t.Errorf("cf ps: exit %d\n%s", code, human)
	}

	// The Worker goes quiet: it is offline within a few seconds, and the Run says so.
	soon(t, "the Worker to be offline", func() bool {
		ps := ok[v1.PsResponse](t, "ps")
		return len(ps.Runs) == 1 && ps.Runs[0].WorkerReachable != nil && !*ps.Runs[0].WorkerReachable
	})
	if ps = ok[v1.PsResponse](t, "ps", "--project", "backend"); ps.Runs[0].NotAttachable == "" {
		t.Errorf("an offline Worker's Run is attachable: %+v", ps.Runs[0])
	}
	fails(t, 3, domain.CodeNotFound, "ps", "--project", "nowhere")
}

func cfJSON(t *testing.T, args ...string) string {
	t.Helper()
	code, out, errOut := cf(t, "", append(args, "--json")...)
	if code != 0 {
		t.Fatalf("cf %v: exit %d: %s", args, code, errOut)
	}
	return out
}

// percentile95 is the sample under which 95% of the others are.
func percentile95(samples []time.Duration) time.Duration {
	sorted := slices.Clone(samples)
	slices.Sort(sorted)
	return sorted[min(len(sorted)-1, (len(sorted)*95+99)/100-1)]
}

func TestObserveAttachEvents_PsAnswersInUnder300msAtP95With100Runs(t *testing.T) {
	host, _ := bringUpController(t)
	use(t, host)
	newOrigin(t).clone(t)
	ok[v1.Project](t, "project", "create", "backend")
	admin, err := client.Open(host.CredentialsDir())
	if err != nil {
		t.Fatal(err)
	}
	commit := strings.TrimSpace(git(t, ".", "rev-parse", "HEAD"))
	for i := range 100 {
		_, err := admin.CreateTask(t.Context(), v1.CreateTaskRequest{
			Project: "backend", Name: "t" + string(rune('a'+i/26)) + string(rune('a'+i%26)), Prompt: "x", ResolvedRef: "main", SourceCommit: commit,
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	// Warm-up, then 50 timed requests, as a person would make them.
	for range 5 {
		if _, err := admin.Ps(t.Context(), ""); err != nil {
			t.Fatal(err)
		}
	}
	var samples []time.Duration
	for range 50 {
		start := time.Now()
		ps, err := admin.Ps(t.Context(), "")
		samples = append(samples, time.Since(start))
		if err != nil || len(ps.Runs) != 100 {
			t.Fatalf("ps = %d Runs, %v", len(ps.Runs), err)
		}
	}
	if p95 := percentile95(samples); p95 > 300*time.Millisecond {
		t.Errorf("ps p95 = %s, want under 300ms (§1.3)", p95)
	}
}

func TestObserveAttachEvents_EventsPagesFiltersAndFollowAsJSONLines(t *testing.T) {
	host, _ := bringUpController(t)
	use(t, host)
	newOrigin(t).clone(t)
	ok[v1.Project](t, "project", "create", "backend")
	ok[v1.CreateTaskResponse](t, "do", "x", "--name", "first")

	page := ok[v1.EventPage](t, "events")
	if len(page.Events) < 2 || page.HasMore {
		t.Fatalf("tail = %+v", page)
	}
	if one := ok[v1.EventPage](t, "events", "--limit", "1"); len(one.Events) != 1 || one.Events[0].Sequence != page.Events[len(page.Events)-1].Sequence {
		t.Fatalf("--limit 1 is the last event: %+v", one)
	}
	if from := ok[v1.EventPage](t, "events", "--after", "1", "--limit", "500"); len(from.Events) != len(page.Events)-1 {
		t.Fatalf("--after 1 = %d events of %d", len(from.Events), len(page.Events))
	}
	if filtered := ok[v1.EventPage](t, "events", "--type", "RunQueued", "--task", "first"); len(filtered.Events) != 1 || filtered.Events[0].Type != "RunQueued" {
		t.Fatalf("filtered = %+v", filtered)
	}
	if none := ok[v1.EventPage](t, "events", "--level", "error", "--since", "1h"); len(none.Events) != 0 {
		t.Fatalf("error events: %+v", none)
	}
	code, human, _ := cf(t, "", "events")
	if code != 0 || !strings.Contains(human, "TaskCreated") || !strings.Contains(human, "RunQueued") {
		t.Errorf("cf events: exit %d\n%s", code, human)
	}
	fails(t, 3, domain.CodeNotFound, "events", "--run", "run-99")
	fails(t, 2, domain.CodeInvalidRequest, "events", "--level", "loud")
	fails(t, 4, domain.CodeCursorExpired, "events", "--after", "999999")

	// Follow: the tail, then what happens, one JSON value per line.
	live := cfLive(t, "events", "--follow", "--json")
	live.waitFor(t, "the tail", func(out string) bool { return strings.Count(out, "\n") >= len(page.Events) })
	ok[v1.CreateTaskResponse](t, "do", "y", "--name", "second")
	live.waitFor(t, "a new event", func(out string) bool { return strings.Count(out, `"type":"RunQueued"`) >= 2 })
	var last uint64
	sc := bufio.NewScanner(strings.NewReader(live.out.String()))
	for sc.Scan() {
		var e v1.Event
		if err := json.Unmarshal(sc.Bytes(), &e); err != nil {
			t.Fatalf("line %q: %v", sc.Text(), err)
		}
		if e.Sequence <= last {
			t.Fatalf("events out of order or repeated at %d", e.Sequence)
		}
		last = e.Sequence
	}
	if code := live.stop(t); code != 0 {
		t.Errorf("Ctrl-C: exit %d: %s", code, live.err.String())
	}
}

func TestObserveAttachEvents_AnExpiredCursorInFollowIsSaidOnStderrAndFollowGoesOn(t *testing.T) {
	host, _ := bringUpController(t)
	use(t, host)
	newOrigin(t).clone(t)
	ok[v1.Project](t, "project", "create", "backend")
	for _, name := range []string{"a", "b", "c"} {
		ok[v1.CreateTaskResponse](t, "do", "x", "--name", name)
	}
	// The retention of Phase 4 will do this: events below a sequence are gone.
	db, err := sql.Open("sqlite", host.DatabaseFile())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	if _, err := db.ExecContext(t.Context(), `PRAGMA busy_timeout = 5000`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(t.Context(), `INSERT INTO controller_meta (key, value) VALUES ('events_purged_through', '5')`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(t.Context(), `DELETE FROM events WHERE sequence <= 5`); err != nil {
		t.Fatal(err)
	}

	live := cfLive(t, "events", "--follow", "--after", "2", "--json")
	soon(t, "the warning", func() bool { return strings.Contains(live.err.String(), "were purged") })
	if !strings.Contains(live.err.String(), "between 2 and 5") {
		t.Errorf("stderr = %q", live.err.String())
	}
	ok[v1.CreateTaskResponse](t, "do", "z", "--name", "after-the-loss")
	live.waitFor(t, "an event after the loss", func(out string) bool { return strings.Contains(out, "after-the-loss") })
	if code := live.stop(t); code != 0 {
		t.Errorf("exit %d", code)
	}
}

func TestObserveAttachEvents_LogsTailThenFollowDeliversEachByteOnceAndEndsAtTheEOF(t *testing.T) {
	host, _ := bringUpController(t)
	r := newWorkerRun(t, host)
	use(t, host)
	r.upload(t, 0, "one\ntwo\nthree\n")

	code, out, _ := cf(t, "", "logs", "--run", r.run.Short, "--tail", "2")
	if code != 0 || out != "two\nthree\n" {
		t.Fatalf("tail: exit %d %q", code, out)
	}
	if page := ok[v1.RunOutput](t, "logs", "--run", r.run.Short, "--tail", "1"); string(page.Data) != "three\n" || page.End != 14 {
		t.Fatalf("--json = %+v", page)
	}

	live := cfLive(t, "logs", "--run", r.run.Short, "--follow", "--tail", "1")
	live.waitFor(t, "the tail", func(out string) bool { return out == "three\n" })
	r.upload(t, 14, "four\n")
	live.waitFor(t, "live output", func(out string) bool { return out == "three\nfour\n" })
	// Output a Run printed to attack the terminal is not passed on.
	r.upload(t, 19, "\x1b]0;pwned\x07\x1b[31mred\x1b[0m\r\x00 plain\n")
	live.waitFor(t, "the sanitised line", func(out string) bool { return strings.HasSuffix(out, "red plain\n") })
	if strings.ContainsAny(live.out.String(), "\x1b\x07\r\x00") {
		t.Fatalf("control characters reached the terminal: %q", live.out.String())
	}
	r.eof(t, 19+uint64(len("\x1b]0;pwned\x07\x1b[31mred\x1b[0m\r\x00 plain\n")))
	select {
	case code := <-live.done:
		if code != 0 {
			t.Fatalf("exit %d: %s", code, live.err.String())
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the follow did not end at the EOF")
	}
	if got := live.out.String(); got != "three\nfour\nred plain\n" {
		t.Fatalf("followed %q", got)
	}

	// --json --follow is JSON Lines: the tail first, then the chunks.
	jl := cfLive(t, "logs", "--run", r.run.Short, "--follow", "--json", "--tail", "2")
	if code := <-jl.done; code != 0 {
		t.Fatalf("json follow: exit %d: %s", code, jl.err.String())
	}
	var chunks []v1.OutputChunk
	sc := bufio.NewScanner(strings.NewReader(jl.out.String()))
	for sc.Scan() {
		var ch v1.OutputChunk
		if err := json.Unmarshal(sc.Bytes(), &ch); err != nil {
			t.Fatalf("line %q: %v", sc.Text(), err)
		}
		chunks = append(chunks, ch)
	}
	if len(chunks) == 0 || chunks[0].RunID != r.run.ID || chunks[0].LeaseEpoch != r.assign.Epoch ||
		!strings.Contains(string(chunks[0].Data), "plain") ||
		chunks[0].Offset+uint64(len(chunks[0].Data)) != 19+uint64(len("\x1b]0;pwned\x07\x1b[31mred\x1b[0m\r\x00 plain\n")) {
		t.Fatalf("the first line is not the tail as a chunk: %+v", chunks)
	}

	raw := cfLive(t, "logs", "--run", r.run.Short, "--raw", "--tail", "1")
	if code := <-raw.done; code != 0 || !strings.Contains(raw.out.String(), "\x1b[31mred") {
		t.Fatalf("--raw: exit %d %q", code, raw.out.String())
	}
}

// soon polls until ok holds.
func soon(t *testing.T, what string, ok func() bool) {
	t.Helper()
	for deadline := time.Now().Add(30 * time.Second); !ok(); time.Sleep(20 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
	}
}
