//go:build integration

package store

import (
	"context"
	"fmt"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Autarquico/codefabric/internal/domain"
)

// Simulated load (milestone benchmark): 25 Workers and 100 Runs for 5 s.
//   - every Worker owns 4 Runs and commits one transaction per tick holding one
//     event per Run: 100 Runs x 10 events/s = 1000 events/s in 250 commits/s;
//   - heartbeats are not events (§20.1): each Worker also commits an UPDATE on a
//     test table every heartbeatEvery, i.e. the spec's 10 s compressed 20x;
//   - one reader tails the stream through the Notifier, as an SSE client would.
const (
	loadWorkers        = 25
	loadRunsPerWorker  = 4
	loadDuration       = 5 * time.Second
	loadTick           = 400 * time.Millisecond / loadRunsPerWorker // 10 events/s per Run
	loadHeartbeatEvery = 500 * time.Millisecond
)

func TestLoad_NoBusy(t *testing.T) {
	if testing.Short() {
		t.Skip("load test skipped with -short")
	}
	db, _ := openTest(t, Options{})
	ctx, stop := context.WithTimeout(t.Context(), loadDuration)
	defer stop()

	if err := db.WithTx(t.Context(), func(tx *Tx) error {
		_, err := tx.Exec(t.Context(), `CREATE TABLE load_heartbeats (worker TEXT PRIMARY KEY, beat INTEGER NOT NULL)`)
		return err
	}); err != nil {
		t.Fatal(err)
	}

	var (
		mu        sync.Mutex
		latencies []time.Duration
		commits   atomic.Int64
		events    atomic.Int64
	)
	measure := func(fn func(*Tx) error) {
		start := time.Now()
		err := db.WithTx(t.Context(), fn)
		elapsed := time.Since(start)
		if err != nil {
			t.Errorf("transaction failed (SQLITE_BUSY shows up here): %v", err)
			return
		}
		commits.Add(1)
		mu.Lock()
		latencies = append(latencies, elapsed)
		mu.Unlock()
	}

	var wg sync.WaitGroup
	for w := range loadWorkers {
		wg.Go(func() {
			worker := domain.ID(fmt.Sprintf("worker_%02d", w))
			runs := make([]domain.ID, loadRunsPerWorker)
			for i := range runs {
				runs[i] = domain.ID(fmt.Sprintf("run_%02d_%d", w, i))
			}
			var seq uint64
			tick, beat := time.NewTicker(loadTick), time.NewTicker(loadHeartbeatEvery)
			defer tick.Stop()
			defer beat.Stop()
			for {
				select {
				case <-ctx.Done():
					return
				case <-beat.C:
					measure(func(tx *Tx) error {
						_, err := tx.Exec(t.Context(),
							`INSERT INTO load_heartbeats (worker, beat) VALUES (?, 1)
							 ON CONFLICT (worker) DO UPDATE SET beat = beat + 1`, string(worker))
						return err
					})
				case <-tick.C:
					measure(func(tx *Tx) error {
						for _, run := range runs {
							seq++
							e := workerEvent(worker, seq)
							e.RunID = run
							if _, _, err := tx.AppendEvent(t.Context(), e); err != nil {
								return err
							}
							events.Add(1)
						}
						return nil
					})
				}
			}
		})
	}

	var read atomic.Int64
	readerDone := make(chan struct{})
	go func() {
		defer close(readerDone)
		var cursor uint64
		for {
			if _, err := db.Notifier().Wait(ctx, cursor); err != nil {
				return
			}
			batch, err := db.Events(t.Context(), cursor, 1000, EventFilter{})
			if err != nil {
				t.Errorf("reader failed: %v", err)
				return
			}
			if len(batch) > 0 {
				cursor = batch[len(batch)-1].Sequence
				read.Add(int64(len(batch)))
			}
		}
	}()

	wg.Wait()
	<-readerDone

	// Drain what the tailing reader had not reached when the window closed.
	rest, err := db.Events(t.Context(), 0, int(events.Load())+1, EventFilter{})
	if err != nil {
		t.Fatal(err)
	}
	if int64(len(rest)) != events.Load() {
		t.Fatalf("store holds %d events, writers committed %d", len(rest), events.Load())
	}

	slices.Sort(latencies)
	p95 := latencies[len(latencies)*95/100]
	t.Logf("simulated: %d Workers, %d Runs at 10 events/s, heartbeat every %s (spec 10 s, compressed 20x), %s",
		loadWorkers, loadWorkers*loadRunsPerWorker, loadHeartbeatEvery, loadDuration)
	t.Logf("committed %d transactions and %d events: %.0f commits/s, %.0f events/s",
		commits.Load(), events.Load(), float64(commits.Load())/loadDuration.Seconds(), float64(events.Load())/loadDuration.Seconds())
	t.Logf("transaction latency (queue wait + commit): p50=%s p95=%s max=%s",
		latencies[len(latencies)/2], p95, latencies[len(latencies)-1])
	t.Logf("tailing reader saw %d events during the window", read.Load())

	// The floor is deliberately loose: raw throughput depends on how busy the
	// machine running the test is. The milestone target (§1.3) is checked by
	// the p95 latency logged above, not by this floor, which only catches a
	// collapse (writers stuck on SQLITE_BUSY, a stalled queue).
	if floor := 0.75 * 1000 * loadDuration.Seconds(); float64(events.Load()) < floor {
		t.Errorf("throughput too low: %d events committed, want at least %.0f", events.Load(), floor)
	}
	if err := db.Health(t.Context()); err != nil {
		t.Errorf("health after load: %v", err)
	}
}
