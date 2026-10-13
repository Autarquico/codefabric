//go:build integration && (linux || darwin)

package main

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	v1 "github.com/Autarquico/codefabric/internal/api/v1"
	"github.com/Autarquico/codefabric/internal/buildinfo"
	"github.com/Autarquico/codefabric/internal/cli"
	"github.com/Autarquico/codefabric/internal/client"
	"github.com/Autarquico/codefabric/internal/config"
	"github.com/Autarquico/codefabric/internal/controller/daemon"
	"github.com/Autarquico/codefabric/internal/support/clock"
	"github.com/Autarquico/codefabric/internal/support/gittest"
	workerdaemon "github.com/Autarquico/codefabric/internal/worker/daemon"
	"github.com/Autarquico/codefabric/internal/worker/execwrap"
	"github.com/Autarquico/codefabric/internal/worker/outputship"
	"github.com/Autarquico/codefabric/internal/worker/state"
)

// The load of the plan (8.C): 100 Runs spread over four Workers print
// continuously while the real shippers send their output to a real Controller,
// and events keep being followed and Tasks created. Nothing may fail with a busy
// database, no transaction may hold the writer for long, every output must reach
// its EOF whole, creating a Task must stay under 300 ms at p95 and an event must
// still reach `cf events --follow` in under 2 s at p95. It takes about a minute,
// so it runs with CF_LOAD=1.
const (
	loadRuns      = 100
	loadWorkers   = 4
	loadWriteFor  = 8 * time.Second
	loadLineEvery = 250 * time.Millisecond
	loadLineBytes = 1 << 10
)

// loadWorker is one Worker of the load: its machine, its state.db with its Runs
// running, and its shipper.
type loadWorker struct {
	box     config.Paths
	client  *client.Client
	db      *state.DB
	assigns []v1.AssignCommand
	keys    []state.Key
	log     syncBuffer
}

// heldRe reads the duration out of the debug line the store writes for a
// transaction that held the writer.
var heldRe = regexp.MustCompile(`a transaction held the writer" held=(\S+)`)

// slowHolds counts the logged holds longer than limit, and says how many it
// recognised, so that a line it cannot read is not taken for a fast one.
func slowHolds(log string, limit time.Duration) (slow, recognised int, worst time.Duration) {
	for _, m := range heldRe.FindAllStringSubmatch(log, -1) {
		d, err := time.ParseDuration(m[1])
		if err != nil {
			continue
		}
		recognised++
		worst = max(worst, d)
		if d > limit {
			slow++
		}
	}
	return slow, recognised, worst
}

func TestSlowHoldsReadsTheLinesTheStoreWrites(t *testing.T) {
	log := `time=2026-10-13T20:33:24.391+01:00 level=DEBUG msg="store: a transaction held the writer" held=101.744125ms
time=2026-10-13T20:33:26.248+01:00 level=DEBUG msg="store: a transaction held the writer" held=600ms
time=2026-10-13T20:33:27.248+01:00 level=INFO msg=request path=/v1/worker/output
time=2026-10-13T20:33:28.248+01:00 level=DEBUG msg="store: a transaction held the writer" held=1.2s
`
	slow, recognised, worst := slowHolds(log, 500*time.Millisecond)
	if slow != 2 || recognised != strings.Count(log, "a transaction held the writer") || worst != 1200*time.Millisecond {
		t.Fatalf("slow %d, recognised %d, worst %s", slow, recognised, worst)
	}
}

func TestLoad_OutputOf100RunsDoesNotStarveEvents(t *testing.T) {
	if os.Getenv("CF_LOAD") != "1" {
		t.Skip("set CF_LOAD=1 to run the output load test")
	}
	gittest.Require(t)
	var controllerLog syncBuffer
	host, _, _ := initController(t)
	serve(t, host, daemon.Options{Logger: slog.New(slog.NewTextHandler(&controllerLog, &slog.HandlerOptions{Level: slog.LevelDebug}))})

	workers := make([]*loadWorker, loadWorkers)
	for i := range workers {
		invite := workerInvite(t, host)
		lw := &loadWorker{box: machine(t)}
		ok[map[string]any](t, "worker", "join", invite, "--name", fmt.Sprintf("load-%d", i), "--max-concurrent", "200")
		c, err := client.Open(lw.box.WorkerCredentialsDir())
		if err != nil {
			t.Fatal(err)
		}
		lw.client = c
		workers[i] = lw
		epoch := fmt.Sprintf("01J0000000000000000000000%d", i)
		beat := func() {
			_, err := c.Heartbeat(t.Context(), v1.HeartbeatRequest{
				Epoch: epoch, DaemonVersion: buildinfo.Get().Version, Hostname: "box", OS: "linux", Arch: "amd64",
				Total: v1.Capacity{CPU: 1024, MemoryMB: 1 << 20, DiskMB: 1 << 20}, Health: v1.Health{Status: v1.HealthOK},
			})
			if err != nil && t.Context().Err() == nil {
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
	}

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
	// Each Worker takes the Runs the scheduler gave it and accepts their leases.
	for got := 0; got < loadRuns; {
		for _, lw := range workers {
			page, err := lw.client.Assignments(t.Context(), 0, 0, 256)
			if err != nil {
				t.Fatal(err)
			}
			for _, c := range page.Commands[len(lw.assigns):] {
				if c.Assign != nil {
					lw.assigns = append(lw.assigns, *c.Assign)
					got++
				}
			}
		}
		time.Sleep(50 * time.Millisecond)
	}
	spread := make([]int, len(workers))
	for i, lw := range workers {
		spread[i] = len(lw.assigns)
		for _, a := range lw.assigns {
			if _, err := lw.client.AcceptLease(t.Context(), a.LeaseID, a.Epoch); err != nil {
				t.Fatal(err)
			}
		}
	}

	// Each Worker's side: a state.db with its Runs running, their logs, and its real shipper.
	now := time.Now()
	shipCtx, stopShip := context.WithCancel(t.Context())
	var shippers sync.WaitGroup
	defer func() { stopShip(); shippers.Wait() }()
	for _, lw := range workers {
		db, err := state.Open(t.Context(), lw.box.WorkerStateFile())
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = db.Close() }()
		lw.db = db
		for i, a := range lw.assigns {
			k := state.Key{RunID: a.RunID, LeaseEpoch: a.Epoch}
			lw.keys = append(lw.keys, k)
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
			if err := os.MkdirAll(lw.box.WorkerRunDir(k.RunID, k.LeaseEpoch), 0o700); err != nil {
				t.Fatal(err)
			}
		}
		ship := outputship.New(outputship.Options{
			DB: db, Uploader: lw.client, Paths: lw.box, Clock: clock.Real{}, Log: slog.New(slog.NewTextHandler(&lw.log, nil)),
		})
		shippers.Go(func() { _ = ship.Run(shipCtx) })
	}

	// Events are followed as a person would, and each is stamped when its line arrives.
	stamps := &lineStamps{}
	followCtx, stopFollow := context.WithCancel(t.Context())
	defer stopFollow()
	go cli.RunContextWith(followCtx, options(), []string{"events", "--follow", "--json", "--type", "TaskCreated"},
		strings.NewReader(""), stamps, &syncBuffer{}, daemon.Command(), workerdaemon.Command())
	time.Sleep(time.Second)

	// The Runs print continuously, and every 100 ms a Task is created.
	var (
		wg      sync.WaitGroup
		written sync.Map
	)
	started := time.Now()
	line := append(bytes.Repeat([]byte("x"), loadLineBytes-1), '\n')
	for _, lw := range workers {
		for _, k := range lw.keys {
			wg.Go(func() {
				f, err := os.OpenFile(filepath.Join(lw.box.WorkerRunDir(k.RunID, k.LeaseEpoch), execwrap.OutputLog), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
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
	}
	type created struct {
		task string
		at   time.Time
	}
	var (
		makes     []created
		createDur []time.Duration
	)
	for i := 0; time.Since(started) < loadWriteFor; i++ {
		at := time.Now()
		resp, err := admin.CreateTask(t.Context(), v1.CreateTaskRequest{Project: "backend", Name: fmt.Sprintf("probe-%03d", i), Prompt: "x", NoStart: true})
		if err != nil {
			t.Fatal(err)
		}
		done := time.Now()
		makes = append(makes, created{task: resp.Task.ID, at: done})
		createDur = append(createDur, done.Sub(at))
		time.Sleep(100 * time.Millisecond)
	}
	wg.Wait()

	// The Runs end: every shipper must close every output whole.
	for _, lw := range workers {
		for _, k := range lw.keys {
			if err := lw.db.WithTx(t.Context(), func(tx *state.Tx) error {
				return state.Close(t.Context(), tx, k, state.LivePhases, state.PhaseStopped, false, time.Now())
			}); err != nil {
				t.Fatal(err)
			}
		}
	}
	soon(t, "every output to be closed", func() bool {
		for _, lw := range workers {
			for _, k := range lw.keys {
				if c, err := state.OutputCursorOf(t.Context(), lw.db.Reader(), k); err != nil || !c.Done || !c.EOFSent {
					return false
				}
			}
		}
		return true
	})
	elapsed := time.Since(started)

	var totalBytes uint64
	for _, lw := range workers {
		for _, k := range lw.keys {
			raw, _ := written.Load(k)
			size := uint64(raw.(int)) //nolint:forcetypeassert,gosec // stored above, a small count
			totalBytes += size
			page, err := admin.RunOutput(t.Context(), k.RunID, client.OutputQuery{Epoch: k.LeaseEpoch})
			if err != nil || !page.Closed || page.End != size {
				t.Fatalf("%s: closed=%v end=%d want %d: %v", k.RunID, page.Closed, page.End, size, err)
			}
			file, _ := os.ReadFile(filepath.Join(lw.box.WorkerRunDir(k.RunID, k.LeaseEpoch), execwrap.OutputLog)) //nolint:gosec // a file of the test
			if !bytes.HasSuffix(file, page.Data) {
				t.Fatalf("%s: the last bytes the Controller holds are not the file's", k.RunID)
			}
		}
	}

	// Event latency and the cost of creating a Task, during the load.
	var samples []time.Duration
	for _, m := range makes {
		var at time.Time
		soon(t, "the event of "+m.task, func() bool { var found bool; at, found = stamps.arrivalOfTask(m.task); return found })
		samples = append(samples, at.Sub(m.at))
	}
	slices.Sort(samples)
	slices.Sort(createDur)
	p95, createP95 := percentile95(samples), percentile95(createDur)
	logged := controllerLog.String() // one snapshot: heartbeats keep writing to the log
	held := strings.Count(logged, "a transaction held the writer")
	t.Logf("%d Runs on %v Workers wrote %.1f MiB in %s; %d Tasks created: CreateTask median %s, p95 %s, max %s; event after the create returns: median %s, p95 %s, max %s; transactions over 100 ms: %d",
		loadRuns, spread, float64(totalBytes)/(1<<20), elapsed.Round(time.Millisecond), len(makes),
		createDur[len(createDur)/2], createP95, createDur[len(createDur)-1], samples[len(samples)/2], p95, samples[len(samples)-1], held)
	if createP95 >= 300*time.Millisecond {
		t.Errorf("CreateTask p95 = %s during the load, want under 300ms", createP95)
	}
	if p95 >= 2*time.Second {
		t.Errorf("event p95 = %s during the load, want under 2s", p95)
	}
	// A transaction of 32 entries or 256 KiB takes about 100 ms under -race: that is
	// the bound of the design, not zero. What must not happen is that many are slow.
	// The denominator is the uploads, each one at least a transaction.
	slow, recognised, worst := slowHolds(logged, holdLimit)
	if recognised != held {
		t.Errorf("the log has %d held-writer lines and %d were read: the count of slow ones cannot be trusted", held, recognised)
	}
	uploads := strings.Count(logged, "path=/v1/worker/output")
	t.Logf("uploads: %d; transactions over 100 ms: %d; over %s: %d; the longest hold: %s", uploads, held, holdLimit, slow, worst)
	if uploads == 0 || slow*100 > uploads {
		t.Errorf("%d of %d output transactions held the writer for over %s, want at most 1%%", slow, uploads, holdLimit)
	}
	logs := controllerLog.String()
	for _, lw := range workers {
		logs += lw.log.String()
	}
	for _, bad := range []string{"sqlite_busy", "database is locked", "busy"} {
		if strings.Contains(strings.ToLower(logs), bad) {
			t.Errorf("a log mentions %q", bad)
		}
	}
}
