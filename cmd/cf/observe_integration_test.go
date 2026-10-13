//go:build integration && (linux || darwin)

package main

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"math/rand/v2"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/Autarquico/codefabric/internal/adapters/runtime/tmux"
	v1 "github.com/Autarquico/codefabric/internal/api/v1"
	"github.com/Autarquico/codefabric/internal/cli"
	"github.com/Autarquico/codefabric/internal/config"
	"github.com/Autarquico/codefabric/internal/controller/daemon"
	"github.com/Autarquico/codefabric/internal/domain"
	workerdaemon "github.com/Autarquico/codefabric/internal/worker/daemon"
)

// The scenarios of attach, events and output against a real Controller, a real
// Worker daemon and real tmux. Two things are not real, and the tests that use
// them say so: the ssh in the PATH of the attach is a script (a composition test,
// not proof that attach works over a network), and the terminal that `cf attach`
// needs is given by a second tmux, of the test's own.

const fakeSSHScript = `#!/bin/sh
# A stand-in for OpenSSH that logs its arguments, insists on the shape cf must
# give it (options, then --, then the destination and one command), and runs the
# command with a shell as the remote side would.
printf '%s\n' "$@" >> "$FAKE_SSH_LOG"
if [ -n "$FAKE_SSH_EXIT" ]; then
  echo "ssh: fake failure" >&2
  exit "$FAKE_SSH_EXIT"
fi
while [ $# -gt 0 ]; do
  case "$1" in
    --) shift; break ;;
    -t) shift ;;
    -o|-p) shift 2 ;;
    *) echo "fake ssh: unexpected option $1" >&2; exit 97 ;;
  esac
done
[ $# -eq 2 ] || { echo "fake ssh: want a destination and one command, got $#" >&2; exit 98; }
shift
unset TMUX TMUX_PANE
exec /bin/sh -c "$1"
`

// observeBox is a Worker with a live interactive Run, and a client machine.
type observeBox struct {
	*reconcileBox
	client   config.Paths
	run      v1.Run
	worktree string
	session  string
	fakeBin  string
	sshLog   string
}

const holdScript = `touch ready
read line
echo "$line" > typed.txt
echo started-line
n=0
while [ ! -e go ]; do
  n=$((n+1)); [ "$n" -gt 2400 ] && exit 99
  sleep 0.05
done
echo finished-line`

func newObserveBox(t *testing.T, script string, extraDaemonArgs ...string) *observeBox {
	t.Helper()
	host, clientInvite := bringUpController(t)
	b := newReconcileBox(t, host)
	laptop := machine(t)
	if code, _, errOut := cf(t, clientInvite+"\n", "connect", "-", "--name", "laptop"); code != 0 {
		t.Fatalf("connect: %s", errOut)
	}
	use(t, host)
	newOrigin(t).clone(t)
	smallProject(t)
	b.startWith(append([]string{"--ssh-target", "tester@fake-worker:2222"}, extraDaemonArgs...)...)
	created := ok[v1.CreateTaskResponse](t, "do", script, "--harness", "command", "-i", "--name", "auth-race")
	b.waitRunning(created.Run.ID)

	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "ssh"), []byte(fakeSSHScript), 0o700); err != nil { //nolint:gosec // a test script
		t.Fatal(err)
	}
	return &observeBox{
		reconcileBox: b, client: laptop, run: *created.Run, worktree: b.box.WorkerWorktreeDir(created.Run.ID, 1),
		session: domain.SessionName(created.Run.ID, 1), fakeBin: dir, sshLog: filepath.Join(dir, "ssh.log"),
	}
}

// workerTmux runs a tmux command against the server of the Worker.
func (o *observeBox) workerTmux(args ...string) (string, error) {
	full := append([]string{"-L", tmux.SocketName(o.box.WorkerDataDir()), "-f", "/dev/null"}, args...)
	out, err := exec.CommandContext(context.WithoutCancel(o.t.Context()), "tmux", full...).Output() //nolint:gosec // tmux of the test
	return strings.TrimSpace(string(out)), err
}

func (o *observeBox) clients() int {
	out, err := o.workerTmux("list-clients", "-t", "="+o.session, "-F", "#{client_name}")
	if err != nil || out == "" {
		return 0
	}
	return len(strings.Split(out, "\n"))
}

func (o *observeBox) sessionAlive() bool {
	_, err := o.workerTmux("has-session", "-t", "="+o.session)
	return err == nil
}

// outer is a tmux of the test that gives a command the terminal it needs.
type outer struct {
	t    *testing.T
	sock string
}

// inTerminal runs the command in a pane of a new tmux server. The environment of
// the command is explicit.
func inTerminal(t *testing.T, env []string, argv ...string) *outer {
	t.Helper()
	o := &outer{t: t, sock: fmt.Sprintf("cf-client-%d", rand.IntN(1<<30))} //nolint:gosec // a name
	args := append([]string{"-L", o.sock, "-f", "/dev/null", "new-session", "-d", "-x", "200", "-y", "50"}, argv...)
	cmd := exec.CommandContext(context.WithoutCancel(t.Context()), "tmux", args...) //nolint:gosec // tmux of the test
	cmd.Env = append(os.Environ(), env...)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("outer tmux: %v: %s", err, out)
	}
	t.Cleanup(o.kill)
	return o
}

func (o *outer) tmux(args ...string) string {
	out, _ := exec.CommandContext(context.WithoutCancel(o.t.Context()), "tmux", append([]string{"-L", o.sock, "-f", "/dev/null"}, args...)...).Output() //nolint:gosec // tmux of the test
	return strings.TrimSpace(string(out))
}

func (o *outer) kill() {
	o.tmux("kill-server")
	// tmux can leave the socket file of a server that is gone.
	dir := os.Getenv("TMUX_TMPDIR")
	if dir == "" {
		dir = "/tmp"
	}
	_ = os.Remove(filepath.Join(dir, fmt.Sprintf("tmux-%d", os.Getuid()), o.sock)) //nolint:gosec // the socket of this test's own tmux
}

func (o *outer) send(keys ...string) { o.tmux(append([]string{"send-keys", "-t", "0"}, keys...)...) }

func (o *outer) panePID() int {
	n, _ := strconv.Atoi(o.tmux("list-panes", "-F", "#{pane_pid}"))
	return n
}

// attachEnv is the environment of `cf attach` as a client on the laptop, with the fake ssh first in the PATH.
func (o *observeBox) attachEnv(extra ...string) []string {
	return append([]string{
		asCF + "=1", "CF_CONFIG_DIR=" + o.client.ConfigDir, "CF_DATA_DIR=" + o.client.DataDir,
		"PATH=" + o.fakeBin + ":" + os.Getenv("PATH"), "FAKE_SSH_LOG=" + o.sshLog, "TERM=xterm-256color",
	}, extra...)
}

func (o *observeBox) attach(extraEnv []string, args ...string) *outer {
	exe, err := os.Executable()
	if err != nil {
		o.t.Fatal(err)
	}
	return inTerminal(o.t, o.attachEnv(extraEnv...), append([]string{exe, "attach", "auth-race"}, args...)...)
}

func readFile(path string) string {
	raw, _ := os.ReadFile(path) //nolint:gosec // a file of the test
	return strings.TrimSpace(string(raw))
}

// TestScenario_ObserveAttachEvents_Attach: cf attach opens the session of an
// interactive Run on the Worker, from another machine; closing it does not end the Run.
// The ssh is a script that runs the remote command with a shell, so this proves
// the quoting, the argv and the tmux session of the composition, not a network.
func TestScenario_ObserveAttachEvents_Attach(t *testing.T) {
	o := newObserveBox(t, holdScript)

	use(t, o.client)
	listed := ok[v1.PsResponse](t, "ps")
	if len(listed.Runs) != 1 || !listed.Runs[0].Attachable || listed.Runs[0].WorkerReachable == nil || !*listed.Runs[0].WorkerReachable {
		t.Fatalf("ps = %+v", listed)
	}

	terminal := o.attach(nil)
	eventually(t, "the attach client to be on the session of the Worker", func() bool { return o.clients() == 1 })

	// What cf asked ssh to run.
	tmuxBin, err := exec.LookPath("tmux")
	if err != nil {
		t.Fatal(err)
	}
	wantCommand := fmt.Sprintf("'%s' '-L' '%s' '-f' '/dev/null' 'attach-session' '-t' '=%s'", tmuxBin, tmux.SocketName(o.box.WorkerDataDir()), o.session)
	logged := strings.Split(strings.TrimSpace(readFile(o.sshLog)), "\n")
	wantArgs := []string{"-t", "-o", "ConnectTimeout=15", "-o", "ForwardAgent=no", "-o", "ForwardX11=no", "-o", "ClearAllForwardings=yes", "-o", "PermitLocalCommand=no", "-p", "2222", "--", "tester@fake-worker", wantCommand}
	if !slices.Equal(logged, wantArgs) {
		t.Fatalf("ssh was run with\n%q\nwant\n%q", logged, wantArgs)
	}

	// What the person types reaches the harness.
	terminal.send("typed over attach", "Enter")
	eventually(t, "the harness to read the line", func() bool { return readFile(filepath.Join(o.worktree, "typed.txt")) == "typed over attach" })

	// Closing the terminal ends the attach and nothing else.
	terminal.kill()
	eventually(t, "the client to leave the session", func() bool { return o.clients() == 0 })
	if !o.sessionAlive() {
		t.Fatal("closing the attach ended the session")
	}
	if r := o.run.ID; o.reconcileBox.run(r).State != domain.RunRunning || fileExists(filepath.Join(o.worktree, "go")) {
		t.Fatalf("the Run did not go on: %+v", o.reconcileBox.run(r))
	}
	o.release(o.run.ID)
	r := o.waitPublished(o.run.ID)
	if r.State != domain.RunCompleted {
		t.Errorf("Run = %+v", r)
	}
}

// What the person types in a read-only attach does not reach the harness.
func TestObserveAttachEvents_ReadOnlyAttachDoesNotSendInput(t *testing.T) {
	o := newObserveBox(t, holdScript)
	typed := filepath.Join(o.worktree, "typed.txt")

	readOnly := o.attach(nil, "--read-only")
	eventually(t, "the client", func() bool { return o.clients() == 1 })
	if !strings.Contains(readFile(o.sshLog), "'-r'") {
		t.Fatalf("ssh was not asked for -r:\n%s", readFile(o.sshLog))
	}
	readOnly.send("nope", "Enter")
	time.Sleep(1500 * time.Millisecond)
	if fileExists(typed) {
		t.Fatalf("a read-only attach sent input: %q", readFile(typed))
	}
	readOnly.kill()
	eventually(t, "the client to leave", func() bool { return o.clients() == 0 })

	writable := o.attach(nil)
	eventually(t, "the client", func() bool { return o.clients() == 1 })
	writable.send("yes", "Enter")
	eventually(t, "the input to arrive", func() bool { return readFile(typed) == "yes" })
	writable.kill()
	o.release(o.run.ID)
	o.waitPublished(o.run.ID)
}

// A transport that fails is said to have failed; the person is not told the Run is gone.
func TestObserveAttachEvents_SSHFailureIsReported(t *testing.T) {
	o := newObserveBox(t, holdScript)
	out := filepath.Join(t.TempDir(), "out")
	exe, _ := os.Executable()
	script := fmt.Sprintf("%s attach auth-race --json > %s; echo $? > %s.code", exe, out, out)
	inTerminal(t, o.attachEnv("FAKE_SSH_EXIT=255"), "sh", "-c", script)
	eventually(t, "cf attach to end", func() bool { return fileExists(out + ".code") })
	if code := readFile(out + ".code"); code != "8" {
		t.Fatalf("exit %s, output %s", code, readFile(out))
	}
	var env struct {
		Error struct {
			Code    domain.Code    `json:"code"`
			Details map[string]any `json:"details"`
		} `json:"error"`
	}
	if err := json.Unmarshal([]byte(readFile(out)), &env); err != nil || env.Error.Code != domain.CodeRunNotAttachable || env.Error.Details["reason"] != "ssh_failed" {
		t.Fatalf("output %q: %v", readFile(out), err)
	}
	if !o.sessionAlive() {
		t.Fatal("the session is gone")
	}
	if _, err := o.workerTmux("send-keys", "-t", "="+o.session+":", "go on", "Enter"); err != nil { // the harness waits for a line
		t.Fatal(err)
	}
	o.release(o.run.ID)
	o.waitPublished(o.run.ID)
}

// cfProcess is `cf` in a process of its own, as a person's terminal would have it.
type cfProcess struct {
	cmd  *exec.Cmd
	out  *syncBuffer
	done chan struct{}
}

func startCFProcess(t *testing.T, paths config.Paths, args ...string) *cfProcess {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	p := &cfProcess{out: &syncBuffer{}, done: make(chan struct{})}
	p.cmd = exec.CommandContext(context.WithoutCancel(t.Context()), exe, args...) //nolint:gosec // this very test binary
	p.cmd.Env = append(os.Environ(), asCF+"=1", "CF_CONFIG_DIR="+paths.ConfigDir, "CF_DATA_DIR="+paths.DataDir)
	p.cmd.Stdout, p.cmd.Stderr = p.out, p.out
	if err := p.cmd.Start(); err != nil {
		t.Fatal(err)
	}
	go func() { _ = p.cmd.Wait(); close(p.done) }()
	t.Cleanup(func() { _ = p.cmd.Process.Kill(); <-p.done })
	return p
}

func (p *cfProcess) sigkill() {
	_ = p.cmd.Process.Signal(syscall.SIGKILL)
	<-p.done
}

// sequences are the sequences of the JSON Lines of an events follow.
func sequences(t *testing.T, out string) []uint64 {
	t.Helper()
	var seqs []uint64
	sc := bufio.NewScanner(strings.NewReader(out))
	for sc.Scan() {
		var e v1.Event
		if json.Unmarshal(sc.Bytes(), &e) == nil && e.Sequence > 0 {
			seqs = append(seqs, e.Sequence)
		}
	}
	return seqs
}

// TestScenario_ObserveAttachEvents_ClienteCerrado (e2e #2): the client machine
// closes, whatever it was doing, and the Run goes on and finishes; a new follow
// from the cursor it had loses no event.
func TestScenario_ObserveAttachEvents_ClienteCerrado(t *testing.T) {
	o := newObserveBox(t, holdScript)
	use(t, o.client)

	terminal := o.attach(nil)
	eventually(t, "attach", func() bool { return o.clients() == 1 })
	terminal.send("hello", "Enter")
	events := startCFProcess(t, o.client, "events", "--follow", "--json")
	logs := startCFProcess(t, o.client, "logs", "--run", o.run.Short, "--follow")
	eventually(t, "the output of the Run to reach the follow", func() bool { return strings.Contains(logs.out.String(), "started-line") })
	eventually(t, "the event follow to be open", func() bool { return len(sequences(t, events.out.String())) > 0 })

	// The Mac closes: every client process dies without a word.
	panePID := terminal.panePID()
	events.sigkill()
	logs.sigkill()
	if panePID > 0 {
		_ = syscall.Kill(panePID, syscall.SIGKILL)
	}
	terminal.kill()
	cursor := slices.Max(sequences(t, events.out.String()))

	eventually(t, "the attach client to be gone", func() bool { return o.clients() == 0 })
	if !o.sessionAlive() || o.reconcileBox.run(o.run.ID).State != domain.RunRunning {
		t.Fatal("the Run did not survive its clients")
	}
	if fileExists(filepath.Join(o.box.WorkerRunDir(o.run.ID, 1), "exit.json")) {
		t.Fatal("the harness ended with its clients")
	}

	// Nobody is watching; the harness is released and the Run finishes and publishes.
	o.release(o.run.ID)
	r := o.waitPublished(o.run.ID)
	if r.State != domain.RunCompleted {
		t.Errorf("Run = %+v", r)
	}

	// A follow from the cursor of the dead one loses nothing.
	use(t, o.client)
	want := ok[v1.EventPage](t, "events", "--after", strconv.FormatUint(cursor, 10), "--limit", "500")
	if len(want.Events) == 0 {
		t.Fatal("no events after the cursor")
	}
	live := cfLive(t, "events", "--follow", "--json", "--after", strconv.FormatUint(cursor, 10))
	live.waitFor(t, "every event after the cursor", func(out string) bool {
		got := sequences(t, out)
		return len(got) >= len(want.Events) && got[len(want.Events)-1] == want.Events[len(want.Events)-1].Sequence
	})
	got := sequences(t, live.out.String())[:len(want.Events)]
	for i, e := range want.Events {
		if got[i] != e.Sequence {
			t.Fatalf("followed %v, want %v", got, sequences(t, eventsJSONLines(want)))
		}
	}
	live.stop(t)
	types := map[domain.EventType]bool{}
	for _, e := range want.Events {
		types[e.Type] = true
	}
	if !types[domain.EventRunCompleted] || !types[domain.EventRunPublished] {
		t.Errorf("the events after the cursor lack the end of the Run: %v", types)
	}
	// Its output is whole, too.
	code, out, _ := cf(t, "", "logs", "--run", o.run.Short)
	if code != 0 || !strings.Contains(out, "started-line") || !strings.Contains(out, "finished-line") {
		t.Errorf("logs: exit %d\n%s", code, out)
	}
}

func eventsJSONLines(p v1.EventPage) string {
	var b strings.Builder
	for _, e := range p.Events {
		raw, _ := json.Marshal(e)
		b.Write(raw)
		b.WriteByte('\n')
	}
	return b.String()
}

// TestObserveAttachEvents_LogsRunFollow: a line the harness prints reaches
// `cf logs --run --follow` in under 2 s, and the follow ends when the output does.
func TestObserveAttachEvents_LogsRunFollow(t *testing.T) {
	host, _ := bringUpController(t)
	b := newReconcileBox(t, host)
	use(t, host)
	newOrigin(t).clone(t)
	smallProject(t)
	b.start()
	script := `touch ready
while [ ! -e go ]; do sleep 0.05; done
printf 'hello-from-the-harness\n'
while [ ! -e done ]; do sleep 0.05; done
printf 'bye\n'`
	created := b.running(script)
	run := created.Run

	live := cfLive(t, "logs", "--run", run.Short, "--follow")
	time.Sleep(500 * time.Millisecond) // the follow is open: it has nothing to show yet
	start := time.Now()
	if err := writeFile(filepath.Join(b.box.WorkerWorktreeDir(run.ID, 1), "go")); err != nil {
		t.Fatal(err)
	}
	live.waitFor(t, "the line", func(out string) bool { return strings.Contains(out, "hello-from-the-harness") })
	if took := time.Since(start); took > 2*time.Second {
		t.Errorf("the line took %s to reach the follow, want under 2s", took)
	}
	if err := writeFile(filepath.Join(b.box.WorkerWorktreeDir(run.ID, 1), "done")); err != nil {
		t.Fatal(err)
	}
	select {
	case code := <-live.done:
		if code != 0 || !strings.HasSuffix(live.out.String(), "bye\n") {
			t.Fatalf("exit %d, output %q, stderr %q", code, live.out.String(), live.err.String())
		}
	case <-time.After(30 * time.Second):
		t.Fatalf("the follow did not end with the output: %q", live.out.String())
	}
	b.waitPublished(run.ID)
}

// lineStamps records when each line of a command's output arrived.
type lineStamps struct {
	mu    sync.Mutex
	buf   []byte
	lines []stamped
}

type stamped struct {
	line string
	at   time.Time
}

func (s *lineStamps) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.buf = append(s.buf, p...)
	for {
		i := slices.Index(s.buf, '\n')
		if i < 0 {
			return len(p), nil
		}
		s.lines = append(s.lines, stamped{line: string(s.buf[:i]), at: time.Now()})
		s.buf = s.buf[i+1:]
	}
}

// received is the event of the type for the Run, as it arrived.
func (s *lineStamps) received(run string, typ domain.EventType) (v1.Event, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, l := range s.lines {
		var e v1.Event
		if json.Unmarshal([]byte(l.line), &e) == nil && e.RunID == run && e.Type == typ {
			return e, true
		}
	}
	return v1.Event{}, false
}

// arrivalOfTask is when the first event line about the Task arrived.
func (s *lineStamps) arrivalOfTask(task string) (time.Time, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, l := range s.lines {
		var e v1.Event
		if json.Unmarshal([]byte(l.line), &e) == nil && e.TaskID == task {
			return l.at, true
		}
	}
	return time.Time{}, false
}

// arrival is when the first event line of the type for the Run arrived.
func (s *lineStamps) arrival(run string, typ domain.EventType) (time.Time, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, l := range s.lines {
		var e v1.Event
		if json.Unmarshal([]byte(l.line), &e) == nil && e.RunID == run && e.Type == typ {
			return l.at, true
		}
	}
	return time.Time{}, false
}

// TestScenario_ObserveAttachEvents_Latencia: an event that a real Worker emits
// appears in `cf events --follow` in under 2 s, p95 of 50 samples. The Worker is
// the daemon, the event is the end of a harness it supervises, and the clock
// starts before the local action that causes it (releasing the harness).
func TestScenario_ObserveAttachEvents_Latencia(t *testing.T) {
	host, _ := bringUpController(t)
	b := newReconcileBox(t, host)
	use(t, host)
	newOrigin(t).clone(t)
	smallProject(t)
	b.start()

	stamps := &lineStamps{}
	ctx, cancel := context.WithCancel(t.Context())
	followDone := make(chan int, 1)
	t.Cleanup(cancel)
	go func() {
		followDone <- cli.RunContextWith(ctx, options(), []string{"events", "--follow", "--json", "--type", "RunCompleted"},
			strings.NewReader(""), stamps, &syncBuffer{}, daemon.Command(), workerdaemon.Command())
	}()
	script := `touch ready
while [ ! -e go ]; do sleep 0.02; done`

	var follow, worker, exited, reported []time.Duration // the share of the Controller to the follow, and of everything before it

	// batch makes n Runs that wait at the barrier, releases them, and measures each.
	batch := func(n int) []time.Duration {
		t.Helper()
		var runs []string
		for range n {
			created := ok[v1.CreateTaskResponse](t, "do", script, "--harness", "command")
			runs = append(runs, created.Run.ID)
		}
		for _, id := range runs {
			b.waitRunning(id)
		}
		started := map[string]time.Time{}
		for _, id := range runs {
			started[id] = time.Now() // before the local action
			if err := writeFile(filepath.Join(b.box.WorkerWorktreeDir(id, 1), "go")); err != nil {
				t.Fatal(err)
			}
		}
		var samples []time.Duration
		for _, id := range runs {
			var at time.Time
			eventually(t, "the event of "+id, func() bool { var ok bool; at, ok = stamps.arrival(id, domain.EventRunCompleted); return ok })
			samples = append(samples, at.Sub(started[id]))
			if e, ok := stamps.received(id, domain.EventRunCompleted); ok {
				follow = append(follow, at.Sub(e.ReceivedAt))
				worker = append(worker, e.ReceivedAt.Sub(started[id]))
				exited = append(exited, e.Timestamp.Sub(started[id]))
				reported = append(reported, e.ReceivedAt.Sub(e.Timestamp))
			}
		}
		return samples
	}

	batch(1) // the sentinel: the follow is open and delivers events; not measured
	const total, per = 50, 3
	var samples []time.Duration
	for len(samples) < total {
		samples = append(samples, batch(min(per, total-len(samples)))...)
	}
	slices.Sort(samples)
	p95 := percentile95(samples)
	slices.Sort(follow)
	slices.Sort(worker)
	t.Logf("from the commit of the event to the line: median %s, max %s; from the local action to the commit: median %s, max %s",
		follow[len(follow)/2], follow[len(follow)-1], worker[len(worker)/2], worker[len(worker)-1])
	slices.Sort(exited)
	slices.Sort(reported)
	t.Logf("harness released → exit marker written: median %s, max %s; exit marker → committed at the Controller: median %s, max %s",
		exited[len(exited)/2], exited[len(exited)-1], reported[len(reported)/2], reported[len(reported)-1])
	t.Logf("%d samples: min %s, median %s, p95 %s, max %s", len(samples), samples[0], samples[len(samples)/2], p95, samples[len(samples)-1])
	if p95 >= 2*time.Second {
		t.Errorf("p95 = %s, want under 2s", p95)
	}
	cancel()
	<-followDone
}
