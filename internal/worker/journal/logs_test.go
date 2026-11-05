package journal_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"

	v1 "github.com/Autarquico/codefabric/internal/api/v1"
	"github.com/Autarquico/codefabric/internal/client"
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

func TestTheLogJournalIsBoundedAndSaysWhatItDroppedAtMostOnceAMinute(t *testing.T) {
	ctx := t.Context()
	db := openState(t)
	clk := clock.NewFake(t0)
	var fallback strings.Builder
	logs := journal.NewLogs(journal.LogsOptions{
		DB: db, Epoch: "E1", Shipper: &logController{}, Clock: clk, Log: slog.New(slog.NewTextHandler(&fallback, nil)), Limit: 10,
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
	if got := strings.Count(fallback.String(), "journal is full"); got != 1 || !strings.Contains(fallback.String(), "dropped=2") {
		t.Fatalf("within a minute, %d warnings: %q", got, fallback.String())
	}
	clk.Advance(time.Minute)
	if err := logs.Store(ctx, lines(6, "a")); err != nil {
		t.Fatal(err)
	}
	if got := strings.Count(fallback.String(), "journal is full"); got != 2 || !strings.Contains(fallback.String(), "dropped=12") {
		t.Fatalf("a minute later, %d warnings: %q", got, fallback.String())
	}
}

// strictController refuses a batch as the Controller does: a body over
// v1.LogBodyBytes, or messages and attributes over v1.LogBatchBytes, is
// CF_INVALID_REQUEST; a line out of bounds is rejected.
type strictController struct {
	bodies  []int
	batches [][]v1.LogEntry
}

func (c *strictController) ShipLogs(_ context.Context, b v1.LogBatch) (v1.LogBatchResponse, error) {
	raw, err := json.Marshal(b)
	if err != nil {
		return v1.LogBatchResponse{}, err
	}
	if len(raw) > v1.LogBodyBytes || len(b.Entries) > v1.LogBatchMaxEntries {
		return v1.LogBatchResponse{}, domain.NewError(domain.CodeInvalidRequest, "the request body is too large", nil)
	}
	var got v1.LogBatch
	if err := json.Unmarshal(raw, &got); err != nil {
		return v1.LogBatchResponse{}, err
	}
	var out v1.LogBatchResponse
	size := 0
	for _, e := range got.Entries {
		size += len(e.Message) + len(e.Attrs)
		if len(e.Message) > v1.LogMessageMaxBytes || len(e.Attrs) > v1.LogAttrsMaxBytes || !utf8.ValidString(e.Message) {
			out.Rejected++
			continue
		}
		out.Applied++
	}
	if size > v1.LogBatchBytes {
		return v1.LogBatchResponse{}, domain.NewError(domain.CodeInvalidRequest, "the entries are too large", nil)
	}
	c.bodies, c.batches = append(c.bodies, len(raw)), append(c.batches, got.Entries)
	return out, nil
}

func TestLogBatchesFitTheControllerAsSentEscapingIncluded(t *testing.T) {
	ctx := t.Context()
	db := openState(t)
	ctl := &strictController{}
	logs := journal.NewLogs(journal.LogsOptions{DB: db, Epoch: "E1", Shipper: ctl, Clock: clock.NewFake(t0), Log: slog.New(slog.DiscardHandler)})
	// 256 lines of 1000 bytes: under v1.LogBatchBytes counted raw, over v1.LogBodyBytes once escaped.
	burst := lines(256, strings.Repeat("<", 1000))
	for i := range burst {
		burst[i].Attrs = []byte(`{"redacted":"<redacted>"}`)
	}
	if err := logs.Store(ctx, burst); err != nil {
		t.Fatal(err)
	}
	if n, err := logs.Flush(ctx); err != nil || n != 256 || pendingLogs(t, db) != 0 {
		t.Fatalf("n %d, err %v, %d pending", n, err, pendingLogs(t, db))
	}
	delivered := 0
	for i, b := range ctl.batches {
		delivered += len(b)
		if ctl.bodies[i] > v1.LogBatchBytes {
			t.Fatalf("batch %d is a body of %d bytes", i, ctl.bodies[i])
		}
		if i+1 < len(ctl.batches) && len(b) < v1.LogBatchMaxEntries {
			next, _ := json.Marshal(ctl.batches[i+1][0])
			if ctl.bodies[i]+1+len(next) <= v1.LogBatchBytes {
				t.Fatalf("batch %d (%d bytes) was cut before the line after it filled it", i, ctl.bodies[i])
			}
		}
	}
	if delivered != 256 || len(ctl.batches) < 2 {
		t.Fatalf("%d lines in %d batches", delivered, len(ctl.batches))
	}
}

func TestALineTooLongForTheControllerIsShortenedAndSaysSo(t *testing.T) {
	ctx := t.Context()
	db := openState(t)
	ctl := &strictController{}
	var fallback strings.Builder
	logs := journal.NewLogs(journal.LogsOptions{DB: db, Epoch: "E1", Shipper: ctl, Clock: clock.NewFake(t0), Log: slog.New(slog.NewTextHandler(&fallback, nil))})
	huge := lines(1, strings.Repeat("<é", 3*v1.LogMessageMaxBytes))
	huge[0].Attrs = []byte(`{"a":"` + strings.Repeat("<", 2*v1.LogAttrsMaxBytes) + `"}`)
	// The largest line within the bounds: every character of its message is escaped on the wire.
	worst := lines(1, strings.Repeat("<", v1.LogMessageMaxBytes))
	worst[0].Attrs = []byte(`{"a":"` + strings.Repeat("x", v1.LogAttrsMaxBytes-8) + `"}`)
	// Attributes the encoder escapes on the wire are bounded as they arrive.
	escaped := lines(1, "e")
	escaped[0].Attrs = []byte(`{"a":"<redacted>"}`)
	if err := logs.Store(ctx, append(append(huge, worst...), escaped...)); err != nil {
		t.Fatal(err)
	}
	if n, err := logs.Flush(ctx); err != nil || n != 3 {
		t.Fatalf("n %d, err %v", n, err)
	}
	var got []v1.LogEntry
	for _, b := range ctl.batches {
		got = append(got, b...)
	}
	if len(got) != 3 {
		t.Fatalf("%d lines delivered", len(got))
	}
	if !strings.HasSuffix(got[0].Message, "[truncated]") || len(got[0].Message) > v1.LogMessageMaxBytes || !strings.Contains(string(got[0].Attrs), "attrs_truncated_bytes") {
		t.Fatalf("the long line = %d bytes %q…, attrs %s", len(got[0].Message), got[0].Message[:16], got[0].Attrs)
	}
	if got[1].Message != worst[0].Msg || string(got[1].Attrs) != string(worst[0].Attrs) {
		t.Fatal("a line within the bounds was changed")
	}
	var attrs struct{ A string }
	if err := json.Unmarshal(got[2].Attrs, &attrs); err != nil || attrs.A != "<redacted>" {
		t.Fatalf("escaped attrs %s: %v", got[2].Attrs, err)
	}
	if !strings.Contains(fallback.String(), "shortened") || !strings.Contains(fallback.String(), "lines=1") {
		t.Fatalf("fallback = %q", fallback.String())
	}
}

// oldController is a Controller older than POST /v1/worker/logs.
type oldController struct {
	mu    sync.Mutex
	calls int
	knows bool
	sent  int
}

func (c *oldController) ShipLogs(_ context.Context, b v1.LogBatch) (v1.LogBatchResponse, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.calls++
	if !c.knows {
		return v1.LogBatchResponse{}, fmt.Errorf("unexpected response: %w", client.ErrUnknownRoute)
	}
	c.sent += len(b.Entries)
	return v1.LogBatchResponse{Applied: len(b.Entries)}, nil
}

func (c *oldController) state() (calls, sent int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.calls, c.sent
}

func TestAControllerWithoutTheRouteIsToldOnceAndAskedAgainOnlyAfterALongWhile(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	db := openState(t)
	clk := clock.NewFake(t0)
	ctl := &oldController{}
	var (
		mu       sync.Mutex
		fallback strings.Builder
	)
	logs := journal.NewLogs(journal.LogsOptions{
		DB: db, Epoch: "E1", Shipper: ctl, Clock: clk, Log: slog.New(slog.NewTextHandler(lockedWriter{&mu, &fallback}, nil)),
	})
	if err := logs.Store(ctx, lines(3, "a")); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- logs.Run(ctx) }()
	waitFor := func(what string, ok func() bool) {
		t.Helper()
		deadline := time.Now().Add(5 * time.Second)
		for !ok() {
			if time.Now().After(deadline) {
				t.Fatalf("timed out: %s", what)
			}
			time.Sleep(time.Millisecond)
		}
	}
	// The ticker and the long wait.
	if err := clk.BlockUntil(ctx, 2); err != nil {
		t.Fatal(err)
	}
	for range 3 {
		clk.Advance(time.Minute) // past any backoff, short of the long wait
	}
	if calls, _ := ctl.state(); calls != 1 {
		t.Fatalf("asked %d times within 3 minutes", calls)
	}
	clk.Advance(10 * time.Minute)
	waitFor("asked again", func() bool { calls, _ := ctl.state(); return calls == 2 })
	if err := clk.BlockUntil(ctx, 2); err != nil {
		t.Fatal(err)
	}
	ctl.mu.Lock()
	ctl.knows = true // upgraded
	ctl.mu.Unlock()
	clk.Advance(10 * time.Minute)
	waitFor("sent once upgraded", func() bool { _, sent := ctl.state(); return sent == 3 })
	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	defer mu.Unlock()
	out := fallback.String()
	if strings.Count(out, "does not take structured logs") != 1 || strings.Contains(out, "level=WARN") || !strings.Contains(out, "level=INFO") {
		t.Fatalf("fallback = %q", out)
	}
	if pendingLogs(t, db) != 0 {
		t.Fatalf("%d pending", pendingLogs(t, db))
	}
}

type lockedWriter struct {
	mu *sync.Mutex
	w  *strings.Builder
}

func (l lockedWriter) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.w.Write(p)
}
