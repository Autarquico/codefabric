package daemon_test

import (
	"context"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/Autarquico/codefabric/internal/controller/daemon"
	"github.com/Autarquico/codefabric/internal/controller/store"
)

// The store logs about its own writes (store.WithTx). Were that logger the
// teed one, storing a line would log a line to store, for ever: the store is
// given the logger without the tee, and what is stored is flushed on stop.
const storeLine = "store: a transaction held the writer"

func TestTheControllerStoresItsLogsWithoutFeedbackAndKeepsThemOnStop(t *testing.T) {
	p := newPaths(t)
	initController(t, p)
	var (
		storeLog *slog.Logger
		db       *store.DB
	)
	daemon.WatchStore(t, func(o store.Options, d *store.DB) { storeLog, db = o.Logger, d })
	debug := slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelDebug}))
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	c, err := daemon.Start(ctx, daemon.Options{Paths: p, Logger: debug})
	if err != nil {
		t.Fatal(err)
	}
	all := func(db *store.DB) []store.LogRecord {
		t.Helper()
		var out []store.LogRecord
		err := db.Read(t.Context(), func(q store.Querier) (err error) {
			out, err = store.LogsAfter(t.Context(), q, 0, 1<<62, 1000, store.LogFilter{})
			return err
		})
		if err != nil {
			t.Fatal(err)
		}
		return out
	}
	latest := func(db *store.DB) uint64 {
		t.Helper()
		var seq uint64
		err := db.Read(t.Context(), func(q store.Querier) (err error) {
			seq, err = store.LatestLogSequence(t.Context(), q)
			return err
		})
		if err != nil {
			t.Fatal(err)
		}
		return seq
	}
	for deadline := time.Now().Add(5 * time.Second); latest(db) == 0; time.Sleep(10 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatal("no line of the Controller was stored")
		}
	}

	// A store that logs on every write: none of it is stored, however long it goes on.
	for range 100 {
		storeLog.Debug(storeLine, "held", time.Second)
	}
	before := latest(db)
	time.Sleep(200 * time.Millisecond)
	for _, l := range all(db) {
		if l.Message == storeLine {
			t.Fatalf("what the store logs was stored: %+v", l)
		}
	}

	cancel()
	if err := c.Wait(); err != nil {
		t.Fatal(err)
	}
	again, err := store.Open(t.Context(), p.DatabaseFile(), store.Options{Logger: slog.New(slog.DiscardHandler)})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = again.Close() }()
	if latest(again) < before {
		t.Fatalf("after stop the logs end at %d, before %d", latest(again), before)
	}
}
