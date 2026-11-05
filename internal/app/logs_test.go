package app_test

import (
	"context"
	"io"
	"log/slog"
	"path/filepath"
	"strings"
	"testing"
	"time"

	v1 "github.com/Autarquico/codefabric/internal/api/v1"
	"github.com/Autarquico/codefabric/internal/app/logs"
	"github.com/Autarquico/codefabric/internal/controller/pki"
	"github.com/Autarquico/codefabric/internal/domain"
	"github.com/Autarquico/codefabric/internal/support/clock"
	"github.com/Autarquico/codefabric/internal/support/slogtee"
	"github.com/Autarquico/codefabric/internal/worker/journal"
	"github.com/Autarquico/codefabric/internal/worker/state"
)

func logLine(epoch string, seq uint64, at time.Time, level, msg string) v1.LogEntry {
	return v1.LogEntry{WorkerEpoch: epoch, WorkerSeq: seq, Timestamp: at, Level: level, Message: msg, Attrs: []byte(`{"k":1}`)}
}

func (e *env) shipLogs(w pki.Session, entries ...v1.LogEntry) v1.LogBatchResponse {
	e.t.Helper()
	out, err := e.app.Logs.Ingest(e.t.Context(), w, v1.LogBatch{Entries: entries})
	must(e.t, err)
	return out
}

func (e *env) listLogs(q logs.Query) []v1.Log {
	e.t.Helper()
	page, err := e.app.Logs.List(e.t.Context(), e.owner, q)
	must(e.t, err)
	return page.Logs
}

func messages(ls []v1.Log) string {
	out := make([]string, len(ls))
	for i, l := range ls {
		out[i] = l.Message
	}
	return strings.Join(out, " ")
}

func TestLogs_ALineSentAgainIsStoredOnceAndAMalformedOneIsRefused(t *testing.T) {
	e := newEnv(t)
	w := e.joinWorker("alpha")
	now := e.clock.Now()
	bad := logLine("E1", 3, now, "loud", "bad level")
	notObject := logLine("E1", 4, now, "info", "attrs not an object")
	notObject.Attrs = []byte(`[1]`)
	if got := e.shipLogs(w, logLine("E1", 1, now, "info", "one"), logLine("E1", 2, now, "warn", "two"), bad, notObject); got != (v1.LogBatchResponse{Applied: 2, Rejected: 2}) {
		t.Fatalf("first = %+v", got)
	}
	// Sent again after a lost answer, and under another epoch with the same seq: a new line.
	if got := e.shipLogs(w, logLine("E1", 1, now, "info", "one"), logLine("E2", 1, now, "info", "three")); got != (v1.LogBatchResponse{Applied: 1, Duplicate: 1}) {
		t.Fatalf("again = %+v", got)
	}
	got := e.listLogs(logs.Query{Worker: "alpha"})
	if messages(got) != "one two three" {
		t.Fatalf("logs = %q", messages(got))
	}
	if got[0].Source != "worker:"+got[0].WorkerID || got[0].WorkerSeq == nil || *got[0].WorkerSeq != 1 || string(got[0].Attrs) != `{"k":1}` {
		t.Fatalf("first log = %+v", got[0])
	}
}

func TestLogs_AWorkerNamesOnlyTheRunsItHeldAndTheirTaskComesFromTheController(t *testing.T) {
	e := newEnv(t)
	mine := e.outputRun("alpha")
	theirs := e.outputRun("beta")
	now := e.clock.Now()

	own := logLine("E1", 1, now, "info", "own")
	own.RunID = mine.run.ID
	other := logLine("E1", 2, now, "info", "other")
	other.RunID = theirs.run.ID
	missing := logLine("E1", 3, now, "info", "missing")
	missing.RunID = "run_01NOTHERE"
	e.shipLogs(mine.worker, own, other, missing)

	got := e.listLogs(logs.Query{Worker: "alpha"})
	if len(got) != 3 {
		t.Fatalf("%d logs", len(got))
	}
	if got[0].RunID != mine.run.ID || got[0].TaskID != mine.run.TaskID || got[0].ProjectID == "" {
		t.Fatalf("own = %+v", got[0])
	}
	for _, l := range got[1:] {
		if l.RunID != "" || l.TaskID != "" || l.ProjectID != "" {
			t.Fatalf("a Run the Worker never held is named: %+v", l)
		}
	}
	if byRun := e.listLogs(logs.Query{Run: theirs.run.ID}); len(byRun) != 0 {
		t.Fatalf("the Run of beta has %d lines of alpha", len(byRun))
	}
}

func TestLogs_OnlyAWorkerShipsLogs(t *testing.T) {
	e := newEnv(t)
	_, err := e.app.Logs.Ingest(t.Context(), e.owner, v1.LogBatch{Entries: []v1.LogEntry{logLine("E1", 1, e.clock.Now(), "info", "x")}})
	wantCode(t, err, domain.CodeUnauthenticated)
	w := e.joinWorker("alpha")
	_, err = e.app.Logs.Ingest(t.Context(), w, v1.LogBatch{Entries: make([]v1.LogEntry, v1.LogBatchMaxEntries+1)})
	wantCode(t, err, domain.CodeInvalidRequest)
}

func TestScenario_LogsOutputStorage_Filtros(t *testing.T) {
	e := newEnv(t)
	a := e.outputRun("alpha")
	b := e.joinWorker("beta")
	start := e.clock.Now()
	e.shipLogs(a.worker, logLine("E", 1, start, "debug", "a-debug"), logLine("E", 2, start, "error", "a-error-old"))
	e.clock.Advance(time.Hour)
	inRun := logLine("E", 3, start, "warn", "a-warn-run")
	inRun.RunID = a.run.ID
	e.shipLogs(a.worker, inRun, logLine("E", 4, start, "info", "a-info"))
	e.shipLogs(b, logLine("E", 1, start, "error", "b-error"))
	must(t, e.app.Logs.AppendController(t.Context(), []slogtee.Entry{{Time: start, Level: "error", Msg: "c-error", Attrs: []byte(`{}`)}}))

	since := start.Add(30 * time.Minute)
	for _, c := range []struct {
		q    logs.Query
		want string
	}{
		{logs.Query{}, "a-debug a-error-old a-warn-run a-info b-error c-error"},
		{logs.Query{Worker: "alpha", Level: "warn"}, "a-error-old a-warn-run"},
		{logs.Query{Worker: "alpha", Level: "warn", Since: since}, "a-warn-run"},
		{logs.Query{Level: "error", Since: since}, "b-error c-error"},
		{logs.Query{Run: a.run.Short}, "a-warn-run"},
		{logs.Query{Task: a.run.TaskID}, "a-warn-run"},
		{logs.Query{Project: "backend"}, "a-warn-run"},
		{logs.Query{Tail: 2}, "b-error c-error"},
	} {
		if got := messages(e.listLogs(c.q)); got != c.want {
			t.Errorf("%+v: %q, want %q", c.q, got, c.want)
		}
	}
	if got := e.listLogs(logs.Query{Level: "error"}); got[len(got)-1].Source != "controller" || got[len(got)-1].WorkerID != "" {
		t.Fatalf("the Controller's line = %+v", got[len(got)-1])
	}
	_, err := e.app.Logs.List(t.Context(), e.owner, logs.Query{Level: "loud"})
	wantCode(t, err, domain.CodeInvalidRequest)
}

// Two Workers whose clocks are an hour ahead and an hour behind, and a
// Controller whose clock steps back: the logs list in the order the Controller
// stored them, never by the time they carry or were received at.
func TestScenario_LogsOutputStorage_Orden(t *testing.T) {
	e := newEnv(t)
	ahead, behind := e.joinWorker("ahead"), e.joinWorker("behind")
	now := e.clock.Now()
	for i := range 6 {
		w, skew := ahead, time.Hour
		if i%2 == 1 {
			w, skew = behind, -time.Hour
		}
		e.shipLogs(w, logLine("E", uint64(i+1), now.Add(skew), "info", string(rune('a'+i))))
		// The Controller's clock steps back too: received_at does not follow the sequence either.
		e.clock.Advance(3*time.Second - time.Duration(i%2)*5*time.Second)
	}
	page, err := e.app.Logs.List(t.Context(), e.owner, logs.Query{Limit: 4})
	must(t, err)
	next, err := e.app.Logs.List(t.Context(), e.owner, logs.Query{After: mustCursor(t, page.Cursor)})
	must(t, err)
	got := append(page.Logs, next.Logs...)
	if messages(got) != "a b c d e f" || !page.HasMore || next.HasMore {
		t.Fatalf("logs = %q (has_more %v %v)", messages(got), page.HasMore, next.HasMore)
	}
	for i := 1; i < len(got); i++ {
		if got[i].Sequence <= got[i-1].Sequence {
			t.Fatalf("sequence %d after %d", got[i].Sequence, got[i-1].Sequence)
		}
	}
	if !got[1].Timestamp.Before(got[0].Timestamp) || !got[2].ReceivedAt.Before(got[1].ReceivedAt) {
		t.Fatal("the clocks of the test are not skewed")
	}
}

func mustCursor(t *testing.T, s string) uint64 {
	t.Helper()
	n, err := logs.ParseCursor(s)
	must(t, err)
	return n
}

func TestScenario_LogsOutputStorage_Follow(t *testing.T) {
	e := newEnv(t)
	w := e.joinWorker("alpha")
	from, err := e.app.Logs.Latest(t.Context())
	must(t, err)
	f, err := e.app.Logs.Open(t.Context(), e.owner, logs.Query{Level: "warn"}, from)
	must(t, err)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	got := make(chan v1.Log, 4)
	go func() {
		_ = f.Run(ctx, func(ls []v1.Log, _ string) error {
			for _, l := range ls {
				got <- l
			}
			return nil
		}, func() error { return nil })
	}()
	time.Sleep(20 * time.Millisecond) // the follower is waiting
	sent := time.Now()
	e.shipLogs(w, logLine("E", 1, e.clock.Now(), "info", "quiet"), logLine("E", 2, e.clock.Now(), "warn", "loud"))
	select {
	case l := <-got:
		if l.Message != "loud" {
			t.Fatalf("followed %q", l.Message)
		}
		if took := time.Since(sent); took > 2*time.Second {
			t.Fatalf("the line took %v", took)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("the line did not arrive in 2 s")
	}
}

// shipperOf is the real Controller service as the log journal's shipper.
type shipperOf struct {
	svc *logs.Service
	by  pki.Session
}

func (s shipperOf) ShipLogs(ctx context.Context, b v1.LogBatch) (v1.LogBatchResponse, error) {
	return s.svc.Ingest(ctx, s.by, b)
}

// A Worker logs through its teed logger; the lines go through its state.db and
// reach a real Controller, which binds them to the Run the Worker holds.
func TestLogs_AWorkerLogsAndTheControllerHoldsTheLinesRedactedAndBoundToItsRun(t *testing.T) {
	e := newEnv(t)
	o := e.outputRun("alpha")
	ctx := t.Context()
	db, err := state.Open(ctx, filepath.Join(t.TempDir(), "state.db"))
	must(t, err)
	t.Cleanup(func() { _ = db.Close() })

	quiet := slog.New(slog.NewTextHandler(io.Discard, nil))
	jrn := journal.NewLogs(journal.LogsOptions{
		DB: db, Epoch: "EPOCH1", Shipper: shipperOf{e.app.Logs, o.worker}, Clock: clock.NewFake(e.clock.Now()), Log: quiet,
	})
	tee := slogtee.New(slogtee.Options{})
	log := slog.New(tee.Wrap(quiet.Handler()))
	teeCtx, stopTee := context.WithCancel(ctx)
	teeDone := make(chan struct{})
	go func() { defer close(teeDone); tee.Run(teeCtx, jrn.Store, quiet) }()

	log.Info("harness started", "run", o.run.ID, "token", "ghp_secret")
	log.Warn("push to https://x:pw@example.com failed")
	stopTee()
	<-teeDone
	sent, err := jrn.Flush(ctx)
	must(t, err)
	if sent != 2 {
		t.Fatalf("%d lines sent", sent)
	}
	got := e.listLogs(logs.Query{Worker: "alpha"})
	if messages(got) != "harness started push to <redacted> failed" {
		t.Fatalf("logs = %q", messages(got))
	}
	if got[0].RunID != o.run.ID || got[0].TaskID != o.run.TaskID || got[0].WorkerEpoch != "EPOCH1" {
		t.Fatalf("first = %+v", got[0])
	}
	if strings.Contains(string(got[0].Attrs), "ghp_secret") {
		t.Fatalf("attrs = %s", got[0].Attrs)
	}
	// Sending the journal again (an answer lost) stores nothing new.
	again, err := e.app.Logs.Ingest(ctx, o.worker, v1.LogBatch{Entries: []v1.LogEntry{{
		WorkerEpoch: "EPOCH1", WorkerSeq: *got[0].WorkerSeq, Timestamp: got[0].Timestamp, Level: "info", Message: "harness started",
	}}})
	must(t, err)
	if again.Duplicate != 1 {
		t.Fatalf("again = %+v", again)
	}
}
