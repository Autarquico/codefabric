package journal_test

import (
	"context"
	"errors"
	"log/slog"
	"path/filepath"
	"strings"
	"testing"

	v1 "github.com/Autarquico/codefabric/internal/api/v1"
	"github.com/Autarquico/codefabric/internal/domain"
	"github.com/Autarquico/codefabric/internal/support/clock"
	"github.com/Autarquico/codefabric/internal/support/slogtee"
	"github.com/Autarquico/codefabric/internal/worker/journal"
	"github.com/Autarquico/codefabric/internal/worker/state"
)

// logController is a LogShipper that remembers the batches it was sent.
type logController struct {
	batches [][]v1.LogEntry
	fail    error
}

func (c *logController) ShipLogs(_ context.Context, b v1.LogBatch) (v1.LogBatchResponse, error) {
	if c.fail != nil {
		return v1.LogBatchResponse{}, c.fail
	}
	c.batches = append(c.batches, b.Entries)
	return v1.LogBatchResponse{Applied: len(b.Entries)}, nil
}

func openState(t *testing.T) *state.DB {
	t.Helper()
	db, err := state.Open(t.Context(), filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

func lines(n int, msg string) []slogtee.Entry {
	out := make([]slogtee.Entry, n)
	for i := range out {
		out[i] = slogtee.Entry{Time: t0, Level: "info", Msg: msg, Attrs: []byte(`{}`), RunID: "run_1"}
	}
	return out
}

func pendingLogs(t *testing.T, db *state.DB) int {
	t.Helper()
	n, err := state.LogJournalCount(t.Context(), db.Reader())
	if err != nil {
		t.Fatal(err)
	}
	return n
}

func TestTheLogJournalSendsInOrderWithinTheLimitsAndKeepsTheKeyAcrossEpochs(t *testing.T) {
	ctx := t.Context()
	db := openState(t)
	ctl := &logController{}
	quiet := slog.New(slog.DiscardHandler)
	first := journal.NewLogs(journal.LogsOptions{DB: db, Epoch: "E1", Shipper: ctl, Clock: clock.NewFake(t0), Log: quiet})
	if err := first.Store(ctx, lines(300, "a")); err != nil {
		t.Fatal(err)
	}
	// A daemon that restarts before sending: its lines keep the epoch they were logged under.
	second := journal.NewLogs(journal.LogsOptions{DB: db, Epoch: "E2", Shipper: ctl, Clock: clock.NewFake(t0), Log: quiet})
	if err := second.Store(ctx, lines(2, strings.Repeat("b", v1.LogMessageMaxBytes))); err != nil {
		t.Fatal(err)
	}
	for pendingLogs(t, db) > 0 {
		if _, err := second.Flush(ctx); err != nil {
			t.Fatal(err)
		}
	}
	var (
		last   uint64
		epochs = map[string]int{}
	)
	for _, b := range ctl.batches {
		size := 0
		for _, e := range b {
			if e.WorkerSeq <= last {
				t.Fatalf("seq %d after %d", e.WorkerSeq, last)
			}
			last, size = e.WorkerSeq, size+len(e.Message)+len(e.Attrs)
			epochs[e.WorkerEpoch]++
			if e.RunID != "run_1" || e.Level != "info" || !e.Timestamp.Equal(t0) {
				t.Fatalf("entry = %+v", e)
			}
		}
		if len(b) > v1.LogBatchMaxEntries || size > v1.LogBatchBytes {
			t.Fatalf("a batch of %d entries, %d bytes", len(b), size)
		}
	}
	if epochs["E1"] != 300 || epochs["E2"] != 2 {
		t.Fatalf("epochs = %v", epochs)
	}
}

func TestAControllerThatFailsLeavesTheLinesAndOneThatRefusesTheBatchDropsIt(t *testing.T) {
	ctx := t.Context()
	db := openState(t)
	ctl := &logController{fail: errors.New("connection refused")}
	logs := journal.NewLogs(journal.LogsOptions{DB: db, Epoch: "E1", Shipper: ctl, Clock: clock.NewFake(t0), Log: slog.New(slog.DiscardHandler)})
	if err := logs.Store(ctx, lines(3, "a")); err != nil {
		t.Fatal(err)
	}
	if _, err := logs.Flush(ctx); err == nil || pendingLogs(t, db) != 3 {
		t.Fatalf("err %v, %d pending", err, pendingLogs(t, db))
	}
	ctl.fail = domain.NewError(domain.CodeInvalidRequest, "no", nil)
	if n, err := logs.Flush(ctx); err != nil || n != 3 || pendingLogs(t, db) != 0 {
		t.Fatalf("n %d, err %v, %d pending", n, err, pendingLogs(t, db))
	}
}

func TestTheLogJournalIsBoundedAndSaysWhatItDropped(t *testing.T) {
	ctx := t.Context()
	db := openState(t)
	var fallback strings.Builder
	logs := journal.NewLogs(journal.LogsOptions{
		DB: db, Epoch: "E1", Shipper: &logController{}, Clock: clock.NewFake(t0), Log: slog.New(slog.NewTextHandler(&fallback, nil)), Limit: 10,
	})
	for range 3 {
		if err := logs.Store(ctx, lines(6, "a")); err != nil {
			t.Fatal(err)
		}
	}
	rows, err := state.PendingLogs(ctx, db.Reader(), 100)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 10 || rows[len(rows)-1].Seq != 18 || rows[0].Seq != 9 {
		t.Fatalf("%d rows kept, from %d to %d", len(rows), rows[0].Seq, rows[len(rows)-1].Seq)
	}
	if !strings.Contains(fallback.String(), "dropped=2") || !strings.Contains(fallback.String(), "dropped=6") {
		t.Fatalf("fallback = %q", fallback.String())
	}
}
