package store

import (
	"bytes"
	"errors"
	"math/rand/v2"
	"strings"
	"testing"
	"time"

	"github.com/Autarquico/codefabric/internal/domain"
)

func TestSettledRuns_IsExactlyTheTerminalStates(t *testing.T) {
	listed := map[string]bool{}
	for _, s := range strings.Split(strings.Trim(settledRuns, "()"), ",") {
		listed[strings.Trim(strings.TrimSpace(s), "'")] = true
	}
	for _, s := range []domain.RunState{
		domain.RunQueued, domain.RunPreparing, domain.RunRunning, domain.RunWaitingInput, domain.RunCheckpointing,
		domain.RunSuspended, domain.RunCompleted, domain.RunFailed, domain.RunCancelled, domain.RunLost,
	} {
		if listed[string(s)] != s.IsTerminal() {
			t.Errorf("%s: listed as settled = %v, terminal = %v", s, listed[string(s)], s.IsTerminal())
		}
	}
}

func (f *leaseFixture) exec(query string, args ...any) {
	f.t.Helper()
	f.tx(func(tx *Tx) error { _, err := tx.Exec(f.t.Context(), query, args...); return err })
}

func (f *leaseFixture) settle(run domain.ID) {
	f.t.Helper()
	f.exec(`UPDATE runs SET state = 'failed', finished_at = ? WHERE id = ?`, f.clk.Now().UnixNano(), string(run))
}

func (f *leaseFixture) purgeStatus() (through uint64, remaining int) {
	f.t.Helper()
	if err := f.db.Read(f.t.Context(), func(q Querier) (err error) {
		_, through, err = EventBounds(f.t.Context(), q)
		return err
	}); err != nil {
		f.t.Fatal(err)
	}
	return through, count(f.t, f.db, "events")
}

func (f *leaseFixture) eventsPurgeable(cutoff time.Time) EventsPurge {
	f.t.Helper()
	var p EventsPurge
	if err := f.db.Read(f.t.Context(), func(q Querier) (err error) {
		p, err = EventsPurgeable(f.t.Context(), q, cutoff)
		return err
	}); err != nil {
		f.t.Fatal(err)
	}
	return p
}

func TestEventsPurgeable_IsAContiguousPrefixThatStopsAtARunThatHasNotEnded(t *testing.T) {
	f, run := outputFixture(t) // run is queued: it has not ended
	seqs := seedEvents(t, f.db, f.clk, 8, []domain.EventType{domain.EventRunQueued}, []string{domain.LevelInfo})
	f.exec(`UPDATE events SET run_id = ? WHERE sequence = ?`, string(run), seqs[3])
	cutoff := f.clk.Now().Add(time.Hour) // everything is old enough

	p := f.eventsPurgeable(cutoff)
	if p.Through != seqs[2] || p.Rows != 3 || p.Held != 4 {
		t.Fatalf("with a live Run at the 4th event: %+v, want through %d, 3 rows, 4 held", p, seqs[2])
	}

	f.settle(run)
	p = f.eventsPurgeable(cutoff)
	if want := seqs[6]; p.Through != want || p.Rows != 7 || p.Held != 0 {
		t.Fatalf("once it ended: %+v, want through %d (the last event stays), 7 rows", p, want)
	}
}

func TestEventsPurgeable_StopsAtTheFirstEventNewerThanTheCutoff(t *testing.T) {
	f, _ := outputFixture(t)
	seqs := seedEvents(t, f.db, f.clk, 6, []domain.EventType{domain.EventRunQueued}, []string{domain.LevelInfo})
	// Event i was received i+1 seconds after epoch0.
	if p := f.eventsPurgeable(epoch0.Add(3*time.Second + time.Millisecond)); p.Through != seqs[2] || p.Rows != 3 {
		t.Fatalf("%+v, want the first three events", p)
	}
	if p := f.eventsPurgeable(epoch0); p.Rows != 0 {
		t.Fatalf("%+v, nothing is old enough", p)
	}
}

func TestPurgeEvents_RecordsThePurgeInTheSameTransactionAsTheDelete(t *testing.T) {
	f, _ := outputFixture(t)
	seqs := seedEvents(t, f.db, f.clk, 7, []domain.EventType{domain.EventRunQueued}, []string{domain.LevelInfo})

	for i, want := range []int64{3, 3, 0} {
		n, err := f.db.PurgeEvents(t.Context(), seqs[5], 3)
		if err != nil || n != want {
			t.Fatalf("batch %d = %d, %v; want %d", i, n, err, want)
		}
	}
	through, remaining := f.purgeStatus()
	if through != seqs[5] || remaining != 1 {
		t.Fatalf("purged through %d with %d events left; want %d and 1", through, remaining, seqs[5])
	}
	// A cursor below the purge is outside the history that remains.
	if n, err := f.db.PurgeEvents(t.Context(), seqs[2], 3); err != nil || n != 0 {
		t.Fatalf("purging again below = %d, %v", n, err)
	}
	if through, _ := f.purgeStatus(); through != seqs[5] {
		t.Fatalf("events_purged_through went back to %d", through)
	}
}

func TestPurgeReceipts_KeepsTheReceiptsOfARunThatHasNotEnded(t *testing.T) {
	f, live := outputFixture(t)
	ended := f.queuedRun(2)
	f.settle(ended.ID)
	w := f.worker(9, "w9", domain.WorkerOnline)
	for i, run := range []domain.ID{live, ended.ID, ended.ID} {
		f.exec(`INSERT INTO report_receipts (worker_id, worker_epoch, worker_seq, run_id, kind, digest, applied_at) VALUES (?, 'e', ?, ?, 'started', 'd', ?)`,
			string(w.ID), i+1, string(run), epoch0.UnixNano())
	}
	cutoff := epoch0.Add(time.Hour)

	var due int64
	_ = f.db.Read(t.Context(), func(q Querier) (err error) { due, err = ReceiptsPurgeable(t.Context(), q, cutoff); return err })
	n, err := f.db.PurgeReceipts(t.Context(), cutoff, 1)
	if err != nil || due != 2 || n != 1 {
		t.Fatalf("due %d, first batch %d, %v; want 2 and 1", due, n, err)
	}
	if n, _ = f.db.PurgeReceipts(t.Context(), cutoff, 10); n != 1 {
		t.Fatalf("second batch = %d, want 1", n)
	}
	if left := count(t, f.db, "report_receipts"); left != 1 {
		t.Fatalf("%d receipts left, want the live Run's", left)
	}
}

func TestPurgeOutput_EmptiesAnOutputInBatchesThatLeaveItValidBetweenThem(t *testing.T) {
	f, run := outputFixture(t)
	data := make([]byte, 2*SegmentBytes+40_000) // two segments and a hot tail
	_, _ = rand.NewChaCha8([32]byte{1}).Read(data)
	for off := 0; off < len(data); off += 64 << 10 {
		if _, err := f.append(run, 1, uint64(off), data[off:min(off+64<<10, len(data))]); err != nil {
			t.Fatal(err)
		}
	}
	f.settle(run)
	before, _ := f.held(run, 1)
	if before.Sealed != 2*SegmentBytes {
		t.Fatalf("sealed %d, want two segments", before.Sealed)
	}
	cutoff := f.clk.Now().Add(time.Hour)

	var due int64
	_ = f.db.Read(t.Context(), func(q Querier) (err error) { due, err = OutputsPurgeable(t.Context(), q, cutoff); return err })
	const limit = 2
	var total int64
	for batches := 0; ; batches++ {
		rows, found, err := f.db.PurgeOutput(t.Context(), cutoff, limit)
		if err != nil {
			t.Fatal(err)
		}
		if !found {
			break
		}
		if rows > limit {
			t.Fatalf("a batch deleted %d rows, the limit is %d", rows, limit)
		}
		total += rows
		o, _ := f.held(run, 1) // readable between batches, whatever is left
		if o.Start > o.End || o.End != before.End {
			t.Fatalf("between batches the output is [%d, %d)", o.Start, o.End)
		}
		if batches > 100 {
			t.Fatal("the purge does not end")
		}
	}
	if total != due {
		t.Fatalf("deleted %d rows, the dry run counted %d", total, due)
	}
	o, held := f.held(run, 1)
	if o.Start != o.End || o.End != before.End || len(held) != 0 {
		t.Fatalf("after the purge: [%d, %d) with %d bytes", o.Start, o.End, len(held))
	}
	if n := count(t, f.db, "logs_index") + count(t, f.db, "run_output_chunks"); n != 0 {
		t.Fatalf("%d rows left", n)
	}
	if _, found, _ := f.db.PurgeOutput(t.Context(), cutoff, limit); found {
		t.Fatal("a purged output is purged again")
	}
	// A reader asking for the old bytes is told they were truncated, not that the output is unreadable.
	err := f.db.Read(t.Context(), func(q Querier) error {
		_, err := f.db.OutputRange(t.Context(), q, run, 1, o.Start, o.End)
		return err
	})
	if errors.Is(err, ErrOutputUnreadable) || err != nil {
		t.Fatalf("reading a purged output: %v", err)
	}
}

func TestPurgeOutput_LeavesAnOutputOfARunThatHasNotEnded(t *testing.T) {
	f, run := outputFixture(t)
	if _, err := f.append(run, 1, 0, bytes.Repeat([]byte("x"), 100)); err != nil {
		t.Fatal(err)
	}
	if _, found, err := f.db.PurgeOutput(t.Context(), f.clk.Now().Add(time.Hour), 10); err != nil || found {
		t.Fatalf("found = %v, %v; a live Run's output is not purged", found, err)
	}
}

func TestPurgeCommands_RemovesOnlyOrdersTheWorkerHasAckedOrTheControllerWithdrew(t *testing.T) {
	f, _ := outputFixture(t)
	w := f.worker(9, "w9", domain.WorkerOnline)
	add := func(seq int, acked, cancelled any) {
		f.exec(`INSERT INTO worker_commands (worker_id, seq, type, payload, created_at, acked_at, cancelled_at) VALUES (?, ?, 'ping', '{}', ?, ?, ?)`,
			string(w.ID), seq, epoch0.UnixNano(), acked, cancelled)
	}
	add(1, 1, nil)
	add(2, nil, 1)
	add(3, nil, nil) // pending: it must still be delivered
	cutoff := epoch0.Add(time.Hour)

	if n, err := f.db.PurgeCommands(t.Context(), cutoff, 10); err != nil || n != 2 {
		t.Fatalf("purged %d, %v; want 2", n, err)
	}
	if left := count(t, f.db, "worker_commands"); left != 1 {
		t.Fatalf("%d orders left, want the pending one", left)
	}
}

func TestPurgeLogs_SeparatesSystemLogsFromRunLogs(t *testing.T) {
	f, run := outputFixture(t)
	f.exec(`INSERT INTO logs (ts, received_at, source, level, msg) VALUES (1, ?, 'controller', 'info', 'system')`, epoch0.UnixNano())
	f.exec(`INSERT INTO logs (ts, received_at, source, level, msg, run_id) VALUES (1, ?, 'worker', 'info', 'run', ?)`, epoch0.UnixNano(), string(run))
	cutoff := epoch0.Add(time.Hour)

	if n, err := f.db.PurgeLogs(t.Context(), SystemLogs, cutoff, 10); err != nil || n != 1 {
		t.Fatalf("system = %d, %v", n, err)
	}
	if left := count(t, f.db, "logs"); left != 1 {
		t.Fatalf("%d logs left, want the Run's", left)
	}
	if n, err := f.db.PurgeLogs(t.Context(), RunLogs, cutoff, 10); err != nil || n != 1 {
		t.Fatalf("run = %d, %v", n, err)
	}
}

func TestManifestDigests_ListsEveryFileOfEveryVersion(t *testing.T) {
	f, _ := outputFixture(t)
	a, b := "sha256:"+strings.Repeat("a", 64), "sha256:"+strings.Repeat("b", 64)
	f.exec(`INSERT INTO workflows (id, name, created_by, created_at) VALUES ('wf_1', 'w', 'me', 1)`)
	f.exec(`INSERT INTO workflow_versions (workflow_id, version, digest, manifest, author_kind, author_id, origin, changed, created_at)
		VALUES ('wf_1', 1, ?, ?, 'user', 'me', 'client', '[]', 1)`, "sha256:"+strings.Repeat("c", 64),
		`{"schema_version":1,"components":[{"files":[{"sha256":"`+a+`"},{"sha256":"`+b+`"}]},{"files":[{"sha256":"`+a+`"}]}]}`)

	got := map[string]bool{}
	if err := f.db.Read(t.Context(), func(q Querier) error {
		return ManifestDigests(t.Context(), q, func(d string) { got[d] = true })
	}); err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || !got[a] || !got[b] {
		t.Fatalf("digests = %v", got)
	}
}
