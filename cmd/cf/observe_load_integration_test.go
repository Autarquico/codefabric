//go:build integration && (linux || darwin)

package main

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	v1 "github.com/Autarquico/codefabric/internal/api/v1"
	"github.com/Autarquico/codefabric/internal/buildinfo"
	"github.com/Autarquico/codefabric/internal/cli"
	"github.com/Autarquico/codefabric/internal/client"
	"github.com/Autarquico/codefabric/internal/controller/daemon"
	"github.com/Autarquico/codefabric/internal/support/clock"
	"github.com/Autarquico/codefabric/internal/support/gittest"
	workerdaemon "github.com/Autarquico/codefabric/internal/worker/daemon"
	"github.com/Autarquico/codefabric/internal/worker/execwrap"
	"github.com/Autarquico/codefabric/internal/worker/outputship"
	"github.com/Autarquico/codefabric/internal/worker/state"
)

// The load of the plan (8.C): 100 Runs of one Worker print continuously while
// the real shipper sends their output to a real Controller, and events keep
// being followed. Nothing may fail with a busy database, every output must reach
// its EOF whole, and an event must still reach `cf events --follow` in under 2 s
// at p95. It takes about a minute, so it runs with CF_LOAD=1.
const (
	loadRuns      = 100
	loadWriteFor  = 8 * time.Second
	loadLineEvery = 250 * time.Millisecond
	loadLineBytes = 1 << 10
)

func TestLoad_OutputOf100RunsDoesNotStarveEvents(t *testing.T) {
	if os.Getenv("CF_LOAD") != "1" {
		t.Skip("set CF_LOAD=1 to run the output load test")
	}
	gittest.Require(t)
	var controllerLog syncBuffer
	host, _, _ := initController(t)
	serve(t, host, daemon.Options{Logger: slog.New(slog.NewTextHandler(&controllerLog, nil))})
	invite := workerInvite(t, host)
	box := machine(t)
	ok[map[string]any](t, "worker", "join", invite, "--name", "alpha", "--max-concurrent", "200")
	w, err := client.Open(box.WorkerCredentialsDir())
	if err != nil {
		t.Fatal(err)
	}
	beat := func() {
		_, err := w.Heartbeat(t.Context(), v1.HeartbeatRequest{
			Epoch: "01J00000000000000000000000", DaemonVersion: buildinfo.Get().Version, Hostname: "box", OS: "linux", Arch: "amd64",
			Total: v1.Capacity{CPU: 1024, MemoryMB: 1 << 20, DiskMB: 1 << 20}, Health: v1.Health{Status: v1.HealthOK},
		})
		if err != nil {
			t.Error(err)
		}
	}
	beat()
	stopBeats := make(chan struct{})
	go func() {
		for {
			select {
			case <-stopBeats:
				return
			case <-time.After(5 * time.Second):
				beat()
			}
		}
	}()
	defer close(stopBeats)

	use(t, host)
	newOrigin(t).clone(t)
	smallProject(t)
	admin, err := client.Open(host.CredentialsDir())
	if err != nil {
		t.Fatal(err)
	}
	commit := strings.TrimSpace(git(t, ".", "rev-parse", "HEAD"))
	for i := range loadRuns {
		if _, err := admin.CreateTask(t.Context(), v1.CreateTaskRequest{
			Project: "backend", Name: fmt.Sprintf("load-%03d", i), Prompt: "x", ResolvedRef: "main", SourceCommit: commit,
		}); err != nil {
			t.Fatal(err)
		}
	}
	var assigns []v1.AssignCommand
	for after := uint64(0); len(assigns) < loadRuns; {
		page, err := w.Assignments(t.Context(), after, 2*time.Second, 64)
		if err != nil {
			t.Fatal(err)
		}
		for _, c := range page.Commands {
			if c.Assign != nil {
				assigns = append(assigns, *c.Assign)
			}
		}
		after = page.LastSeq
	}
	for _, a := range assigns {
		if _, err := w.AcceptLease(t.Context(), a.LeaseID, a.Epoch); err != nil {
			t.Fatal(err)
		}
	}

	// The Worker's side: a state.db with the Runs running, their logs, and the real shipper.
	db, err := state.Open(t.Context(), box.WorkerStateFile())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	var keys []state.Key
	now := time.Now()
	for i, a := range assigns {
		k := state.Key{RunID: a.RunID, LeaseEpoch: a.Epoch}
		keys = append(keys, k)
		err := db.WithTx(t.Context(), func(tx *state.Tx) error {
			r := state.Run{
				Key: k, LeaseID: a.LeaseID, RunShort: "run-1", TaskID: "task_1", ProjectID: "project_1", CommandSeq: uint64(i + 1), AcceptBy: now.Add(time.Hour), //nolint:gosec // a small index
				SourceCommit: commit, CPU: 1, MemoryMB: 256, Spec: []byte(`{}`), Harness: "command", Mode: "headless", CreatedAt: now,
			}
			if _, err := state.InsertAssigned(t.Context(), tx, r); err != nil {
				return err
			}
			if err := state.Advance(t.Context(), tx, k, state.PhaseAssigned, state.PhaseAccepted, now); err != nil {
				return err
			}
			if err := state.Advance(t.Context(), tx, k, state.PhaseAccepted, state.PhaseStarting, now); err != nil {
				return err
			}
			return state.SetRunning(t.Context(), tx, k, `{"runtime":"tmux","ref":"x/y"}`, now)
		})
		if err != nil {
			t.Fatal(err)
		}
		if err := os.MkdirAll(box.WorkerRunDir(k.RunID, k.LeaseEpoch), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	var workerLog syncBuffer
	ship := outputship.New(outputship.Options{
		DB: db, Uploader: w, Paths: box, Clock: clock.Real{}, Log: slog.New(slog.NewTextHandler(&workerLog, nil)),
	})
	shipCtx, stopShip := context.WithCancel(t.Context())
	shipped := make(chan struct{})
	go func() { defer close(shipped); _ = ship.Run(shipCtx) }()
	defer func() { stopShip(); <-shipped }()

	// Events are followed as a person would, and each is stamped when its line arrives.
	stamps := &lineStamps{}
	followCtx, stopFollow := context.WithCancel(t.Context())
	defer stopFollow()
	go cli.RunContextWith(followCtx, options(), []string{"events", "--follow", "--json", "--type", "TaskCreated"},
		strings.NewReader(""), stamps, &syncBuffer{}, daemon.Command(), workerdaemon.Command())
	time.Sleep(time.Second)

	// 100 Runs print continuously, and every 200 ms a Task is created.
	var (
		wg      sync.WaitGroup
		written sync.Map
	)
	started := time.Now()
	line := bytes.Repeat([]byte("x"), loadLineBytes-1)
	line = append(line, '\n')
	for _, k := range keys {
		wg.Go(func() {
			f, err := os.OpenFile(filepath.Join(box.WorkerRunDir(k.RunID, k.LeaseEpoch), execwrap.OutputLog), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
			if err != nil {
				t.Error(err)
				return
			}
			defer func() { _ = f.Close() }()
			total := 0
			for time.Since(started) < loadWriteFor {
				n, _ := f.Write(append([]byte(fmt.Sprintf("%s %06d ", k.RunID[len(k.RunID)-6:], total/loadLineBytes)), line...))
				total += n
				time.Sleep(loadLineEvery)
			}
			written.Store(k, total)
		})
	}
	type created struct {
		task string
		at   time.Time
	}
	var makes []created
	for i := 0; time.Since(started) < loadWriteFor; i++ {
		at := time.Now()
		resp, err := admin.CreateTask(t.Context(), v1.CreateTaskRequest{Project: "backend", Name: fmt.Sprintf("probe-%03d", i), Prompt: "x", NoStart: true})
		if err != nil {
			t.Fatal(err)
		}
		makes = append(makes, created{task: resp.Task.ID, at: at})
		time.Sleep(200 * time.Millisecond)
	}
	wg.Wait()

	// The Runs end: the shipper must close every output whole.
	for _, k := range keys {
		if err := db.WithTx(t.Context(), func(tx *state.Tx) error {
			return state.Close(t.Context(), tx, k, state.LivePhases, state.PhaseStopped, false, time.Now())
		}); err != nil {
			t.Fatal(err)
		}
	}
	soon(t, "every output to be closed", func() bool {
		for _, k := range keys {
			if c, err := state.OutputCursorOf(t.Context(), db.Reader(), k); err != nil || !c.Done || !c.EOFSent {
				return false
			}
		}
		return true
	})
	elapsed := time.Since(started)

	var totalBytes uint64
	for _, k := range keys {
		raw, _ := written.Load(k)
		size := uint64(raw.(int)) //nolint:forcetypeassert,gosec // stored above, a small count
		totalBytes += size
		page, err := admin.RunOutput(t.Context(), k.RunID, client.OutputQuery{Epoch: k.LeaseEpoch})
		if err != nil || !page.Closed || page.End != size {
			t.Fatalf("%s: closed=%v end=%d want %d: %v", k.RunID, page.Closed, page.End, size, err)
		}
		file, _ := os.ReadFile(filepath.Join(box.WorkerRunDir(k.RunID, k.LeaseEpoch), execwrap.OutputLog)) //nolint:gosec // a file of the test
		if !bytes.HasSuffix(file, page.Data) {
			t.Fatalf("%s: the last bytes the Controller holds are not the file's", k.RunID)
		}
	}

	// Event latency during the load.
	var samples []time.Duration
	for _, m := range makes {
		var at time.Time
		soon(t, "the event of "+m.task, func() bool { var found bool; at, found = stamps.arrivalOfTask(m.task); return found })
		samples = append(samples, at.Sub(m.at))
	}
	slices.Sort(samples)
	p95 := percentile95(samples)
	t.Logf("%d Runs wrote %.1f MiB in %s (%.1f MiB/s); %d events: median %s, p95 %s, max %s",
		loadRuns, float64(totalBytes)/(1<<20), elapsed.Round(time.Millisecond), float64(totalBytes)/(1<<20)/loadWriteFor.Seconds(),
		len(samples), samples[len(samples)/2], p95, samples[len(samples)-1])
	if p95 >= 2*time.Second {
		t.Errorf("event p95 = %s during the load, want under 2s", p95)
	}
	for name, log := range map[string]string{"controller": controllerLog.String(), "worker": workerLog.String()} {
		for _, bad := range []string{"SQLITE_BUSY", "database is locked", "busy"} {
			if strings.Contains(strings.ToLower(log), strings.ToLower(bad)) {
				t.Errorf("the %s log mentions %q:\n%s", name, bad, log)
			}
		}
	}
}
