package journal_test

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	v1 "github.com/Autarquico/codefabric/internal/api/v1"
	"github.com/Autarquico/codefabric/internal/domain"
	"github.com/Autarquico/codefabric/internal/support/clock"
	"github.com/Autarquico/codefabric/internal/worker/journal"
	"github.com/Autarquico/codefabric/internal/worker/state"
)

var t0 = time.Date(2026, 10, 10, 12, 0, 0, 0, time.UTC)

type sentEntry struct {
	run   string
	epoch string
	seq   uint64
	kind  v1.RunReportKind
}

// controller is a Reporter that remembers what it was sent. answer decides
// the outcome of a request; by default everything is applied.
type controller struct {
	mu       sync.Mutex
	requests []int
	got      []sentEntry
	answer   func(run string, req v1.RunReportRequest) (v1.RunReportResponse, error)
}

func (c *controller) ReportRun(_ context.Context, run string, req v1.RunReportRequest) (v1.RunReportResponse, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.answer != nil {
		if resp, err := c.answer(run, req); resp.Outcomes != nil || err != nil {
			return resp, err
		}
	}
	c.requests = append(c.requests, len(req.Entries))
	var resp v1.RunReportResponse
	for _, e := range req.Entries {
		c.got = append(c.got, sentEntry{run, e.WorkerEpoch, e.WorkerSeq, e.Kind})
		resp.Outcomes = append(resp.Outcomes, v1.RunReportOutcome{WorkerSeq: e.WorkerSeq, Result: v1.ReportApplied})
	}
	return resp, nil
}

func (c *controller) sent() []sentEntry {
	c.mu.Lock()
	defer c.mu.Unlock()
	return slices.Clone(c.got)
}

type fixture struct {
	t     *testing.T
	path  string
	db    *state.DB
	clock *clock.Fake
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	f := &fixture{t: t, path: filepath.Join(t.TempDir(), "state.db"), clock: clock.NewFake(t0)}
	f.reopen()
	return f
}

func (f *fixture) reopen() {
	f.t.Helper()
	if f.db != nil {
		_ = f.db.Close()
	}
	db, err := state.Open(f.t.Context(), f.path)
	if err != nil {
		f.t.Fatal(err)
	}
	f.db = db
	f.t.Cleanup(func() { _ = db.Close() })
}

func (f *fixture) sender(c journal.Reporter, j *journal.Journal) *journal.Sender {
	return &journal.Sender{DB: f.db, Reporter: c, Journal: j, Clock: f.clock, Log: slog.New(slog.DiscardHandler)}
}

// write journals the reports of a Run in one transaction, as the executor does
// with the change they tell about.
func (f *fixture) write(j *journal.Journal, run string, kinds ...v1.RunReportKind) []uint64 {
	f.t.Helper()
	k := state.Key{RunID: run, LeaseEpoch: 1}
	var seqs []uint64
	err := f.db.WithTx(f.t.Context(), func(tx *state.Tx) error {
		if _, err := state.InsertAssigned(f.t.Context(), tx, state.Run{
			Key: k, LeaseID: "lease_" + run, Spec: []byte("{}"), CreatedAt: t0, AcceptBy: t0,
		}); err != nil {
			return err
		}
		for _, kind := range kinds {
			e := v1.RunReportEntry{Kind: kind, At: t0}
			seq, err := j.AppendTx(f.t.Context(), tx, k, e)
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

func (f *fixture) pending() int {
	f.t.Helper()
	var n int
	err := f.db.Reader().QueryRowContext(f.t.Context(), `SELECT COUNT(*) FROM journal`).Scan(&n)
	if err != nil {
		f.t.Fatal(err)
	}
	return n
}

var lifecycle = []v1.RunReportKind{v1.ReportStarted, v1.ReportExited, v1.ReportStopped}

func TestScenario_WorkerExecutorTmux_ReenvioDelJournal(t *testing.T) {
	f := newFixture(t)
	first := journal.New("01J0000000000000000000000E1", f.clock)

	// 100 reports of several Runs, as valid sequences, none acknowledged.
	for i := range 33 {
		f.write(first, fmt.Sprintf("run_%02d", i), lifecycle...)
	}
	f.write(first, "run_33", v1.ReportStarted)
	if f.pending() != 100 {
		t.Fatalf("pending = %d", f.pending())
	}

	// The Controller cannot be reached; the daemon dies.
	down := &controller{answer: func(string, v1.RunReportRequest) (v1.RunReportResponse, error) {
		return v1.RunReportResponse{}, domain.NewError(domain.CodeControllerUnreachable, "down", nil)
	}}
	if _, err := f.sender(down, first).Flush(t.Context()); err == nil {
		t.Fatal("an unreachable Controller was not an error")
	}
	if f.pending() != 100 {
		t.Fatalf("pending after the failure = %d", f.pending())
	}

	// The daemon comes back with a new epoch and writes more.
	f.reopen()
	second := journal.New("01J0000000000000000000000E2", f.clock)
	f.write(second, "run_34", lifecycle...)

	up := &controller{}
	s := f.sender(up, second)
	for range 10 { // the 103 rows go in a few requests: more flushes than that is a loop
		n, err := s.Flush(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		if n == 0 {
			break
		}
	}

	old, fresh := map[uint64]bool{}, 0
	last := map[string]uint64{}
	for _, e := range up.sent() {
		if last[e.run] >= e.seq {
			t.Errorf("%s: seq %d after %d", e.run, e.seq, last[e.run])
		}
		last[e.run] = e.seq
		switch e.epoch {
		case "01J0000000000000000000000E1":
			old[e.seq] = true
		case "01J0000000000000000000000E2":
			fresh++
			if e.seq <= 100 {
				t.Errorf("the new epoch reuses seq %d", e.seq)
			}
		default:
			t.Errorf("unknown epoch %s", e.epoch)
		}
	}
	for seq := uint64(1); seq <= 100; seq++ {
		if !old[seq] {
			t.Errorf("(E1, %d) was not sent again", seq)
		}
	}
	if len(old) != 100 || fresh != 3 {
		t.Errorf("sent %d old and %d new entries", len(old), fresh)
	}
	for _, n := range up.requests {
		if n > v1.MaxReportEntries {
			t.Errorf("a request of %d entries", n)
		}
	}
	if f.pending() != 0 {
		t.Errorf("%d rows left after the Controller confirmed everything", f.pending())
	}
}

func TestSendingSplitsABatchByEntries(t *testing.T) {
	f := newFixture(t)
	j := journal.New("01J0000000000000000000000E1", f.clock)
	k := state.Key{RunID: "run_a", LeaseEpoch: 1}
	err := f.db.WithTx(t.Context(), func(tx *state.Tx) error {
		for range 150 {
			if _, err := j.AppendTx(t.Context(), tx, k, v1.RunReportEntry{Kind: v1.ReportStarted, At: t0}); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	c := &controller{}
	if _, err := f.sender(c, j).Flush(t.Context()); err != nil {
		t.Fatal(err)
	}
	if want := []int{64, 64, 22}; !slices.Equal(c.requests, want) {
		t.Errorf("requests = %v, want %v", c.requests, want)
	}
	if f.pending() != 0 {
		t.Errorf("%d rows left", f.pending())
	}
}

func TestSendingNeverExceedsTheByteBudgetOfARequest(t *testing.T) {
	f := newFixture(t)
	j := journal.New("01J0000000000000000000000E1", f.clock)
	k := state.Key{RunID: "run_a", LeaseEpoch: 1}
	fat := v1.RunReportEntry{
		Kind: v1.ReportStarted, At: t0, Message: strings.Repeat("y", v1.MaxReportMessageLen),
		Handle: &v1.RuntimeHandle{Runtime: "tmux", Ref: strings.Repeat("r", 240)},
	}
	err := f.db.WithTx(t.Context(), func(tx *state.Tx) error {
		for range 60 {
			if _, err := j.AppendTx(t.Context(), tx, k, fat); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	var sizes []int
	c := &controller{answer: func(_ string, req v1.RunReportRequest) (v1.RunReportResponse, error) {
		raw, _ := json.Marshal(req)
		sizes = append(sizes, len(raw))
		return v1.RunReportResponse{}, nil
	}}
	if _, err := f.sender(c, j).Flush(t.Context()); err != nil {
		t.Fatal(err)
	}
	if len(sizes) < 2 {
		t.Errorf("60 fat entries went in %d requests", len(sizes))
	}
	for _, n := range sizes {
		if n > v1.MaxReportBytes {
			t.Errorf("a request of %d bytes is over the budget of %d", n, v1.MaxReportBytes)
		}
	}
	if f.pending() != 0 {
		t.Errorf("%d rows left", f.pending())
	}
}

func TestARejectionFencesTheRunKeepsItsReportsAndDoesNotStopOtherRuns(t *testing.T) {
	f := newFixture(t)
	j := journal.New("01J0000000000000000000000E1", f.clock)
	f.write(j, "run_a", lifecycle...)
	f.write(j, "run_b", lifecycle...)
	c := &controller{answer: func(run string, req v1.RunReportRequest) (v1.RunReportResponse, error) {
		if run != "run_a" {
			return v1.RunReportResponse{}, nil
		}
		return v1.RunReportResponse{Outcomes: []v1.RunReportOutcome{
			{WorkerSeq: req.Entries[0].WorkerSeq, Result: v1.ReportApplied},
			{WorkerSeq: req.Entries[1].WorkerSeq, Result: v1.ReportRejected, Code: domain.CodeInvalidTransition},
			{WorkerSeq: req.Entries[2].WorkerSeq, Result: v1.ReportNotApplied},
		}}, nil
	}}
	if _, err := f.sender(c, j).Flush(t.Context()); err != nil {
		t.Fatal(err)
	}
	run, err := state.RunBy(t.Context(), f.db.Reader(), state.Key{RunID: "run_a", LeaseEpoch: 1})
	if err != nil || run.FencedCode != "CF_INVALID_TRANSITION" {
		t.Fatalf("run_a = %+v (%v)", run, err)
	}
	if f.pending() != 2 {
		t.Errorf("%d rows kept, want the rejected one and the one not applied", f.pending())
	}
	if other, _ := state.RunBy(t.Context(), f.db.Reader(), state.Key{RunID: "run_b", LeaseEpoch: 1}); other.FencedCode != "" {
		t.Error("run_b was fenced")
	}
	// Nothing more is sent for run_a.
	before := len(c.sent())
	if n, err := f.sender(c, j).Flush(t.Context()); err != nil || n != 0 || len(c.sent()) != before {
		t.Errorf("flush after fencing: %d %v", n, err)
	}
	if pending, _ := state.PendingJournal(t.Context(), f.db.Reader(), 10); len(pending) != 0 {
		t.Errorf("pending = %+v: a fenced Run is still offered", pending)
	}
}

func TestARequestRefusedAsMalformedOrUnknownFencesThatRunAloneWithoutDelayingTheOthers(t *testing.T) {
	for _, code := range []domain.Code{domain.CodeInvalidRequest, domain.CodeNotFound} {
		f := newFixture(t)
		j := journal.New("01J0000000000000000000000E1", f.clock)
		f.write(j, "run_a", lifecycle...)
		f.write(j, "run_b", lifecycle...)
		c := &controller{answer: func(run string, _ v1.RunReportRequest) (v1.RunReportResponse, error) {
			if run == "run_a" {
				return v1.RunReportResponse{}, domain.NewError(code, "at is out of range", nil)
			}
			return v1.RunReportResponse{}, nil
		}}
		if _, err := f.sender(c, j).Flush(t.Context()); err != nil {
			t.Fatalf("%s: %v: the Run is fenced, and there is nothing to back off for", code, err)
		}
		a, _ := state.RunBy(t.Context(), f.db.Reader(), state.Key{RunID: "run_a", LeaseEpoch: 1})
		b, _ := state.RunBy(t.Context(), f.db.Reader(), state.Key{RunID: "run_b", LeaseEpoch: 1})
		if a.FencedCode != string(code) || b.FencedCode != "" {
			t.Errorf("%s: fenced %q and %q", code, a.FencedCode, b.FencedCode)
		}
		if f.pending() != 3 {
			t.Errorf("%s: %d rows left, want the 3 of run_a kept", code, f.pending())
		}
	}
}

func TestAnErrorOfTheRequestThatSaysNothingOfTheEntriesFencesNothingAndLosesNothing(t *testing.T) {
	for _, code := range []domain.Code{domain.CodeStaleLease, domain.CodeUnauthenticated, domain.CodePolicyDenied, domain.CodeInternal, domain.CodeControllerUnreachable} {
		f := newFixture(t)
		j := journal.New("01J0000000000000000000000E1", f.clock)
		f.write(j, "run_a", lifecycle...)
		f.write(j, "run_b", lifecycle...)
		c := &controller{answer: func(run string, _ v1.RunReportRequest) (v1.RunReportResponse, error) {
			if run == "run_a" {
				return v1.RunReportResponse{}, domain.NewError(code, "no", nil)
			}
			return v1.RunReportResponse{}, nil
		}}
		if _, err := f.sender(c, j).Flush(t.Context()); err == nil {
			t.Fatalf("%s: the failure was not reported to the caller, which backs off", code)
		}
		for _, id := range []string{"run_a", "run_b"} {
			run, _ := state.RunBy(t.Context(), f.db.Reader(), state.Key{RunID: id, LeaseEpoch: 1})
			if run.FencedCode != "" {
				t.Errorf("%s: %s was fenced by an error of the request", code, id)
			}
		}
		if f.pending() != 3 {
			t.Errorf("%s: %d rows left, want the 3 of run_a", code, f.pending())
		}
	}
}

func TestAnAnswerThatDoesNotMatchWhatWasSentIsNotTrusted(t *testing.T) {
	f := newFixture(t)
	j := journal.New("01J0000000000000000000000E1", f.clock)
	f.write(j, "run_a", lifecycle...)
	c := &controller{answer: func(string, v1.RunReportRequest) (v1.RunReportResponse, error) {
		return v1.RunReportResponse{Outcomes: []v1.RunReportOutcome{{WorkerSeq: 1, Result: v1.ReportApplied}}}, nil
	}}
	if _, err := f.sender(c, j).Flush(t.Context()); err == nil {
		t.Error("a short answer was accepted")
	}
	if f.pending() != 3 {
		t.Errorf("%d rows left: the rows were deleted on an answer that did not cover them", f.pending())
	}
}

func TestRunSendsWhatIsNotifiedAndRetriesAfterABackoff(t *testing.T) {
	f := newFixture(t)
	j := journal.New("01J0000000000000000000000E1", f.clock)
	failures := 1
	c := &controller{answer: func(string, v1.RunReportRequest) (v1.RunReportResponse, error) {
		if failures > 0 {
			failures--
			return v1.RunReportResponse{}, domain.NewError(domain.CodeControllerUnreachable, "down", nil)
		}
		return v1.RunReportResponse{}, nil
	}}
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- f.sender(c, j).Run(ctx) }()
	defer func() {
		cancel()
		<-done
	}()

	f.write(j, "run_a", lifecycle...)
	j.Notify()
	// The first try fails; the sender waits on the clock for its backoff.
	waitTimers(t, f.clock, 2) // its ticker and the backoff
	if len(c.sent()) != 0 {
		t.Fatal("sent while failing")
	}
	f.clock.Advance(2 * time.Second)
	deadline := time.Now().Add(10 * time.Second)
	for f.pending() != 0 {
		if time.Now().After(deadline) {
			t.Fatalf("%d rows left; sent %d", f.pending(), len(c.sent()))
		}
		time.Sleep(5 * time.Millisecond)
	}
	if len(c.sent()) != 3 {
		t.Errorf("sent = %+v", c.sent())
	}
}

func waitTimers(t *testing.T, clk *clock.Fake, n int) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	if err := clk.BlockUntil(ctx, n); err != nil {
		t.Fatalf("waiting for %d timers: %v", n, err)
	}
}
