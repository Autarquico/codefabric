package journal_test

import (
	"bytes"
	"context"
	"encoding/json"
	"reflect"
	"strings"
	"sync"
	"testing"

	v1 "github.com/Autarquico/codefabric/internal/api/v1"
	"github.com/Autarquico/codefabric/internal/domain"
	"github.com/Autarquico/codefabric/internal/worker/journal"
	"github.com/Autarquico/codefabric/internal/worker/state"
)

// beta5Request is a report as a Controller without v1.FeatureRunAnswer decodes
// it: the entry has no `answer`, and a field it does not know is refused.
func beta5Request(t *testing.T) reflect.Type {
	t.Helper()
	var fields []reflect.StructField
	entry := reflect.TypeFor[v1.RunReportEntry]()
	for f := range entry.Fields() {
		if f.Name != "Answer" {
			fields = append(fields, f)
		}
	}
	request := reflect.TypeFor[v1.RunReportRequest]()
	var out []reflect.StructField
	for f := range request.Fields() {
		if f.Name == "Entries" {
			f.Type = reflect.SliceOf(reflect.StructOf(fields))
		}
		out = append(out, f)
	}
	return reflect.StructOf(out)
}

func strictDecode(raw []byte, into any) error {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	return dec.Decode(into)
}

// wire is a Reporter that keeps the answer of each entry as it was sent and
// refuses the request as the decoder of the Controller does.
type wire struct {
	mu      sync.Mutex
	decoder reflect.Type // the request the Controller decodes
	answers []string
	refused int
}

func (w *wire) ReportRun(_ context.Context, _ string, req v1.RunReportRequest) (v1.RunReportResponse, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	raw, err := json.Marshal(req)
	if err != nil {
		return v1.RunReportResponse{}, err
	}
	if err := strictDecode(raw, reflect.New(w.decoder).Interface()); err != nil {
		w.refused++
		return v1.RunReportResponse{}, err
	}
	var resp v1.RunReportResponse
	for _, e := range req.Entries {
		w.answers = append(w.answers, e.Answer)
		resp.Outcomes = append(resp.Outcomes, v1.RunReportOutcome{WorkerSeq: e.WorkerSeq, Result: v1.ReportApplied})
	}
	return resp, nil
}

func (f *fixture) writeExitedWithAnswer(j *journal.Journal, run, answer string) {
	f.t.Helper()
	k := state.Key{RunID: run, LeaseEpoch: 1}
	err := f.db.WithTx(f.t.Context(), func(tx *state.Tx) error {
		if _, err := state.InsertAssigned(f.t.Context(), tx, state.Run{
			Key: k, LeaseID: "lease_" + run, Spec: []byte("{}"), CreatedAt: t0, AcceptBy: t0,
		}); err != nil {
			return err
		}
		_, err := j.AppendTx(f.t.Context(), tx, k, v1.RunReportEntry{Kind: v1.ReportExited, At: t0, Answer: answer})
		return err
	})
	if err != nil {
		f.t.Fatal(err)
	}
}

func TestAnExitedJournaledBeforeAnyHeartbeatWaitsForItAndThenGoesWithItsAnswer(t *testing.T) {
	f := newFixture(t)
	j := journal.New("01J0000000000000000000000E1", f.clock)
	f.writeExitedWithAnswer(j, "run_a", "all done")
	w := &wire{decoder: reflect.TypeFor[v1.RunReportRequest]()}
	features := &journal.ControllerFeatures{}
	s := f.sender(w, j)
	s.Features = features

	if n, err := s.Flush(t.Context()); err != nil || n != 0 || f.pending() != 1 || len(w.answers) != 0 {
		t.Fatalf("before the first heartbeat: sent %d (%v), %d pending, %d requests", n, err, f.pending(), len(w.answers))
	}
	features.Set([]string{v1.FeatureRunAnswer})
	if _, err := s.Flush(t.Context()); err != nil {
		t.Fatal(err)
	}
	if len(w.answers) != 1 || w.answers[0] != "all done" || f.pending() != 0 {
		t.Errorf("answers sent = %q, %d pending", w.answers, f.pending())
	}
}

func TestAControllerWithoutTheFeatureGetsTheReportWithoutAnswerAndAcceptsIt(t *testing.T) {
	f := newFixture(t)
	j := journal.New("01J0000000000000000000000E1", f.clock)
	f.writeExitedWithAnswer(j, "run_a", "all done")
	beta5 := beta5Request(t)

	// The decoder of such a Controller refuses the field: the double is faithful.
	probe := &wire{decoder: beta5}
	if _, err := probe.ReportRun(t.Context(), "run_a", v1.RunReportRequest{Entries: []v1.RunReportEntry{{Kind: v1.ReportExited, Answer: "x"}}}); err == nil {
		t.Fatal("the decoder without the field accepted an answer")
	}

	w := &wire{decoder: beta5}
	features := &journal.ControllerFeatures{}
	features.Set(nil)
	s := f.sender(w, j)
	s.Features = features
	if _, err := s.Flush(t.Context()); err != nil {
		t.Fatal(err)
	}
	if w.refused != 0 || len(w.answers) != 1 || w.answers[0] != "" || f.pending() != 0 {
		t.Errorf("refused %d, answers %q, %d pending", w.refused, w.answers, f.pending())
	}
}

func TestARolledBackControllerDropsTheAnswerOfAnEntryJournaledWithItAndTheReportIsNotRefused(t *testing.T) {
	f := newFixture(t)
	j := journal.New("01J0000000000000000000000E1", f.clock)
	features := &journal.ControllerFeatures{}
	features.Set([]string{v1.FeatureRunAnswer}) // the Controller took the answer when it was journaled
	f.writeExitedWithAnswer(j, "run_a", strings.Repeat("x", 100))
	features.Set([]string{}) // and then it was rolled back
	w := &wire{decoder: beta5Request(t)}
	s := f.sender(w, j)
	s.Features = features
	if _, err := s.Flush(t.Context()); err != nil {
		t.Fatal(err)
	}
	if w.refused != 0 || len(w.answers) != 1 || w.answers[0] != "" || f.pending() != 0 {
		t.Errorf("refused %d, answers %q, %d pending", w.refused, w.answers, f.pending())
	}
}

func TestAControllerBetaSixDecodesAReportWithoutAnswer(t *testing.T) {
	raw, err := json.Marshal(v1.RunReportRequest{Entries: []v1.RunReportEntry{{Kind: v1.ReportExited, WorkerSeq: 1}}})
	if err != nil || bytes.Contains(raw, []byte("answer")) {
		t.Fatalf("the report is %s (%v)", raw, err)
	}
	var req v1.RunReportRequest
	if err := strictDecode(raw, &req); err != nil {
		t.Errorf("decoding: %v", err)
	}
}

func TestAControllerWithoutIdleAfterResultGetsTheExitWithoutTheEndReasonAndOneWithItGetsIt(t *testing.T) {
	for _, announced := range []bool{false, true} {
		f := newFixture(t)
		j := journal.New("01J0000000000000000000000E1", f.clock)
		k := state.Key{RunID: "run_a", LeaseEpoch: 1}
		err := f.db.WithTx(t.Context(), func(tx *state.Tx) error {
			if _, err := state.InsertAssigned(t.Context(), tx, state.Run{Key: k, LeaseID: "lease_a", Spec: []byte("{}"), CreatedAt: t0, AcceptBy: t0}); err != nil {
				return err
			}
			_, err := j.AppendTx(t.Context(), tx, k, v1.RunReportEntry{Kind: v1.ReportExited, At: t0, EndReason: domain.EndIdleAfterResult})
			return err
		})
		if err != nil {
			t.Fatal(err)
		}
		var got []domain.EndReason
		features := &journal.ControllerFeatures{}
		features.Set([]string{v1.FeatureRunAnswer})
		if announced {
			features.Set([]string{v1.FeatureRunAnswer, v1.FeatureIdleAfterResult})
		}
		s := f.sender(reasonsWire{&got}, j)
		s.Features = features
		if _, err := s.Flush(t.Context()); err != nil {
			t.Fatal(err)
		}
		want := domain.EndReason("")
		if announced {
			want = domain.EndIdleAfterResult
		}
		if len(got) != 1 || got[0] != want || f.pending() != 0 {
			t.Errorf("announced=%v: end reasons sent = %q, %d pending", announced, got, f.pending())
		}
	}
}

type reasonsWire struct{ got *[]domain.EndReason }

func (w reasonsWire) ReportRun(_ context.Context, _ string, req v1.RunReportRequest) (v1.RunReportResponse, error) {
	var resp v1.RunReportResponse
	for _, e := range req.Entries {
		*w.got = append(*w.got, e.EndReason)
		resp.Outcomes = append(resp.Outcomes, v1.RunReportOutcome{WorkerSeq: e.WorkerSeq, Result: v1.ReportApplied})
	}
	return resp, nil
}
