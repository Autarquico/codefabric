//go:build integration

package gc_test

import (
	"context"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/Autarquico/codefabric/internal/controller/store"
	"github.com/Autarquico/codefabric/internal/domain"
)

// Milestone benchmark of the Latencia scenario (run by `make bench`): the real
// p95 of a write that competes with a large GC pass. CI proves the invariant
// (batches of at most gc.batch_size rows, in transactions of their own); the
// time a write waits depends on the disk, so it is reported and never asserted.
func TestLoad_GCWriteLatencyP95(t *testing.T) {
	if testing.Short() {
		t.Skip("load test skipped with -short")
	}
	const (
		rows  = 50_000
		chunk = 1_000
	)
	f := newFixture(t)
	for range rows / chunk {
		f.tx(func(tx *store.Tx) error {
			for range chunk {
				e := domain.Event{Type: domain.EventRunQueued, Actor: domain.ActorRef{Kind: domain.ActorSystem, ID: "controller"}}
				if _, _, err := tx.AppendEvent(t.Context(), e); err != nil {
					return err
				}
			}
			return nil
		})
	}
	f.clk.Advance(10 * day)

	var (
		mu        sync.Mutex
		latencies []time.Duration
	)
	done := make(chan struct{})
	var writer sync.WaitGroup
	writer.Go(func() {
		for i := 0; ; i++ {
			select {
			case <-done:
				return
			default:
			}
			start := time.Now()
			err := f.db.WithTx(context.Background(), func(tx *store.Tx) error {
				_, _, err := tx.AppendEvent(context.Background(),
					domain.Event{Type: domain.EventRunQueued, Actor: domain.ActorRef{Kind: domain.ActorSystem, ID: "controller"}})
				return err
			})
			if err != nil {
				t.Errorf("a write during the pass failed: %v", err)
				return
			}
			mu.Lock()
			latencies = append(latencies, time.Since(start))
			mu.Unlock()
		}
	})

	policy := shortPolicy
	policy.BatchSize = 500
	start := time.Now()
	rep, err := f.service(policy).Run(t.Context(), false)
	close(done)
	writer.Wait()
	if err != nil {
		t.Fatal(err)
	}
	// The newest event is never purged, and is one of the writer's once it has written.
	if got := items(rep)["events"]; got < rows-1 || got > rows {
		t.Fatalf("purged %d events, want %d or %d", got, rows-1, rows)
	}
	if len(latencies) == 0 {
		t.Fatal("no write was measured")
	}
	slices.Sort(latencies)
	t.Logf("GC of %d events in batches of %d took %s; %d concurrent writes", rows, policy.BatchSize, time.Since(start), len(latencies))
	t.Logf("write latency during the pass: p50=%s p95=%s max=%s",
		latencies[len(latencies)/2], latencies[len(latencies)*95/100], latencies[len(latencies)-1])
}
