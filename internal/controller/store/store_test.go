package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/pressly/goose/v3"

	"github.com/Autarquico/codefabric/internal/domain"
	"github.com/Autarquico/codefabric/internal/support/clock"
)

var epoch0 = time.Date(2026, 10, 10, 12, 0, 0, 0, time.UTC)

func openTest(t *testing.T, opts Options) (*DB, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "controller.db")
	return reopen(t, path, opts), path
}

func reopen(t *testing.T, path string, opts Options) *DB {
	t.Helper()
	if opts.Clock == nil {
		opts.Clock = clock.NewFake(epoch0)
	}
	db, err := Open(t.Context(), path, opts)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

func workerEvent(workerID domain.ID, seq uint64) domain.Event {
	return domain.Event{
		Type:        domain.EventRunStarted,
		Source:      "worker:" + string(workerID),
		WorkerID:    workerID,
		WorkerEpoch: "epoch-1",
		WorkerSeq:   &seq,
		Actor:       domain.ActorRef{Kind: domain.ActorWorker, ID: string(workerID)},
	}
}

func controllerEvent(t domain.EventType) domain.Event {
	return domain.Event{Type: t, Actor: domain.ActorRef{Kind: domain.ActorSystem, ID: "controller"}}
}

func count(t *testing.T, db *DB, table string) int {
	t.Helper()
	var n int
	if err := db.Reader().QueryRowContext(t.Context(), "SELECT COUNT(*) FROM "+table).Scan(&n); err != nil {
		t.Fatalf("count %s: %v", table, err)
	}
	return n
}

func pragma(t *testing.T, q interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}, name string,
) string {
	t.Helper()
	var v string
	if err := q.QueryRowContext(t.Context(), "PRAGMA "+name).Scan(&v); err != nil {
		t.Fatalf("PRAGMA %s: %v", name, err)
	}
	return v
}

func TestScenario_ControllerStore_Pragmas(t *testing.T) {
	db, _ := openTest(t, Options{ReaderConns: 3})

	if got := pragma(t, db.writer, "journal_mode"); got != "wal" {
		t.Errorf("writer journal_mode = %q, want wal", got)
	}
	if got := pragma(t, db.writer, "synchronous"); got != "2" { // 2 = FULL
		t.Errorf("writer synchronous = %q, want 2 (FULL)", got)
	}
	if got := pragma(t, db.writer, "foreign_keys"); got != "1" {
		t.Errorf("writer foreign_keys = %q, want 1", got)
	}

	// Hold every reader connection at once so each one is inspected, not one reused.
	conns := make([]*sql.Conn, 0, 3)
	for range 3 {
		c, err := db.reader.Conn(t.Context())
		if err != nil {
			t.Fatalf("reader conn: %v", err)
		}
		defer func() { _ = c.Close() }()
		conns = append(conns, c)
	}
	for i, c := range conns {
		if got := pragma(t, c, "query_only"); got != "1" {
			t.Errorf("reader conn %d query_only = %q, want 1", i, got)
		}
		if _, err := c.ExecContext(t.Context(), `INSERT INTO controller_meta (key, value) VALUES ('x', 'y')`); err == nil {
			t.Errorf("reader conn %d accepted a write", i)
		}
	}
}

// The embedded migrations define the current schema version; the tests derive
// everything from it, so adding a migration never breaks them.
var (
	currentVersion = latestEmbeddedVersion()
	nextVersion    = currentVersion + 1
	backupSuffix   = fmt.Sprintf(".pre-%d", currentVersion)
)

func latestEmbeddedVersion() int64 {
	entries, err := fs.ReadDir(embeddedMigrations, "migrations")
	if err != nil {
		panic(err)
	}
	var latest int64
	for _, e := range entries {
		prefix, _, _ := strings.Cut(e.Name(), "_")
		v, err := strconv.ParseInt(prefix, 10, 64)
		if err != nil {
			panic(fmt.Sprintf("migration %q does not start with its version", e.Name()))
		}
		latest = max(latest, v)
	}
	return latest
}

func TestScenario_ControllerStore_BackupBeforeMigrating(t *testing.T) {
	path := filepath.Join(t.TempDir(), "controller.db")
	first := reopen(t, path, Options{})
	if _, err := os.Stat(path + backupSuffix); err == nil {
		t.Fatal("a brand-new database must not be backed up")
	}
	id := first.ControllerID()
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}

	var backupExistedDuringMigration bool
	second := goose.NewGoMigration(nextVersion, &goose.GoFunc{RunTx: func(_ context.Context, _ *sql.Tx) error {
		_, err := os.Stat(path + backupSuffix)
		backupExistedDuringMigration = err == nil
		return nil
	}}, nil)
	db := reopen(t, path, Options{goMigrations: []*goose.Migration{second}})

	if !backupExistedDuringMigration {
		t.Fatal("the pre-migration backup did not exist when the pending migration ran")
	}
	if err := quickCheckFile(t.Context(), path+backupSuffix); err != nil {
		t.Fatalf("backup is not a valid database: %v", err)
	}
	if db.ControllerID() != id {
		t.Errorf("controller id changed across restarts: %q -> %q", id, db.ControllerID())
	}
}

func TestBackup_InterruptedLeftoversAreReplacedByFreshBackup(t *testing.T) {
	path := filepath.Join(t.TempDir(), "controller.db")
	_ = reopen(t, path, Options{}).Close()

	// Leftovers of a crashed attempt: a half-written temporary and an unusable backup.
	if err := os.WriteFile(path+backupSuffix+".tmp", []byte("half written"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path+backupSuffix, []byte("not a database at all"), 0o600); err != nil {
		t.Fatal(err)
	}

	noop := goose.NewGoMigration(nextVersion, &goose.GoFunc{RunTx: func(context.Context, *sql.Tx) error { return nil }}, nil)
	clk := clock.NewFake(epoch0)
	reopen(t, path, Options{Clock: clk, goMigrations: []*goose.Migration{noop}})

	if err := quickCheckFile(t.Context(), path+backupSuffix); err != nil {
		t.Fatalf("backup was not rebuilt: %v", err)
	}
	if _, err := os.Stat(path + backupSuffix + ".tmp"); err == nil {
		t.Error("temporary backup left behind")
	}
	aside := fmt.Sprintf("%s%s.%d", path, backupSuffix, epoch0.Unix())
	got, err := os.ReadFile(aside) //nolint:gosec // test path
	if err != nil || string(got) != "not a database at all" {
		t.Errorf("previous backup not set aside at %s: content=%q err=%v", aside, got, err)
	}
}

func TestBackup_ExistingBackupIsNeverReused(t *testing.T) {
	path := filepath.Join(t.TempDir(), "controller.db")
	_ = reopen(t, path, Options{}).Close()

	failing := goose.NewGoMigration(nextVersion, &goose.GoFunc{RunTx: func(context.Context, *sql.Tx) error { return errors.New("boom") }}, nil)
	if _, err := Open(t.Context(), path, Options{goMigrations: []*goose.Migration{failing}}); err == nil {
		t.Fatal("migration failure must fail Open")
	}
	first, err := os.Stat(path + backupSuffix)
	if err != nil {
		t.Fatalf("no backup after a failed migration: %v", err)
	}

	fixed := goose.NewGoMigration(nextVersion, &goose.GoFunc{RunTx: func(context.Context, *sql.Tx) error { return nil }}, nil)
	reopen(t, path, Options{goMigrations: []*goose.Migration{fixed}})

	fresh, err := os.Stat(path + backupSuffix)
	if err != nil || os.SameFile(first, fresh) {
		t.Fatalf("backup was reused instead of rebuilt (err=%v)", err)
	}
	aside, err := os.Stat(fmt.Sprintf("%s%s.%d", path, backupSuffix, epoch0.Unix()))
	if err != nil || !os.SameFile(first, aside) {
		t.Fatalf("old backup not preserved under a timestamped name (err=%v)", err)
	}
}

func TestScenario_ControllerStore_Atomicity(t *testing.T) {
	db, _ := openTest(t, Options{})
	key := IdemKey{Principal: "owner", Operation: "task.create", RequestID: "req-1"}
	payload := []byte(`{"name":"t"}`)

	mutation := func(tx *Tx) error {
		if _, err := tx.Exec(t.Context(), `INSERT INTO controller_meta (key, value) VALUES ('probe', 'x')`); err != nil {
			return err
		}
		_, _, err := tx.AppendEvent(t.Context(), controllerEvent(domain.EventTaskCreated))
		return err
	}

	func() {
		defer func() {
			if recover() == nil {
				t.Error("the panic was swallowed")
			}
		}()
		_, _ = db.Idempotent(t.Context(), key, payload, func(tx *Tx) ([]byte, error) {
			if err := mutation(tx); err != nil {
				return nil, err
			}
			panic("crash mid-transaction")
		})
	}()

	if n := count(t, db, "controller_meta WHERE key = 'probe'"); n != 0 {
		t.Errorf("%d rows survived the panic", n)
	}
	if n := count(t, db, "events"); n != 0 {
		t.Errorf("%d events survived the panic", n)
	}
	if n := count(t, db, "idempotency_keys"); n != 0 {
		t.Errorf("%d idempotency keys survived the panic", n)
	}
	if got := db.Notifier().Latest(); got != 0 {
		t.Errorf("notifier announced %d for a rolled-back event", got)
	}

	resp, err := db.Idempotent(t.Context(), key, payload, func(tx *Tx) ([]byte, error) {
		return []byte("done"), mutation(tx)
	})
	if err != nil || string(resp) != "done" {
		t.Fatalf("retry = (%q, %v), want clean execution", resp, err)
	}
	if count(t, db, "events") != 1 || count(t, db, "idempotency_keys") != 1 {
		t.Error("retry did not persist exactly one event and one key")
	}
}

func TestScenario_ControllerStore_Idempotency(t *testing.T) {
	db, _ := openTest(t, Options{})
	key := IdemKey{Principal: "owner", Operation: "task.create", RequestID: "req-1"}
	var calls int
	run := func(payload string) ([]byte, error) {
		return db.Idempotent(t.Context(), key, []byte(payload), func(tx *Tx) ([]byte, error) {
			calls++
			_, _, err := tx.AppendEvent(t.Context(), controllerEvent(domain.EventTaskCreated))
			return []byte("response-" + payload), err
		})
	}

	first, err := run("a")
	if err != nil {
		t.Fatal(err)
	}
	again, err := run("a")
	if err != nil || string(again) != string(first) {
		t.Fatalf("replay = (%q, %v), want %q", again, err, first)
	}
	if calls != 1 {
		t.Errorf("fn ran %d times, want 1", calls)
	}
	if n := count(t, db, "events"); n != 1 {
		t.Errorf("replay produced new events: %d total", n)
	}

	_, err = run("b")
	if !errors.Is(err, domain.NewError(domain.CodeIdempotencyMismatch, "", nil)) {
		t.Fatalf("different payload: err = %v, want CF_IDEMPOTENCY_MISMATCH", err)
	}
	if calls != 1 || count(t, db, "events") != 1 {
		t.Error("a mismatching request must not run or emit anything")
	}
}

func TestIdempotent_KeysAreScopedByPrincipalAndOperation(t *testing.T) {
	db, _ := openTest(t, Options{})
	var calls int
	for _, key := range []IdemKey{
		{"owner", "task.create", "r"}, {"other", "task.create", "r"}, {"owner", "task.cancel", "r"},
	} {
		_, err := db.Idempotent(t.Context(), key, []byte("p"), func(*Tx) ([]byte, error) {
			calls++
			return nil, nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	if calls != 3 {
		t.Fatalf("fn ran %d times, want 3 (distinct scopes)", calls)
	}
}

func TestIdempotent_EmptyKeyAlwaysExecutes(t *testing.T) {
	db, _ := openTest(t, Options{})
	var calls int
	for range 2 {
		if _, err := db.Idempotent(t.Context(), IdemKey{}, nil, func(*Tx) ([]byte, error) {
			calls++
			return nil, nil
		}); err != nil {
			t.Fatal(err)
		}
	}
	if calls != 2 || count(t, db, "idempotency_keys") != 0 {
		t.Fatalf("calls = %d, keys = %d", calls, count(t, db, "idempotency_keys"))
	}
}

func TestIdempotent_RequestIDNeedsScope(t *testing.T) {
	db, _ := openTest(t, Options{})
	_, err := db.Idempotent(t.Context(), IdemKey{RequestID: "r"}, nil, func(*Tx) ([]byte, error) { return nil, nil })
	if !errors.Is(err, ErrInvalidIdempotencyKey) {
		t.Fatalf("err = %v, want ErrInvalidIdempotencyKey", err)
	}
}

func TestIdempotent_ErrorRollsBackAndLeavesNoKey(t *testing.T) {
	db, _ := openTest(t, Options{})
	key := IdemKey{"owner", "op", "r"}
	boom := errors.New("boom")

	_, err := db.Idempotent(t.Context(), key, []byte("p"), func(tx *Tx) ([]byte, error) {
		_, _, _ = tx.AppendEvent(t.Context(), controllerEvent(domain.EventTaskCreated))
		return nil, boom
	})
	if !errors.Is(err, boom) {
		t.Fatalf("err = %v", err)
	}
	if count(t, db, "events") != 0 || count(t, db, "idempotency_keys") != 0 {
		t.Fatal("a failed callback left state behind")
	}
	// The same key with a different payload is fine: nothing was recorded.
	if _, err := db.Idempotent(t.Context(), key, []byte("other"), func(*Tx) ([]byte, error) { return nil, nil }); err != nil {
		t.Fatalf("retry after failure: %v", err)
	}
}

func TestIdempotent_ConcurrentSameKeyExecutesOnce(t *testing.T) {
	db, _ := openTest(t, Options{})
	key := IdemKey{"owner", "op", "r"}
	var calls atomic.Int32

	const callers = 16
	responses := make([][]byte, callers)
	var wg sync.WaitGroup
	for i := range callers {
		wg.Go(func() {
			resp, err := db.Idempotent(t.Context(), key, []byte("p"), func(tx *Tx) ([]byte, error) {
				calls.Add(1)
				_, _, err := tx.AppendEvent(t.Context(), controllerEvent(domain.EventTaskCreated))
				return []byte("resp"), err
			})
			if err != nil {
				t.Errorf("caller %d: %v", i, err)
			}
			responses[i] = resp
		})
	}
	wg.Wait()

	if calls.Load() != 1 {
		t.Fatalf("fn ran %d times, want 1", calls.Load())
	}
	for i, r := range responses {
		if string(r) != "resp" {
			t.Errorf("caller %d got %q", i, r)
		}
	}
	if n := count(t, db, "events"); n != 1 {
		t.Errorf("%d events, want 1", n)
	}
}

func TestPurgeIdempotency_RemovesOnlyExpiredKeys(t *testing.T) {
	clk := clock.NewFake(epoch0)
	db, _ := openTest(t, Options{Clock: clk})
	put := func(id string) {
		t.Helper()
		if _, err := db.Idempotent(t.Context(), IdemKey{"owner", "op", id}, nil, func(*Tx) ([]byte, error) { return nil, nil }); err != nil {
			t.Fatal(err)
		}
	}
	put("old")
	clk.Advance(25 * time.Hour)
	put("new")

	purged, err := db.PurgeIdempotency(t.Context(), clk.Now().Add(-domain.Defaults().IdempotencyTTL))
	if err != nil || purged != 1 {
		t.Fatalf("purged = (%d, %v), want (1, nil)", purged, err)
	}
	var left string
	if err := db.Reader().QueryRowContext(t.Context(), `SELECT request_id FROM idempotency_keys`).Scan(&left); err != nil || left != "new" {
		t.Fatalf("remaining key = %q, %v", left, err)
	}
}

func TestScenario_ControllerStore_EventDeduplication(t *testing.T) {
	db, _ := openTest(t, Options{})
	worker := db.ids.New(domain.KindWorker)

	var seqs [2]uint64
	var dups [2]bool
	err := db.WithTx(t.Context(), func(tx *Tx) (err error) {
		for i := range 2 {
			if seqs[i], dups[i], err = tx.AppendEvent(t.Context(), workerEvent(worker, 7)); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if dups[0] || !dups[1] || seqs[0] != seqs[1] {
		t.Fatalf("got seqs=%v dups=%v, want same sequence and dup only on the second", seqs, dups)
	}

	// A redelivery in a later transaction (Worker retry) is deduplicated too.
	var again uint64
	var dup bool
	if err := db.WithTx(t.Context(), func(tx *Tx) (err error) {
		again, dup, err = tx.AppendEvent(t.Context(), workerEvent(worker, 7))
		return err
	}); err != nil {
		t.Fatalf("redelivery transaction: %v", err)
	}
	if !dup || again != seqs[0] {
		t.Errorf("redelivery = (%d, dup=%v), want (%d, true)", again, dup, seqs[0])
	}
	if n := count(t, db, "events"); n != 1 {
		t.Fatalf("%d events stored, want 1", n)
	}
	if got := db.Notifier().Latest(); got != seqs[0] {
		t.Errorf("notifier = %d, want %d", got, seqs[0])
	}
}

func TestAppendEvent_SameSeqFromAnotherWorkerOrEpochIsDistinct(t *testing.T) {
	db, _ := openTest(t, Options{})
	w1, w2 := db.ids.New(domain.KindWorker), db.ids.New(domain.KindWorker)
	otherEpoch := workerEvent(w1, 1)
	otherEpoch.WorkerEpoch = "epoch-2"

	err := db.WithTx(t.Context(), func(tx *Tx) error {
		for _, e := range []domain.Event{workerEvent(w1, 1), workerEvent(w2, 1), otherEpoch} {
			if _, dup, err := tx.AppendEvent(t.Context(), e); err != nil || dup {
				t.Errorf("AppendEvent dup=%v err=%v", dup, err)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if n := count(t, db, "events"); n != 3 {
		t.Fatalf("%d events, want 3", n)
	}
}

func TestAppendEvent_Validation(t *testing.T) {
	db, _ := openTest(t, Options{})
	seq := uint64(1)
	worker := db.ids.New(domain.KindWorker)

	cases := map[string]domain.Event{
		"heartbeat is never persisted": controllerEvent(domain.EventWorkerHeartbeat),
		"missing type":                 {},
		"epoch without seq":            {Type: domain.EventRunStarted, WorkerID: worker, WorkerEpoch: "e"},
		"seq without epoch":            {Type: domain.EventRunStarted, WorkerID: worker, WorkerSeq: &seq},
		"seq without worker":           {Type: domain.EventRunStarted, WorkerEpoch: "e", WorkerSeq: &seq},
		"data that is not JSON":        {Type: domain.EventRunStarted, Data: map[string]any{"c": make(chan int)}},
	}
	for name, e := range cases {
		t.Run(name, func(t *testing.T) {
			err := db.WithTx(t.Context(), func(tx *Tx) error {
				_, _, err := tx.AppendEvent(t.Context(), e)
				return err
			})
			if !errors.Is(err, ErrInvalidEvent) {
				t.Fatalf("err = %v, want ErrInvalidEvent", err)
			}
		})
	}
	if n := count(t, db, "events"); n != 0 {
		t.Fatalf("%d invalid events were stored", n)
	}
}

func TestAppendEvent_ControllerEventMayNameItsWorker(t *testing.T) {
	db, _ := openTest(t, Options{})
	lost := controllerEvent(domain.EventWorkerLost)
	lost.WorkerID = db.ids.New(domain.KindWorker)

	for range 2 { // no dedup key: both are stored
		err := db.WithTx(t.Context(), func(tx *Tx) error {
			_, dup, err := tx.AppendEvent(t.Context(), lost)
			if dup {
				t.Error("controller events have no dedup key")
			}
			return err
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	if n := count(t, db, "events"); n != 2 {
		t.Fatalf("%d events, want 2", n)
	}
}

func TestEvents_RoundTripFilterAndPaging(t *testing.T) {
	clk := clock.NewFake(epoch0)
	db, _ := openTest(t, Options{Clock: clk})
	worker := db.ids.New(domain.KindWorker)
	run := db.ids.New(domain.KindRun)
	task := db.ids.New(domain.KindTask)

	rich := workerEvent(worker, 1)
	rich.RunID, rich.TaskID = run, task
	rich.RequestID = "req-9"
	rich.Level = "info"
	rich.Message = "started"
	rich.Timestamp = epoch0.Add(-time.Second)
	rich.Data = map[string]any{"attempt": float64(2), "tags": []any{"a"}}

	err := db.WithTx(t.Context(), func(tx *Tx) error {
		if _, _, err := tx.AppendEvent(t.Context(), rich); err != nil {
			return err
		}
		for _, ty := range []domain.EventType{domain.EventRunCompleted, domain.EventTaskCreated, domain.EventRunFailed} {
			if _, _, err := tx.AppendEvent(t.Context(), controllerEvent(ty)); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}

	all, err := db.Events(t.Context(), 0, 0, EventFilter{})
	if err != nil || len(all) != 4 {
		t.Fatalf("Events = %d, %v", len(all), err)
	}
	got := all[0]
	if got.Sequence != 1 || got.ID == "" || got.SchemaVersion != EventSchemaVersion ||
		got.Source != "worker:"+string(worker) || got.WorkerEpoch != "epoch-1" || *got.WorkerSeq != 1 ||
		got.RunID != run || got.TaskID != task || got.RequestID != "req-9" || got.Message != "started" ||
		!got.Timestamp.Equal(rich.Timestamp) || !got.ReceivedAt.Equal(epoch0) ||
		got.Actor.Kind != domain.ActorWorker || got.Data["attempt"] != float64(2) {
		t.Fatalf("event did not round-trip: %+v", got)
	}
	if all[1].Source != "controller" || all[1].WorkerSeq != nil {
		t.Errorf("controller event = %+v", all[1])
	}
	for i := 1; i < len(all); i++ {
		if all[i].Sequence <= all[i-1].Sequence {
			t.Fatalf("not ordered by sequence: %v", all)
		}
	}

	page, _ := db.Events(t.Context(), all[0].Sequence, 2, EventFilter{})
	if len(page) != 2 || page[0].Sequence != all[1].Sequence {
		t.Errorf("paging after %d: %+v", all[0].Sequence, page)
	}
	byRun, _ := db.Events(t.Context(), 0, 10, EventFilter{RunID: run})
	if len(byRun) != 1 || byRun[0].ID != got.ID {
		t.Errorf("filter by run = %+v", byRun)
	}
	byType, _ := db.Events(t.Context(), 0, 10, EventFilter{Types: []domain.EventType{domain.EventRunFailed, domain.EventRunCompleted}})
	if len(byType) != 2 {
		t.Errorf("filter by types = %d events, want 2", len(byType))
	}
}

func TestWithTx_NotifiesOnlyAfterCommit(t *testing.T) {
	db, _ := openTest(t, Options{})
	n := db.Notifier()

	// Rolled back: nothing is announced.
	boom := errors.New("boom")
	err := db.WithTx(t.Context(), func(tx *Tx) error {
		if _, _, err := tx.AppendEvent(t.Context(), controllerEvent(domain.EventTaskCreated)); err != nil {
			return err
		}
		return boom
	})
	if !errors.Is(err, boom) || n.Latest() != 0 {
		t.Fatalf("after rollback: err=%v latest=%d", err, n.Latest())
	}

	// Committed: a waiter that started before the commit wakes only after it,
	// and by then the event is readable from the store.
	woke := make(chan uint64, 1)
	go func() {
		latest, err := n.Wait(t.Context(), 0)
		if err != nil {
			t.Errorf("Wait: %v", err)
		}
		woke <- latest
	}()

	release := make(chan struct{})
	committed := make(chan struct{})
	go func() {
		defer close(committed)
		_ = db.WithTx(t.Context(), func(tx *Tx) error {
			_, _, err := tx.AppendEvent(t.Context(), controllerEvent(domain.EventTaskCreated))
			<-release
			return err
		})
	}()

	select {
	case <-woke:
		t.Fatal("waiter woke before the transaction committed")
	case <-time.After(100 * time.Millisecond):
	}
	if n.Latest() != 0 {
		t.Fatal("event announced inside the open transaction")
	}

	close(release)
	<-committed
	select {
	case latest := <-woke:
		events, err := db.Events(t.Context(), 0, 10, EventFilter{})
		if err != nil || len(events) != 1 || events[0].Sequence != latest {
			t.Fatalf("woken at %d but store has %v, %v", latest, events, err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("waiter never woke after the commit")
	}
}

func TestNextShortID(t *testing.T) {
	db, _ := openTest(t, Options{})

	issue := func(kind domain.Kind) (n uint64) {
		t.Helper()
		err := db.WithTx(t.Context(), func(tx *Tx) (err error) {
			n, err = tx.NextShortID(t.Context(), kind)
			return err
		})
		if err != nil {
			t.Fatal(err)
		}
		return n
	}
	if a, b, c := issue(domain.KindRun), issue(domain.KindRun), issue(domain.KindTask); a != 1 || b != 2 || c != 1 {
		t.Fatalf("run, run, task = %d, %d, %d; want 1, 2, 1 (per-kind sequences)", a, b, c)
	}

	// A rolled-back transaction gives its number back.
	_ = db.WithTx(t.Context(), func(tx *Tx) error {
		_, _ = tx.NextShortID(t.Context(), domain.KindRun)
		return errors.New("rollback")
	})
	if got := issue(domain.KindRun); got != 3 {
		t.Fatalf("after rollback got %d, want 3", got)
	}
}

func TestNextShortID_MonotonicAndUniqueUnderConcurrency(t *testing.T) {
	db, _ := openTest(t, Options{})
	const callers = 50
	got := make(chan uint64, callers)
	var wg sync.WaitGroup
	for range callers {
		wg.Go(func() {
			err := db.WithTx(t.Context(), func(tx *Tx) error {
				n, err := tx.NextShortID(t.Context(), domain.KindRun)
				got <- n
				return err
			})
			if err != nil {
				t.Error(err)
			}
		})
	}
	wg.Wait()
	close(got)

	seen := map[uint64]bool{}
	for n := range got {
		if seen[n] {
			t.Fatalf("short id %d issued twice", n)
		}
		seen[n] = true
	}
	for n := uint64(1); n <= callers; n++ {
		if !seen[n] {
			t.Fatalf("short id %d never issued: the sequence has a gap", n)
		}
	}
}

func TestAppendAudit(t *testing.T) {
	db, _ := openTest(t, Options{})
	err := db.WithTx(t.Context(), func(tx *Tx) error {
		return tx.AppendAudit(t.Context(), AuditEntry{
			ActorKind: domain.ActorHuman, ActorID: "owner", Action: "worker.remove",
			ResourceKind: "worker", ResourceID: "worker_x",
			Before: map[string]any{"state": "online"}, After: map[string]any{"state": "removed"},
			Result: "ok", ConfirmationID: "confirmation_1",
		})
	})
	if err != nil {
		t.Fatal(err)
	}
	var actor, action, before, after, confirmation string
	var ts int64
	err = db.Reader().QueryRowContext(t.Context(),
		`SELECT ts, actor_id, action, before, after, confirmation_id FROM audit_entries`).
		Scan(&ts, &actor, &action, &before, &after, &confirmation)
	if err != nil {
		t.Fatal(err)
	}
	if ts != epoch0.UnixNano() || actor != "owner" || action != "worker.remove" ||
		!strings.Contains(before, "online") || !strings.Contains(after, "removed") || confirmation != "confirmation_1" {
		t.Fatalf("audit row = %d %s %s %s %s %s", ts, actor, action, before, after, confirmation)
	}
}

func TestAudit_RollsBackWithTransaction(t *testing.T) {
	db, _ := openTest(t, Options{})
	_ = db.WithTx(t.Context(), func(tx *Tx) error {
		_ = tx.AppendAudit(t.Context(), AuditEntry{Action: "a"})
		return errors.New("rollback")
	})
	if n := count(t, db, "audit_entries"); n != 0 {
		t.Fatalf("%d audit entries survived the rollback", n)
	}
}

func TestOpen_ReopenKeepsDataAndPrimesNotifier(t *testing.T) {
	db, path := openTest(t, Options{})
	_ = db.WithTx(t.Context(), func(tx *Tx) error {
		_, _, err := tx.AppendEvent(t.Context(), controllerEvent(domain.EventTaskCreated))
		return err
	})
	id := db.ControllerID()
	_ = db.Close()

	again := reopen(t, path, Options{})
	if again.ControllerID() != id || id == "" {
		t.Errorf("controller id %q -> %q", id, again.ControllerID())
	}
	if again.Notifier().Latest() != 1 {
		t.Errorf("notifier primed with %d, want 1", again.Notifier().Latest())
	}
	if err := again.Health(t.Context()); err != nil {
		t.Errorf("Health: %v", err)
	}
}

func TestOpen_PathWithSpecialCharacters(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "a b?c#d%e")
	db, err := Open(t.Context(), filepath.Join(dir, "controller.db"), Options{})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer func() { _ = db.Close() }()
	if _, err := os.Stat(filepath.Join(dir, "controller.db")); err != nil {
		t.Fatalf("database not created at the requested path: %v", err)
	}
}
