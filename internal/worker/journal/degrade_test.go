package journal_test

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"testing"
	"time"

	v1 "github.com/Autarquico/codefabric/internal/api/v1"
	"github.com/Autarquico/codefabric/internal/worker/journal"
	"github.com/Autarquico/codefabric/internal/worker/state"
)

// output is the output.log side as the degrader sees it: a number of unsent
// bytes that it can give up.
type output struct {
	unsent uint64
	shed   uint64
	db     *state.DB // where the real one records what it skipped, in the open truncation
}

func (o *output) Unsent(context.Context) (uint64, error) { return o.unsent, nil }

func (o *output) Shed(_ context.Context, need uint64) (uint64, error) {
	n := min(need, o.unsent)
	o.unsent, o.shed = o.unsent-n, o.shed+n
	if o.db == nil || n == 0 {
		return n, nil
	}
	return n, o.db.WithTx(context.Background(), func(tx *state.Tx) error {
		return state.RecordShed(context.Background(), tx, state.Shed{OutputBytes: n, RunIDs: []string{"run_1"}}, time.Now())
	})
}

type degradeFixture struct {
	*fixture
	out      *output
	d        *journal.Degrader
	reasons  []string
	compacts []uint64
}

const (
	maxSize = 1000
	hardMax = 3000
)

func newDegradeFixture(t *testing.T) *degradeFixture {
	t.Helper()
	f := &degradeFixture{fixture: newFixture(t), out: &output{}}
	f.out.db = f.db
	f.d = journal.NewDegrader(journal.DegradeOptions{
		DB: f.db, Output: f.out, Clock: f.clock, Log: slog.New(slog.DiscardHandler),
		Limits:     journal.Limits{MaxSize: maxSize, HardMax: hardMax},
		OnDegraded: func(reason string) { f.reasons = append(f.reasons, reason) },
	})
	return f
}

func (f *degradeFixture) once() {
	f.t.Helper()
	if err := f.d.Once(f.t.Context()); err != nil {
		f.t.Fatal(err)
	}
}

// reports adds n reports of about size bytes each.
func (f *degradeFixture) reports(n, size int) {
	f.t.Helper()
	jrn := journal.New("E1", f.clock)
	for range n {
		if err := f.db.WithTx(f.t.Context(), func(tx *state.Tx) error {
			_, err := jrn.AppendTx(f.t.Context(), tx, state.Key{RunID: "run_1", LeaseEpoch: 1}, v1.RunReportEntry{
				Kind: v1.ReportStarted, Message: strings.Repeat("r", size),
			})
			return err
		}); err != nil {
			f.t.Fatal(err)
		}
	}
}

func (f *degradeFixture) logLines(n, size int) {
	f.t.Helper()
	logs := journal.NewLogs(journal.LogsOptions{DB: f.db, Epoch: "E1", Shipper: &logController{}, Clock: f.clock, Log: slog.New(slog.DiscardHandler)})
	if err := logs.Store(f.t.Context(), lines(n, strings.Repeat("l", size))); err != nil {
		f.t.Fatal(err)
	}
}

func (f *degradeFixture) backlog() state.Backlog {
	f.t.Helper()
	b, err := state.JournalBacklog(f.t.Context(), f.db.Reader())
	if err != nil {
		f.t.Fatal(err)
	}
	return b
}

func (f *degradeFixture) sealed() []state.Truncation {
	f.t.Helper()
	got, err := state.SealedTruncations(f.t.Context(), f.db.Reader(), 100)
	if err != nil {
		f.t.Fatal(err)
	}
	return got
}

func TestDegrade_BelowMaxSizeNothingIsGivenUp(t *testing.T) {
	f := newDegradeFixture(t)
	f.out.unsent = 400
	f.logLines(3, 100)
	f.once()

	if f.out.shed != 0 || f.backlog().Logs == 0 || len(f.reasons) != 0 || f.d.Backlog().Reason != "" {
		t.Fatalf("shed %d, logs %d, reasons %v", f.out.shed, f.backlog().Logs, f.reasons)
	}
	logs := uint64(f.backlog().Logs) //nolint:gosec // a small count
	if got := f.d.Backlog().Bytes; got != 400+logs {
		t.Errorf("backlog = %d", got)
	}
}

func TestDegrade_Stage1GivesUpOutputBeforeLogs(t *testing.T) {
	f := newDegradeFixture(t)
	f.out.unsent = 900
	f.logLines(5, 100) // about 510 bytes
	logs := f.backlog().Logs

	f.once()

	if f.out.shed == 0 || f.backlog().Logs != logs {
		t.Fatalf("shed %d bytes of output and logs went from %d to %d: output goes first, and alone if it is enough", f.out.shed, logs, f.backlog().Logs)
	}
	if got := f.d.Backlog(); got.Bytes >= maxSize || got.Reason != "" {
		t.Errorf("backlog = %+v, want it back under max_size and not degraded", got)
	}
}

func TestDegrade_Stage1GivesUpTheOldestLogsOnceThereIsNoOutputLeft(t *testing.T) {
	f := newDegradeFixture(t)
	f.out.unsent = 200
	f.logLines(10, 100) // about 1020 bytes

	f.once()

	if f.out.unsent != 0 {
		t.Errorf("%d bytes of output are left: they go before any log", f.out.unsent)
	}
	left, err := state.PendingLogs(t.Context(), f.db.Reader(), 100)
	if err != nil || len(left) == 0 || len(left) == 10 {
		t.Fatalf("%d lines left (%v): some of the oldest, not all, had to go", len(left), err)
	}
	if left[len(left)-1].Seq != 10 {
		t.Errorf("the newest line is %d, want 10", left[len(left)-1].Seq)
	}
	if got := f.d.Backlog(); got.Bytes >= maxSize || got.Reason != "" {
		t.Errorf("backlog = %+v", got)
	}
}

func TestDegrade_Stage2ReportsAloneAtMaxSizeDegradeTheWorkerAndAreNeverGivenUp(t *testing.T) {
	f := newDegradeFixture(t)
	f.reports(8, 150) // over max_size, under hard_max
	f.out.unsent = 500
	f.logLines(4, 100)

	f.once()

	if f.out.unsent != 0 || f.backlog().Logs != 0 {
		t.Errorf("output %d, logs %d: both are given up before the Worker degrades", f.out.unsent, f.backlog().Logs)
	}
	if n, _ := state.JournalCounts(t.Context(), f.db.Reader()); n[state.Key{RunID: "run_1", LeaseEpoch: 1}] != 8 {
		t.Fatalf("reports left = %v, want all 8", n)
	}
	if len(f.reasons) != 1 || f.reasons[0] == "" || f.d.Backlog().Reason != f.reasons[0] {
		t.Fatalf("reasons = %v, backlog = %+v", f.reasons, f.d.Backlog())
	}
	if got := f.sealed(); len(got) != 1 || got[0].LogLines != 4 || got[0].OutputBytes != 500 {
		t.Errorf("truncations = %+v, want the 4 lines and 500 bytes of this pass sealed below hard_max", got)
	}

	// It says so once, not on every pass.
	f.once()
	f.once()
	if len(f.reasons) != 1 {
		t.Errorf("reasons = %v, want one", f.reasons)
	}
}

func TestDegrade_TheWorkerRecoversWhenTheControllerConfirmsTheReports(t *testing.T) {
	f := newDegradeFixture(t)
	f.reports(8, 150)
	f.once()
	if len(f.reasons) != 1 || f.reasons[0] == "" {
		t.Fatalf("reasons = %v, want degraded", f.reasons)
	}

	pending, err := state.PendingJournal(t.Context(), f.db.Reader(), 100)
	if err != nil {
		t.Fatal(err)
	}
	var seqs []uint64
	for _, r := range pending {
		seqs = append(seqs, r.Seq)
	}
	if err := f.db.WithTx(t.Context(), func(tx *state.Tx) error { return state.DeleteJournal(t.Context(), tx, seqs) }); err != nil {
		t.Fatal(err)
	}
	f.once()

	if len(f.reasons) != 2 || f.reasons[1] != "" || f.d.Backlog().Reason != "" {
		t.Fatalf("reasons = %v, backlog = %+v: degraded should have been cleared", f.reasons, f.d.Backlog())
	}
}

func TestDegrade_Stage2CompactsBeforeItDegrades(t *testing.T) {
	f := newDegradeFixture(t)
	f.reports(8, 150)
	f.d = journal.NewDegrader(journal.DegradeOptions{
		DB: f.db, Output: f.out, Clock: f.clock, Log: slog.New(slog.DiscardHandler), Limits: journal.Limits{MaxSize: maxSize, HardMax: hardMax},
		OnDegraded: func(reason string) { f.reasons = append(f.reasons, reason) },
		Compact: func(context.Context) (uint64, error) {
			f.compacts = append(f.compacts, 1)
			return uint64(f.backlog().Reports), nil //nolint:gosec // a small count: everything is freed
		},
	})
	f.once()

	if len(f.compacts) != 1 || len(f.reasons) != 0 {
		t.Fatalf("compacted %d times, reasons %v: freeing the space leaves the Worker as it was", len(f.compacts), f.reasons)
	}
}

func TestDegrade_Stage3AtHardMaxSealsTheRangeOfWhatWasGivenUpAndKeepsEveryReport(t *testing.T) {
	f := newDegradeFixture(t)
	f.out.unsent = 700
	f.logLines(6, 100)
	f.once() // stage 1: the output goes, some logs maybe
	f.reports(25, 150)
	f.logLines(3, 100)
	f.out.unsent += 300
	f.clock.Advance(time.Minute) // the first pass already reported what it gave up

	f.once()

	sealed := f.sealed()
	if len(sealed) != 2 {
		t.Fatalf("%d truncations sealed, want 2 (stage 1 and hard_max)", len(sealed))
	}
	got := sealed[1]
	if got.LogLines == 0 || got.LogsFrom == 0 || got.LogsFrom > got.LogsTo {
		t.Errorf("truncation = %+v: it needs the range of the lines given up", got)
	}
	if n, _ := state.JournalCounts(t.Context(), f.db.Reader()); n[state.Key{RunID: "run_1", LeaseEpoch: 1}] != 25 {
		t.Errorf("reports left = %v, want all 25", n)
	}
}

func TestDegrade_NoKindOfReportIsEverGivenUp(t *testing.T) {
	kinds := []v1.RunReportKind{
		v1.ReportStarted, v1.ReportExited, v1.ReportPrepareFailed, v1.ReportStopped, v1.ReportSealed, v1.ReportPublished,
		v1.ReportPublicationFailed, v1.ReportConfigResolved, v1.ReportOrphanPreserved, v1.ReportInputRequested, v1.ReportInputDelivered,
	}
	for _, k := range kinds {
		if !k.IsValid() {
			t.Fatalf("%s is not a kind", k)
		}
	}
	f := newDegradeFixture(t)
	jrn := journal.New("E1", f.clock)
	for _, kind := range kinds {
		for range 40 {
			if err := f.db.WithTx(t.Context(), func(tx *state.Tx) error {
				_, err := jrn.AppendTx(t.Context(), tx, state.Key{RunID: "run_" + string(kind), LeaseEpoch: 1}, v1.RunReportEntry{
					Kind: kind, Message: strings.Repeat("x", 10),
				})
				return err
			}); err != nil {
				t.Fatal(err)
			}
		}
	}
	f.out.unsent = 5000
	f.logLines(50, 100)
	if f.backlog().Reports < hardMax/2 {
		t.Fatalf("the reports are %d bytes: too few to be a test", f.backlog().Reports)
	}

	f.once()
	f.clock.Advance(time.Hour)
	f.once()

	counts, err := state.JournalCounts(t.Context(), f.db.Reader())
	if err != nil {
		t.Fatal(err)
	}
	for _, kind := range kinds {
		if n := counts[state.Key{RunID: "run_" + string(kind), LeaseEpoch: 1}]; n != 40 {
			t.Errorf("%s: %d reports left, want 40", kind, n)
		}
	}
}

func TestDegrade_ATruncationIsSealedAtMostOnceAMinuteSoTheControllerIsNotFlooded(t *testing.T) {
	f := newDegradeFixture(t)
	f.reports(30, 150) // over hard_max by itself
	f.logLines(2, 100)
	f.once()
	if len(f.sealed()) != 1 {
		t.Fatalf("%d sealed after the first pass, want 1", len(f.sealed()))
	}

	f.clock.Advance(20 * time.Second)
	f.logLines(2, 100)
	f.once()
	if len(f.sealed()) != 1 {
		t.Fatalf("%d sealed 20 s later, want still 1", len(f.sealed()))
	}

	f.clock.Advance(time.Minute)
	f.once()
	if got := f.sealed(); len(got) != 2 || got[1].LogLines != 2 {
		t.Fatalf("sealed = %+v, want a second one with the 2 lines given up since", got)
	}
}

func TestDegrade_HardMaxWithNothingGivenUpReportsNoTruncation(t *testing.T) {
	f := newDegradeFixture(t)
	f.reports(30, 150)
	f.once()
	if len(f.sealed()) != 0 {
		t.Fatalf("sealed = %+v: there is no range to tell", f.sealed())
	}
	if f.d.Backlog().Reason == "" {
		t.Error("the Worker is not degraded")
	}
}

func TestDegrade_RunPassesOnTheClockAndStopsWithItsContext(t *testing.T) {
	f := newDegradeFixture(t)
	f.reports(8, 150)
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- f.d.Run(ctx) }()

	deadline := time.Now().Add(5 * time.Second)
	for f.d.Backlog().Reason == "" {
		if time.Now().After(deadline) {
			t.Fatal("the first pass did not degrade the Worker")
		}
		time.Sleep(time.Millisecond)
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

func TestDegrade_APassThatGivesUpOutputBelowHardMaxIsReported(t *testing.T) {
	f := newDegradeFixture(t)
	f.out.unsent = 2000 // between max_size and hard_max
	f.once()

	got := f.sealed()
	if len(got) != 1 || got[0].OutputBytes == 0 {
		t.Fatalf("truncations = %+v, want one with output_bytes > 0", got)
	}
}

func TestDegrade_AtHardMaxNothingIsDiscardedButItIsSaidAsAnErrorAndInTheReason(t *testing.T) {
	f := newDegradeFixture(t)
	var logs bytes.Buffer
	f.d = journal.NewDegrader(journal.DegradeOptions{
		DB: f.db, Output: f.out, Clock: f.clock, Log: slog.New(slog.NewTextHandler(&logs, nil)),
		Limits:     journal.Limits{MaxSize: maxSize, HardMax: hardMax},
		OnDegraded: func(reason string) { f.reasons = append(f.reasons, reason) },
	})
	f.reports(30, 150)
	f.once()

	if !strings.Contains(logs.String(), "level=ERROR") || !strings.Contains(logs.String(), "hard_max") {
		t.Errorf("log = %q, want an error that names hard_max", logs.String())
	}
	if r := f.d.Backlog().Reason; !strings.Contains(r, "hard_max") {
		t.Errorf("reason = %q, want it to name hard_max", r)
	}
	if n, _ := state.JournalCounts(t.Context(), f.db.Reader()); n[state.Key{RunID: "run_1", LeaseEpoch: 1}] != 30 {
		t.Errorf("reports left = %v, want all 30", n)
	}
}
