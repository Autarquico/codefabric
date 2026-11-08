package journal_test

import (
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"testing"
	"time"

	v1 "github.com/Autarquico/codefabric/internal/api/v1"
	"github.com/Autarquico/codefabric/internal/domain"
	"github.com/Autarquico/codefabric/internal/worker/journal"
	"github.com/Autarquico/codefabric/internal/worker/state"
)

// The scenarios of the spec journal-degradation-replay at component level: a
// real state.db and a fake clock, with only the Controller and the output.log
// side faked. The Replay has a short real counterpart in internal/e2e.
//
// Compaction of usage (stage 2) is not exercised: the seam Compact is nil until
// usage-attribution (Phase 6) writes usage, so there is nothing to compact yet.
// TestDegrade_Stage2CompactsBeforeItDegrades covers the seam itself.

func TestScenario_JournalDegradation_Etapa1(t *testing.T) {
	t.Run("raw output is discarded first and logs are left alone", func(t *testing.T) {
		f := newDegradeFixture(t)
		f.out.unsent = 900
		f.logLines(5, 100)
		logs := f.backlog().Logs

		f.out.unsent += 400 // output keeps coming in
		f.once()

		if f.out.shed == 0 || f.backlog().Logs != logs {
			t.Fatalf("shed %d bytes of output; logs went from %d to %d", f.out.shed, logs, f.backlog().Logs)
		}
	})
	t.Run("logs are discarded, the oldest first, once there is no output left", func(t *testing.T) {
		f := newDegradeFixture(t)
		f.out.unsent = 200
		f.logLines(10, 100)

		f.once()

		left, err := state.PendingLogs(t.Context(), f.db.Reader(), 100)
		if err != nil {
			t.Fatal(err)
		}
		if f.out.unsent != 0 || len(left) == 0 || len(left) == 10 || left[len(left)-1].Seq != 10 {
			t.Fatalf("output left %d, %d of 10 lines left (newest %v)", f.out.unsent, len(left), left)
		}
	})
}

func TestScenario_JournalDegradation_Etapa2(t *testing.T) {
	f := newDegradeFixture(t)
	f.reports(8, 150) // events keep growing past max_size, under hard_max
	f.out.unsent = 500
	f.logLines(4, 100)

	f.once()

	if len(f.reasons) != 1 || f.reasons[0] == "" || f.d.Backlog().Reason != f.reasons[0] {
		t.Fatalf("the Worker is not degraded: reasons %v, backlog %+v", f.reasons, f.d.Backlog())
	}
	counts, _ := state.JournalCounts(t.Context(), f.db.Reader())
	if counts[state.Key{RunID: "run_1", LeaseEpoch: 1}] != 8 {
		t.Errorf("reports left = %v, want all 8: they are never discarded", counts)
	}
}

func TestScenario_JournalDegradation_Etapa3(t *testing.T) {
	f := newDegradeFixture(t)
	f.out.unsent = 700
	f.logLines(6, 100)
	f.once() // stage 1
	f.reports(25, 150)
	f.logLines(3, 100)
	f.out.unsent += 300

	f.once() // hard_max

	sealed := f.sealed()
	if len(sealed) != 1 {
		t.Fatalf("%d JournalTruncated to report, want 1", len(sealed))
	}
	if got := sealed[0]; got.LogLines == 0 || got.LogsFrom == 0 || got.LogsFrom > got.LogsTo {
		t.Errorf("truncation = %+v: it must carry the range of what was discarded", got)
	}
	counts, _ := state.JournalCounts(t.Context(), f.db.Reader())
	if counts[state.Key{RunID: "run_1", LeaseEpoch: 1}] != 25 {
		t.Errorf("reports of Run state left = %v, want all 25", counts)
	}
}

// TestScenario_JournalDegradation_Replay: a Controller out of reach for 10
// simulated minutes while the Worker goes on reporting and logging; when it
// comes back everything arrives once and in order.
func TestScenario_JournalDegradation_Replay(t *testing.T) {
	f := newFixture(t)
	jrn := journal.New("E1", f.clock)
	down := true
	ctl := &controller{answer: func(string, v1.RunReportRequest) (v1.RunReportResponse, error) {
		if down {
			return v1.RunReportResponse{}, domain.NewError(domain.CodeControllerUnreachable, "partitioned", nil)
		}
		return v1.RunReportResponse{}, nil
	}}
	logCtl := &logController{fail: errors.New("partitioned")}
	logs := journal.NewLogs(journal.LogsOptions{DB: f.db, Epoch: "E1", Shipper: logCtl, Clock: f.clock, Log: slog.New(slog.DiscardHandler)})
	sender := f.sender(ctl, jrn)

	const minutes = 10
	want := map[string][]uint64{}
	for m := range minutes {
		for _, run := range []string{"run_a", "run_b"} {
			if m == 0 {
				want[run] = f.write(jrn, run, v1.ReportStarted)
				continue
			}
			want[run] = append(want[run], f.appendMore(jrn, run, v1.ReportConfigResolved)...)
		}
		if err := logs.Store(t.Context(), lines(5, fmt.Sprintf("minute %d", m))); err != nil {
			t.Fatal(err)
		}
		if _, err := sender.Flush(t.Context()); err == nil {
			t.Fatal("a partitioned Controller was not an error")
		}
		if _, err := logs.Flush(t.Context()); err == nil {
			t.Fatal("a partitioned log shipper was not an error")
		}
		f.clock.Advance(time.Minute)
	}
	if n := f.pending(); n != 2*minutes {
		t.Fatalf("pending = %d, want %d: nothing is lost while partitioned", n, 2*minutes)
	}

	down, logCtl.fail = false, nil
	for f.pending() > 0 {
		if _, err := sender.Flush(t.Context()); err != nil {
			t.Fatal(err)
		}
	}
	for pendingLogs(t, f.db) > 0 {
		if _, err := logs.Flush(t.Context()); err != nil {
			t.Fatal(err)
		}
	}

	byRun := map[string][]uint64{}
	for _, s := range ctl.sent() {
		byRun[s.run] = append(byRun[s.run], s.seq)
	}
	for run, seqs := range want {
		if !slices.Equal(byRun[run], seqs) {
			t.Errorf("%s: the Controller got worker_seq %v, want %v once each and in order", run, byRun[run], seqs)
		}
	}
	var seen []uint64
	for _, b := range logCtl.batches {
		for _, e := range b {
			seen = append(seen, e.WorkerSeq)
		}
	}
	if len(seen) != 5*minutes || !slices.IsSorted(seen) || len(slices.Compact(slices.Clone(seen))) != len(seen) {
		t.Errorf("log seqs = %v, want %d, increasing and with no repeats", seen, 5*minutes)
	}
}

// appendMore journals reports of a Run that already exists.
func (f *fixture) appendMore(j *journal.Journal, run string, kinds ...v1.RunReportKind) []uint64 {
	f.t.Helper()
	var seqs []uint64
	err := f.db.WithTx(f.t.Context(), func(tx *state.Tx) error {
		for _, kind := range kinds {
			seq, err := j.AppendTx(f.t.Context(), tx, state.Key{RunID: run, LeaseEpoch: 1}, v1.RunReportEntry{Kind: kind, At: t0})
			if err != nil {
				return err
			}
			seqs = append(seqs, seq)
		}
		return nil
	})
	if err != nil {
		f.t.Fatal(err)
	}
	return seqs
}
