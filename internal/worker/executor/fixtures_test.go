//go:build linux || darwin

package executor

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	cfruntime "github.com/Autarquico/codefabric/internal/adapters/runtime"

	"github.com/Autarquico/codefabric/internal/adapters/harness"
	"github.com/Autarquico/codefabric/internal/adapters/harness/command"
	"github.com/Autarquico/codefabric/internal/adapters/harness/localhost"
	v1 "github.com/Autarquico/codefabric/internal/api/v1"
	"github.com/Autarquico/codefabric/internal/config"
	"github.com/Autarquico/codefabric/internal/domain"
	"github.com/Autarquico/codefabric/internal/support/clock"
	"github.com/Autarquico/codefabric/internal/worker/execwrap"
	"github.com/Autarquico/codefabric/internal/worker/journal"
	"github.com/Autarquico/codefabric/internal/worker/procident"
	"github.com/Autarquico/codefabric/internal/worker/state"
)

var t0 = time.Date(2026, 10, 10, 12, 0, 0, 0, time.UTC)

// fakeController answers the executor's calls from tables a test fills in.
type fakeController struct {
	mu       sync.Mutex
	pages    []v1.AssignmentsResponse
	accept   func(lease string) error
	reject   func(lease string, req v1.RejectLeaseRequest) error
	accepted []string
	rejected []v1.RejectLeaseRequest
	acks     []uint64
	tokens   []string
	after    []uint64
	// relayToken, if set, answers RelayToken.
	relayToken func(run string, epoch uint64) (v1.RelayTokenResponse, error)
	blocked    chan struct{}
	// reconcile, if set, answers Reconcile; by default every run goes on and the round is complete.
	reconcile  func(req v1.ReconcileRequest) (v1.ReconcileResponse, error)
	reconciles []v1.ReconcileRequest
	// orphanRef, if set, is the ref OrphanToken answers with.
	orphanRef string
	// grant, if set, answers GrantSecrets; by default every ref is granted.
	grant  func(run string, req v1.GrantSecretsRequest) (v1.GrantSecretsResponse, error)
	grants []v1.GrantSecretsRequest
}

func (c *fakeController) GrantSecrets(_ context.Context, run string, req v1.GrantSecretsRequest) (v1.GrantSecretsResponse, error) {
	c.mu.Lock()
	c.grants = append(c.grants, req)
	answer := c.grant
	c.mu.Unlock()
	if answer != nil {
		return answer(run, req)
	}
	return v1.GrantSecretsResponse{Granted: append([]string{}, req.Refs...)}, nil
}

func (c *fakeController) grantRequests() []v1.GrantSecretsRequest {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]v1.GrantSecretsRequest{}, c.grants...)
}

func (c *fakeController) Reconcile(_ context.Context, req v1.ReconcileRequest) (v1.ReconcileResponse, error) {
	c.mu.Lock()
	c.reconciles = append(c.reconciles, req)
	answer := c.reconcile
	c.mu.Unlock()
	if answer != nil {
		return answer(req)
	}
	out := v1.ReconcileResponse{Ack: v1.ReconcileAck{Complete: true}}
	for _, r := range req.Runs {
		out.Verdicts = append(out.Verdicts, v1.ReconcileVerdict{RunID: r.RunID, LeaseEpoch: r.LeaseEpoch, Verdict: v1.ReconcileContinue})
	}
	return out, nil
}

func (c *fakeController) Assignments(ctx context.Context, after uint64, _ time.Duration, _ int) (v1.AssignmentsResponse, error) {
	c.mu.Lock()
	c.after = append(c.after, after)
	if len(c.pages) > 0 {
		p := c.pages[0]
		c.pages = c.pages[1:]
		c.mu.Unlock()
		return p, nil
	}
	blocked := c.blocked
	c.mu.Unlock()
	if blocked == nil {
		<-ctx.Done()
	} else {
		select {
		case <-ctx.Done():
		case <-blocked:
		}
	}
	return v1.AssignmentsResponse{}, ctx.Err()
}

func (c *fakeController) AcceptLease(_ context.Context, lease string, _ uint64) (v1.Lease, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.accept != nil {
		if err := c.accept(lease); err != nil {
			return v1.Lease{}, err
		}
	}
	c.accepted = append(c.accepted, lease)
	return v1.Lease{ID: lease, State: domain.LeaseAccepted}, nil
}

func (c *fakeController) RejectLease(_ context.Context, lease string, req v1.RejectLeaseRequest) (v1.Lease, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.reject != nil {
		if err := c.reject(lease, req); err != nil {
			return v1.Lease{}, err
		}
	}
	c.rejected = append(c.rejected, req)
	return v1.Lease{ID: lease, State: domain.LeaseRejected}, nil
}

func (c *fakeController) RelayToken(_ context.Context, run string, epoch uint64) (v1.RelayTokenResponse, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.tokens = append(c.tokens, fmt.Sprintf("%s-e%d", run, epoch))
	if c.relayToken != nil {
		return c.relayToken(run, epoch)
	}
	return v1.RelayTokenResponse{Token: "token-" + run, Path: "/v1/relay/project_1.git", Write: true}, nil
}

func (c *fakeController) OrphanToken(_ context.Context, run string, epoch uint64, commit string) (v1.RelayTokenResponse, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.tokens = append(c.tokens, fmt.Sprintf("orphan:%s-e%d:%s", run, epoch, commit))
	ref := domain.OrphanRef(domain.ID(run), epoch)
	if c.orphanRef != "" {
		ref = c.orphanRef
	}
	return v1.RelayTokenResponse{Token: "orphan-" + run, Path: "/v1/relay/project_1.git", Write: true, Ref: ref}, nil
}

func (c *fakeController) RelayURL(path string) string { return "https://controller.test" + path }

func (c *fakeController) AckCommands(_ context.Context, upTo uint64) (v1.AckCommandsResponse, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.acks = append(c.acks, upTo)
	return v1.AckCommandsResponse{AckedThrough: upTo}, nil
}

// fakeRuntime keeps sessions in memory and counts what it is asked.
type fakeRuntime struct {
	mu       sync.Mutex
	sessions map[string]bool
	starts   []cfruntime.Spec
	wired    []cfruntime.Spec
	stops    []string
	startErr error
}

func newFakeRuntime() *fakeRuntime { return &fakeRuntime{sessions: map[string]bool{}} }

func (r *fakeRuntime) HandleFor(name string) cfruntime.Handle {
	return cfruntime.Handle{Runtime: "fake", Ref: "sock/" + name}
}

func (r *fakeRuntime) Start(_ context.Context, spec cfruntime.Spec) (cfruntime.Handle, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.starts = append(r.starts, spec)
	if r.startErr != nil {
		return cfruntime.Handle{}, r.startErr
	}
	r.sessions[spec.Name] = true
	return r.HandleFor(spec.Name), nil
}

func (r *fakeRuntime) Wire(_ context.Context, spec cfruntime.Spec) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.wired = append(r.wired, spec)
	return nil
}

func (r *fakeRuntime) Stop(_ context.Context, h cfruntime.Handle) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	name := h.Ref[len("sock/"):]
	r.stops = append(r.stops, name)
	delete(r.sessions, name)
	return nil
}

func (r *fakeRuntime) Status(_ context.Context, h cfruntime.Handle) (cfruntime.Status, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return cfruntime.Status{Alive: r.sessions[h.Ref[len("sock/"):]]}, nil
}

func (r *fakeRuntime) List(context.Context) ([]string, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []string
	for name, alive := range r.sessions {
		if alive {
			out = append(out, name)
		}
	}
	sort.Strings(out)
	return out, nil
}

func (r *fakeRuntime) startCount() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.starts)
}

func (r *fakeRuntime) setAlive(name string, alive bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if alive {
		r.sessions[name] = true
	} else {
		delete(r.sessions, name)
	}
}

type fixture struct {
	t      *testing.T
	db     *state.DB
	clock  *clock.Fake
	ctrl   *fakeController
	rt     *fakeRuntime
	j      *journal.Journal
	e      *Executor
	paths  config.Paths
	opts   Options
	nextID uint64
}

const testEpoch = "01J0000000000000000000000E1"

func newFixture(t *testing.T, mutate ...func(*Options)) *fixture {
	t.Helper()
	root := t.TempDir()
	f := &fixture{
		t: t, clock: clock.NewFake(t0), ctrl: &fakeController{}, rt: newFakeRuntime(),
		paths: config.Paths{ConfigDir: filepath.Join(root, "config"), DataDir: filepath.Join(root, "data")},
	}
	db, err := state.Open(t.Context(), f.paths.WorkerStateFile())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	f.db = db
	f.j = journal.New(testEpoch, f.clock)
	f.opts = Options{
		DB: db, Controller: f.ctrl, Runtime: f.rt, Journal: f.j, Harnesses: registry(t, command.Adapter{}), Host: testHost(t, root),
		Paths: f.paths, Clock: f.clock, Log: slog.New(slog.DiscardHandler), WrapperArgv: []string{"/wrapper", "worker", "exec"}, StopGrace: 2 * time.Second,
	}
	for _, m := range mutate {
		m(&f.opts)
	}
	f.e = f.build()
	f.e.SetLimits(Limits{MaxConcurrentRuns: 4, CPU: 16, MemoryMB: 65536})
	return f
}

func registry(t *testing.T, adapters ...harness.Adapter) *harness.Registry {
	t.Helper()
	reg, err := harness.NewRegistry(adapters...)
	if err != nil {
		t.Fatal(err)
	}
	return reg
}

// testHost is a Host whose home is a directory of the test.
func testHost(t *testing.T, root string) harness.Host {
	t.Helper()
	home := filepath.Join(root, "home")
	if err := os.MkdirAll(home, 0o700); err != nil {
		t.Fatal(err)
	}
	h, err := localhost.New(localhost.Options{Home: home, Env: []string{"PATH=/usr/bin:/bin"}})
	if err != nil {
		t.Fatal(err)
	}
	return h
}

func (f *fixture) build() *Executor {
	f.t.Helper()
	e, err := New(f.opts)
	if err != nil {
		f.t.Fatal(err)
	}
	// The tests that drive the steps by hand stand for a Worker that reconciled.
	e.reconciledGen.Store(e.reconcileGen.Load())
	return e
}

// restart is a new daemon: a new executor, and a new journal epoch, over the same state.
func (f *fixture) restart(epoch string) {
	f.t.Helper()
	f.j = journal.New(epoch, f.clock)
	f.opts.Journal = f.j
	f.rt.mu.Lock()
	f.rt.starts = nil
	f.rt.mu.Unlock()
	f.e = f.build()
	f.e.SetLimits(Limits{MaxConcurrentRuns: 4, CPU: 16, MemoryMB: 65536})
}

func specJSON(prompt, harness, mode string) []byte {
	raw, _ := json.Marshal(map[string]any{
		"schema": 2, "prompt": prompt, "mode": mode,
		"requirements": map[string]any{"harness": harness},
	})
	return raw
}

// assign gives the executor an assign order, as the queue would.
func (f *fixture) assign(spec []byte, mutate ...func(*v1.AssignCommand)) state.Key {
	f.t.Helper()
	f.nextID++
	a := v1.AssignCommand{
		LeaseID: fmt.Sprintf("lease_%d", f.nextID), RunID: runID(f.nextID), RunShort: fmt.Sprintf("run-%d", f.nextID),
		TaskID: "task_1", ProjectID: "project_1", Epoch: 1, AcceptBy: t0.Add(time.Minute), SourceCommit: "abc",
		Branch: fmt.Sprintf("cf/t/run-%d", f.nextID), Repo: "/srv/r",
		Resources: v1.Capacity{CPU: 2, MemoryMB: 4096}, Spec: spec,
	}
	for _, m := range mutate {
		m(&a)
	}
	blocked, err := f.e.persist(f.t.Context(), []v1.WorkerCommand{{Seq: f.nextID, Type: domain.CommandAssign, Assign: &a}})
	if err != nil || blocked {
		f.t.Fatalf("persist: blocked=%v err=%v", blocked, err)
	}
	return state.Key{RunID: a.RunID, LeaseEpoch: a.Epoch}
}

// accepted leaves a new assignment in phase accepted, as after a good accept.
func (f *fixture) accepted(prompt string) state.Key {
	f.t.Helper()
	k := f.assign(specJSON(prompt, "command", "headless"))
	if f.e.decideOnce(f.t.Context()) {
		f.t.Fatal("deciding wanted a retry")
	}
	if got := f.run(k).Phase; got != state.PhaseAccepted {
		f.t.Fatalf("phase = %s, want accepted", got)
	}
	return k
}

// started takes an accepted assignment to running through the real steps.
func (f *fixture) started(prompt string) state.Key {
	f.t.Helper()
	k := f.accepted(prompt)
	if f.e.step(f.t.Context(), k) {
		f.t.Fatal("the Run ended at its first step")
	}
	if got := f.run(k).Phase; got != state.PhaseRunning {
		f.t.Fatalf("phase = %s, want running", got)
	}
	return k
}

func (f *fixture) run(k state.Key) state.Run {
	f.t.Helper()
	r, err := state.RunBy(f.t.Context(), f.db.Reader(), k)
	if err != nil {
		f.t.Fatal(err)
	}
	return r
}

// reports are the unconfirmed reports of a Run, in order, as the sender would send them.
func (f *fixture) reports(k state.Key) []v1.RunReportEntry {
	f.t.Helper()
	rows, err := state.PendingJournal(f.t.Context(), f.db.Reader(), 1000)
	if err != nil {
		f.t.Fatal(err)
	}
	var out []v1.RunReportEntry
	for _, r := range rows {
		if r.Key != k {
			continue
		}
		var e v1.RunReportEntry
		if err := json.Unmarshal(r.Payload, &e); err != nil {
			f.t.Fatal(err)
		}
		e.WorkerEpoch, e.WorkerSeq = r.Epoch, r.Seq
		out = append(out, e)
	}
	return out
}

func kinds(entries []v1.RunReportEntry) []v1.RunReportKind {
	out := make([]v1.RunReportKind, len(entries))
	for i, e := range entries {
		out[i] = e.Kind
	}
	return out
}

func (f *fixture) runDir(k state.Key) string { return f.paths.WorkerRunDir(k.RunID, k.LeaseEpoch) }

// helper is a real process standing in for a wrapper: it has a group of its
// own, and its identity can be written to wrapper.json.
type helper struct {
	cmd   *exec.Cmd
	w     execwrap.Wrapper
	ended chan struct{}
}

// newWrapperProcess starts `sh -c script` as the leader of its own group.
func newWrapperProcess(t *testing.T, script string) *helper {
	t.Helper()
	// The script waits on its input until the identity is taken, so that one that
	// ends at once (`exit 0`) cannot be gone before it is registered.
	input, hold, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.CommandContext(t.Context(), "/bin/sh", "-c", "read _go; "+script) //nolint:gosec // the script of a test
	cmd.Stdin = input
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	_ = input.Close()
	start, err := procident.StartOf(cmd.Process.Pid)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := hold.WriteString("go\n"); err != nil {
		t.Fatal(err)
	}
	_ = hold.Close()
	h := &helper{cmd: cmd, ended: make(chan struct{}), w: execwrap.Wrapper{PID: cmd.Process.Pid, PGID: cmd.Process.Pid, Start: start, StartedAt: t0}}
	go func() {
		_ = cmd.Wait()
		close(h.ended)
	}()
	t.Cleanup(func() {
		_ = syscall.Kill(-h.w.PGID, syscall.SIGKILL)
		<-h.ended
	})
	return h
}

// newTrappingWrapperProcess is newWrapperProcess for a script that starts with
// a trap: it returns once the trap is in place, so a signal sent at once is
// not taken before the shell installed it.
func newTrappingWrapperProcess(t *testing.T, trap, body string) *helper {
	t.Helper()
	ready := filepath.Join(t.TempDir(), "ready")
	h := newWrapperProcess(t, trap+"; : > "+ready+"; "+body)
	deadline := time.Now().Add(10 * time.Second)
	for {
		if _, err := os.Stat(ready); err == nil {
			return h
		}
		if time.Now().After(deadline) {
			t.Fatal("the trap was not installed")
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func (h *helper) alive() bool {
	select {
	case <-h.ended:
		return false
	default:
		return true
	}
}

func (f *fixture) writeWrapper(k state.Key, w execwrap.Wrapper) {
	f.t.Helper()
	if err := os.MkdirAll(f.runDir(k), 0o700); err != nil {
		f.t.Fatal(err)
	}
	if err := execwrap.Write(f.runDir(k), execwrap.WrapperFile, w); err != nil {
		f.t.Fatal(err)
	}
}

func (f *fixture) writeStarted(k state.Key, w execwrap.Wrapper, at time.Time) {
	f.t.Helper()
	if err := execwrap.Write(f.runDir(k), execwrap.StartedFile, execwrap.Started{PID: w.PID + 1, PGID: w.PGID, WrapperPID: w.PID, Argv: []string{"x"}, StartedAt: at}); err != nil {
		f.t.Fatal(err)
	}
}

func (f *fixture) writeExit(k state.Key, e execwrap.Exit) {
	f.t.Helper()
	if err := os.MkdirAll(f.runDir(k), 0o700); err != nil {
		f.t.Fatal(err)
	}
	if err := execwrap.Write(f.runDir(k), execwrap.ExitFile, e); err != nil {
		f.t.Fatal(err)
	}
}

func intp(n int) *int { return &n }

func contextWithCancel(t *testing.T) (context.Context, context.CancelFunc) {
	t.Helper()
	return context.WithCancel(t.Context())
}

func waitFor(t *testing.T, what string, ok func() bool) {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		if ok() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

// groupSize is the number of live processes in a group.
func groupSize(pgid int) int {
	members, _ := procident.GroupMembers(pgid)
	return len(members)
}

// runID is a valid Run ID for the n-th Run of a test.
func runID(n uint64) string { return fmt.Sprintf("run_01J0000000000000000000%04d", n) }

// syncLog is a log destination that tests can read while the executor writes.
type syncLog struct {
	mu  sync.Mutex
	buf strings.Builder
}

func (l *syncLog) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.buf.Write(p)
}

func (l *syncLog) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.buf.String()
}
