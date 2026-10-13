package store

import (
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	v1 "github.com/Autarquico/codefabric/internal/api/v1"
	"github.com/Autarquico/codefabric/internal/domain"
)

// seedEvents appends n events of the types, at one second apart on the clock,
// at the given levels in turn, and returns their sequences.
func seedEvents(t *testing.T, db *DB, clk interface{ Advance(time.Duration) }, n int, types []domain.EventType, levels []string) []uint64 {
	t.Helper()
	var seqs []uint64
	for i := range n {
		clk.Advance(time.Second)
		e := controllerEvent(types[i%len(types)])
		e.Level = levels[i%len(levels)]
		if err := db.WithTx(t.Context(), func(tx *Tx) error {
			seq, _, err := tx.AppendEvent(t.Context(), e)
			seqs = append(seqs, seq)
			return err
		}); err != nil {
			t.Fatal(err)
		}
	}
	return seqs
}

func sequences(events []domain.Event) []uint64 {
	var out []uint64
	for _, e := range events {
		out = append(out, e.Sequence)
	}
	return out
}

func TestEventsAfter_CeilingLevelAndSince(t *testing.T) {
	f := newWorkerFixture(t)
	seqs := seedEvents(t, f.db, f.clk, 8,
		[]domain.EventType{domain.EventRunQueued, domain.EventRunFailed},
		[]string{domain.LevelInfo, domain.LevelWarn, domain.LevelDebug, domain.LevelError})

	read := func(after, through uint64, limit int, filter EventFilter) []uint64 {
		var got []domain.Event
		if err := f.db.Read(t.Context(), func(q Querier) (err error) {
			got, err = EventsAfter(t.Context(), q, after, through, limit, filter)
			return err
		}); err != nil {
			t.Fatal(err)
		}
		return sequences(got)
	}
	if got := read(0, seqs[4], 100, EventFilter{}); !slices.Equal(got, seqs[:5]) {
		t.Errorf("through %d = %v, want %v", seqs[4], got, seqs[:5])
	}
	if got := read(seqs[1], seqs[6], 2, EventFilter{}); !slices.Equal(got, seqs[2:4]) {
		t.Errorf("after/limit = %v", got)
	}
	if got := read(0, 0, 100, EventFilter{}); len(got) != 0 {
		t.Errorf("a ceiling of 0 is a ceiling: %v", got)
	}
	// levels cycle info, warn, debug, error, info, warn, debug, error
	if got := read(0, seqs[7], 100, EventFilter{MinLevel: domain.LevelWarn}); !slices.Equal(got, []uint64{seqs[1], seqs[3], seqs[5], seqs[7]}) {
		t.Errorf("level warn = %v", got)
	}
	if got := read(0, seqs[7], 100, EventFilter{MinLevel: domain.LevelDebug}); len(got) != 8 {
		t.Errorf("level debug = %v", got)
	}
	if got := read(0, seqs[7], 100, EventFilter{Types: []domain.EventType{domain.EventRunFailed}, MinLevel: domain.LevelError}); !slices.Equal(got, []uint64{seqs[3], seqs[7]}) {
		t.Errorf("type and level = %v", got)
	}
	// Event i was received at epoch0 + (i+1) seconds.
	if got := read(0, seqs[7], 100, EventFilter{Since: epoch0.Add(7 * time.Second)}); !slices.Equal(got, seqs[6:]) {
		t.Errorf("since = %v, want %v", got, seqs[6:])
	}
}

func TestEventsTail_IsTheLastOnesOldestFirst(t *testing.T) {
	f := newWorkerFixture(t)
	seqs := seedEvents(t, f.db, f.clk, 6, []domain.EventType{domain.EventRunQueued, domain.EventRunFailed}, []string{domain.LevelInfo})
	var got []domain.Event
	if err := f.db.Read(t.Context(), func(q Querier) (err error) {
		got, err = EventsTail(t.Context(), q, seqs[4], 2, EventFilter{})
		return err
	}); err != nil || !slices.Equal(sequences(got), seqs[3:5]) {
		t.Fatalf("tail = %v, %v", sequences(got), err)
	}
	if err := f.db.Read(t.Context(), func(q Querier) (err error) {
		got, err = EventsTail(t.Context(), q, seqs[5], 5, EventFilter{Types: []domain.EventType{domain.EventRunFailed}})
		return err
	}); err != nil || !slices.Equal(sequences(got), []uint64{seqs[1], seqs[3], seqs[5]}) {
		t.Fatalf("tail by type = %v, %v", sequences(got), err)
	}
}

func TestEventBounds(t *testing.T) {
	f := newWorkerFixture(t)
	bounds := func() (latest, purged uint64) {
		t.Helper()
		if err := f.db.Read(t.Context(), func(q Querier) (err error) {
			latest, purged, err = EventBounds(t.Context(), q)
			return err
		}); err != nil {
			t.Fatal(err)
		}
		return latest, purged
	}
	if latest, purged := bounds(); latest != 0 || purged != 0 {
		t.Fatalf("empty = %d, %d", latest, purged)
	}
	seqs := seedEvents(t, f.db, f.clk, 3, []domain.EventType{domain.EventRunQueued}, []string{domain.LevelInfo})
	if _, err := f.db.writer.ExecContext(t.Context(), `INSERT INTO controller_meta (key, value) VALUES (?, '2')`, purgedThroughKey); err != nil {
		t.Fatal(err)
	}
	if latest, purged := bounds(); latest != seqs[2] || purged != 2 {
		t.Fatalf("bounds = %d, %d", latest, purged)
	}
}

func TestAppendEvent_RefusesAnEventAboveTheCap(t *testing.T) {
	f := newWorkerFixture(t)
	e := controllerEvent(domain.EventRunQueued)
	e.Message = strings.Repeat("m", MaxEventBytes)
	err := f.db.WithTx(t.Context(), func(tx *Tx) error {
		_, _, err := tx.AppendEvent(t.Context(), e)
		return err
	})
	if err != nil {
		t.Fatalf("an event at the cap: %v", err)
	}
	e.Data = map[string]any{"k": "v"}
	err = f.db.WithTx(t.Context(), func(tx *Tx) error {
		_, _, err := tx.AppendEvent(t.Context(), e)
		return err
	})
	if !errors.Is(err, ErrInvalidEvent) {
		t.Fatalf("an event above the cap: %v", err)
	}
}

func TestTerminalRunStatesMatchTheDomain(t *testing.T) {
	var terminal []string
	for _, s := range []domain.RunState{
		domain.RunQueued, domain.RunPreparing, domain.RunRunning, domain.RunWaitingInput, domain.RunCheckpointing,
		domain.RunSuspended, domain.RunCompleted, domain.RunFailed, domain.RunCancelled, domain.RunLost,
	} {
		if s.IsTerminal() {
			terminal = append(terminal, string(s))
		}
	}
	slices.Sort(terminal)
	if got := sqlStates(terminalRunStates); !slices.Equal(got, terminal) {
		t.Errorf("terminalRunStates = %v, want %v", got, terminal)
	}
}

func TestPsRunsAndLiveRunOfTask(t *testing.T) {
	f := newLeaseFixture(t)
	queued := f.queuedRun(1)
	done := f.queuedRun(2)
	f.tx(func(tx *Tx) error {
		_, err := tx.Exec(t.Context(), `UPDATE runs SET state = 'cancelled' WHERE id = ?`, string(done.ID))
		return err
	})
	var (
		runs  []Run
		tasks map[domain.ID]TaskRef
	)
	if err := f.db.Read(t.Context(), func(q Querier) (err error) {
		runs, tasks, err = PsRuns(t.Context(), q)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if len(runs) != 1 || runs[0].ID != queued.ID || tasks["task_1"].Name != "t" {
		t.Fatalf("PsRuns = %v, %v", runs, tasks)
	}
	// A finished Run whose session is still active is in ps too.
	f.tx(func(tx *Tx) error {
		_, err := tx.Exec(t.Context(), `UPDATE runs SET runtime = 'active' WHERE id = ?`, string(done.ID))
		return err
	})
	_ = f.db.Read(t.Context(), func(q Querier) (err error) {
		runs, _, err = PsRuns(t.Context(), q)
		return err
	})
	if len(runs) != 2 {
		t.Fatalf("PsRuns with an active runtime = %d Runs", len(runs))
	}

	var live Run
	if err := f.db.Read(t.Context(), func(q Querier) (err error) {
		live, err = LiveRunOfTask(t.Context(), q, "task_1")
		return err
	}); err != nil || live.ID != queued.ID {
		t.Fatalf("LiveRunOfTask = %v, %v", live.ID, err)
	}
	err := f.db.Read(t.Context(), func(q Querier) error {
		_, err := LiveRunOfTask(t.Context(), q, "task_9")
		return err
	})
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("a Task with no Run: %v", err)
	}
}

func TestRecordHeartbeat_KeepsWhatTheWorkerAnnouncesForAttach(t *testing.T) {
	f := newLeaseFixture(t)
	w := f.alpha
	report := WorkerReport{Epoch: "e", SSHTarget: "alex@box:22", AttachBinaries: map[string]string{"tmux": "/usr/bin/tmux"}}
	f.tx(func(tx *Tx) error {
		return tx.RecordHeartbeat(t.Context(), w.ID, report, v1.WorkerResources{}, v1.Health{Status: v1.HealthOK}, f.clk.Now())
	})
	got := f.read(w.ID)
	if got.Report.SSHTarget != "alex@box:22" || got.Report.AttachBinaries["tmux"] != "/usr/bin/tmux" {
		t.Fatalf("report = %+v", got.Report)
	}
	// A heartbeat that announces nothing clears it.
	f.tx(func(tx *Tx) error {
		return tx.RecordHeartbeat(t.Context(), w.ID, WorkerReport{Epoch: "e"}, v1.WorkerResources{}, v1.Health{Status: v1.HealthOK}, f.clk.Now())
	})
	if got = f.read(w.ID); got.Report.SSHTarget != "" || got.Report.AttachBinaries != nil {
		t.Fatalf("report = %+v", got.Report)
	}
}
