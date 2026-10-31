//go:build linux || darwin

package daemon_test

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	cfruntime "github.com/Autarquico/codefabric/internal/adapters/runtime"

	"github.com/Autarquico/codefabric/internal/adapters/harness"
	v1 "github.com/Autarquico/codefabric/internal/api/v1"
	"github.com/Autarquico/codefabric/internal/buildinfo"
	"github.com/Autarquico/codefabric/internal/client"
	controllerdaemon "github.com/Autarquico/codefabric/internal/controller/daemon"
	"github.com/Autarquico/codefabric/internal/domain"
	"github.com/Autarquico/codefabric/internal/support/clock"
	"github.com/Autarquico/codefabric/internal/testutil/fakeharness"
	"github.com/Autarquico/codefabric/internal/worker/daemon"
	"github.com/Autarquico/codefabric/internal/worker/state"
)

// recordingRuntime is a Runtime that only remembers that it was asked to start.
type recordingRuntime struct {
	mu     sync.Mutex
	starts int
}

func (r *recordingRuntime) Start(context.Context, cfruntime.Spec) (cfruntime.Handle, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.starts++
	return cfruntime.Handle{Runtime: "fake", Ref: "x/y"}, nil
}
func (r *recordingRuntime) Wire(context.Context, cfruntime.Spec) error   { return nil }
func (r *recordingRuntime) Stop(context.Context, cfruntime.Handle) error { return nil }
func (r *recordingRuntime) Status(context.Context, cfruntime.Handle) (cfruntime.Status, error) {
	return cfruntime.Status{}, nil
}

func (r *recordingRuntime) List(context.Context) ([]string, error) { return nil, nil }

func (r *recordingRuntime) HandleFor(name string) cfruntime.Handle {
	return cfruntime.Handle{Runtime: "fake", Ref: "x/" + name}
}

func (r *recordingRuntime) count() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.starts
}

const commit = "0123456789abcdef0123456789abcdef01234567"

// TestScenario_WorkerExecutorTmux_CrashAntesDelAccept: a Worker that died after
// it persisted an assignment and before it answered finds, when it comes back,
// that the Controller's lease expired. The Controller is real, on a clock the
// test moves; the answer to the accept is a real CF_STALE_LEASE.
func TestScenario_WorkerExecutorTmux_CrashAntesDelAccept(t *testing.T) {
	clk := clock.NewFake(time.Now())
	c := newController(t, func(o *controllerdaemon.Options) { o.Clock = clk })
	paths := machine(t)
	c.join("box", paths)
	ctx := t.Context()

	// The Worker is online and offers the harness; the Run is assigned to it.
	wc, err := client.Open(paths.WorkerCredentialsDir())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := wc.Heartbeat(ctx, v1.HeartbeatRequest{
		Epoch: "01J00000000000000000000000", DaemonVersion: buildinfo.Get().Version, Hostname: "box", OS: "linux", Arch: "amd64",
		Capabilities: []string{"harness:command", domain.RepoConfigCapability}, Total: v1.Capacity{CPU: 8, MemoryMB: 16384, DiskMB: 100000}, Health: v1.Health{Status: v1.HealthOK},
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := c.owner.CreateProject(ctx, v1.CreateProjectRequest{Name: "backend", Repo: "/srv/r"}); err != nil {
		t.Fatal(err)
	}
	created, err := c.owner.CreateTask(ctx, v1.CreateTaskRequest{
		Project: "backend", Prompt: "echo hi", ResolvedRef: "main", SourceCommit: commit,
		Requirements: v1.Requirements{Harness: "command"},
	})
	if err != nil {
		t.Fatal(err)
	}
	var assign *v1.AssignCommand
	var seq uint64
	deadline := time.Now().Add(10 * time.Second)
	for assign == nil {
		page, err := wc.Assignments(ctx, 0, 0, 0)
		if err != nil {
			t.Fatal(err)
		}
		if len(page.Commands) == 1 {
			assign, seq = page.Commands[0].Assign, page.Commands[0].Seq
		} else if time.Now().After(deadline) {
			t.Fatal("the Run was never assigned")
		} else {
			time.Sleep(10 * time.Millisecond)
		}
	}

	// What the daemon persisted before it died: the assignment, not answered.
	db, err := state.Open(ctx, paths.WorkerStateFile())
	if err != nil {
		t.Fatal(err)
	}
	key := state.Key{RunID: assign.RunID, LeaseEpoch: assign.Epoch}
	if err := db.WithTx(ctx, func(tx *state.Tx) error {
		if _, err := state.InsertAssigned(ctx, tx, state.Run{
			Key: key, LeaseID: assign.LeaseID, RunShort: assign.RunShort, TaskID: assign.TaskID, ProjectID: assign.ProjectID,
			CommandSeq: seq, AcceptBy: assign.AcceptBy, SourceCommit: assign.SourceCommit,
			CPU: assign.Resources.CPU, MemoryMB: assign.Resources.MemoryMB, Spec: assign.Spec, Harness: "command", Mode: "headless", CreatedAt: clk.Now(),
		}); err != nil {
			return err
		}
		return state.SetCursor(ctx, tx, seq)
	}); err != nil {
		t.Fatal(err)
	}
	_ = db.Close()

	// The lease expires while the Worker is down.
	clk.Advance(2 * domain.Defaults().LeaseAcceptBy)

	rt := &recordingRuntime{}
	out := &logs{}
	running := run(t, paths, out, rt)
	defer func() { _ = running.stop() }()

	deadline = time.Now().Add(15 * time.Second)
	for {
		check, err := state.Open(ctx, paths.WorkerStateFile())
		if err != nil {
			t.Fatal(err)
		}
		got, err := state.RunBy(ctx, check.Reader(), key)
		_ = check.Close()
		if err == nil && got.Phase == state.PhaseDiscarded {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the assignment was never discarded: %+v (%v)", got, err)
		}
		time.Sleep(20 * time.Millisecond)
	}
	if !strings.Contains(out.String(), "CF_STALE_LEASE") {
		t.Errorf("the Controller did not answer CF_STALE_LEASE:\n%s", out.String())
	}
	if n := rt.count(); n != 0 {
		t.Errorf("Start was called %d times for a lease the Controller had expired", n)
	}
	got, err := c.owner.Run(ctx, created.Run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.State != domain.RunQueued {
		t.Errorf("the Run is %s in the Controller", got.State)
	}
}

func TestRun_OffersTheCommandHarnessWhenItHasARuntime(t *testing.T) {
	withoutCodex(t)
	c := newController(t)
	paths := machine(t)
	c.join("box", paths)
	run(t, paths, &logs{}, &recordingRuntime{})
	w := c.eventually("box", "the harness to be offered", func(w v1.Worker) bool {
		return w.State == domain.WorkerOnline && len(w.Capabilities) > 0
	})
	want := []string{"environment-host:v1", "harness-modes:v1", "harness:command", "harness:command:mode:headless", "harness:command:mode:interactive", "repo-config:v1"}
	if !slices.Equal(slices.Sorted(slices.Values(w.Capabilities)), want) || w.Health.Status != v1.HealthOK {
		t.Errorf("worker = %+v, want the capabilities %q", w, want)
	}
}

func TestRun_OffersTheInjectedHarnessesThatAreInstalledWithTheirModes(t *testing.T) {
	c := newController(t)
	paths := machine(t)
	c.join("box", paths)
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	headless := harness.Capabilities{Modes: []domain.Mode{domain.ModeHeadless}, ConfigIsolation: true}
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = daemon.Run(ctx, daemon.Options{
			Paths: paths, Interval: interval, Logger: slog.New(slog.DiscardHandler), Runtime: &recordingRuntime{},
			Harnesses: []harness.Adapter{
				fakeharness.Adapter{Binary: self, Caps: headless},
				fakeharness.Adapter{Name: "absent", Binary: "/nonexistent/fakeharness", Caps: headless},
			},
		})
	}()
	t.Cleanup(func() { cancel(); <-done })
	w := c.eventually("box", "the harness to be offered", func(w v1.Worker) bool {
		return w.State == domain.WorkerOnline && len(w.Capabilities) > 0
	})
	want := []string{"environment-host:v1", "harness-modes:v1", "harness:fake", "harness:fake:config-isolation", "harness:fake:mode:headless", "repo-config:v1"}
	if !slices.Equal(slices.Sorted(slices.Values(w.Capabilities)), want) {
		t.Errorf("capabilities = %q, want %q", w.Capabilities, want)
	}
}

func TestRun_OffersNoHarnessWithoutAUsableGit(t *testing.T) {
	c := newController(t)
	paths := machine(t)
	c.join("box", paths)
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = daemon.Run(ctx, daemon.Options{
			Paths: paths, Interval: interval, Logger: slog.New(slog.DiscardHandler), Runtime: &recordingRuntime{},
			GitPath: "/nonexistent/git",
		})
	}()
	t.Cleanup(func() { cancel(); <-done })
	w := c.eventually("box", "the Worker to be online", func(w v1.Worker) bool { return w.State == domain.WorkerOnline })
	if len(w.Capabilities) != 0 {
		t.Errorf("a Worker without git offers %v", w.Capabilities)
	}
}

// withoutCodex leaves out of the PATH of the test every directory with a
// codex or claude binary, so that what the Worker offers by default does not depend on
// the machine.
func withoutCodex(t *testing.T) {
	t.Helper()
	var kept []string
	for _, dir := range filepath.SplitList(os.Getenv("PATH")) {
		_, codex := os.Stat(filepath.Join(dir, "codex"))   //nolint:gosec // a directory of the PATH of the test
		_, claude := os.Stat(filepath.Join(dir, "claude")) //nolint:gosec // a directory of the PATH of the test
		if codex != nil && claude != nil {
			kept = append(kept, dir)
		}
	}
	t.Setenv("PATH", strings.Join(kept, string(os.PathListSeparator)))
}
