package runs

import (
	"strings"
	"testing"
	"time"

	v1 "github.com/Autarquico/codeflock/internal/api/v1"
	"github.com/Autarquico/codeflock/internal/domain"
)

func stoppedWith(kind v1.RunReportKind, fs ...v1.StopFailure) v1.RunReportEntry {
	return v1.RunReportEntry{Kind: kind, LeaseEpoch: 1, At: time.Now(), StopFailures: fs}
}

func TestValidateEntry_StopFailures(t *testing.T) {
	ok := v1.StopFailure{Service: "db", Error: "stop exited with 1"}
	many := make([]v1.StopFailure, v1.MaxStopFailures+1)
	for i := range many {
		many[i] = ok
	}
	cases := []struct {
		name  string
		entry v1.RunReportEntry
		valid bool
	}{
		{"a stopped with a failure is accepted", stoppedWith(v1.ReportStopped, ok), true},
		{"a stopped with the most failures is accepted", stoppedWith(v1.ReportStopped, many[:v1.MaxStopFailures]...), true},
		{"on a kind other than stopped", stoppedWith(v1.ReportStarted, ok), false},
		{"more than the most failures", stoppedWith(v1.ReportStopped, many...), false},
		{"an empty service", stoppedWith(v1.ReportStopped, v1.StopFailure{Error: "x"}), false},
		{"a service over the limit", stoppedWith(v1.ReportStopped, v1.StopFailure{Service: strings.Repeat("s", v1.MaxReportMessageLen+1)}), false},
		{"an error over the limit", stoppedWith(v1.ReportStopped, v1.StopFailure{Service: "db", Error: strings.Repeat("e", v1.MaxReportMessageLen+1)}), false},
		{"an error that does not print", stoppedWith(v1.ReportStopped, v1.StopFailure{Service: "db", Error: "a\x1b[31m"}), false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := validateEntry(c.entry, time.Now())
			if c.valid != (err == nil) {
				t.Fatalf("validateEntry = %v, want valid=%v", err, c.valid)
			}
		})
	}
}

func TestValidateEntry_EndReasonModelAndAnswer(t *testing.T) {
	exited := func(mutate func(*v1.RunReportEntry)) v1.RunReportEntry {
		e := v1.RunReportEntry{Kind: v1.ReportExited, LeaseEpoch: 1, At: time.Now(), Cause: v1.ExitCauseStop}
		mutate(&e)
		return e
	}
	cases := []struct {
		name  string
		entry v1.RunReportEntry
		valid bool
	}{
		{"input_required with a model", exited(func(e *v1.RunReportEntry) { e.EndReason, e.Model = domain.EndInputRequired, "m" }), true},
		{"idle_after_result", exited(func(e *v1.RunReportEntry) { e.EndReason = domain.EndIdleAfterResult }), true},
		{"idle_after_result of a lost wrapper", exited(func(e *v1.RunReportEntry) { e.EndReason, e.Cause = domain.EndIdleAfterResult, v1.ExitCauseWrapperLost }), false},
		{"another end reason", exited(func(e *v1.RunReportEntry) { e.EndReason = domain.EndBudget }), false},
		{"input_required of a lost wrapper", exited(func(e *v1.RunReportEntry) { e.EndReason, e.Cause = domain.EndInputRequired, v1.ExitCauseWrapperLost }), false},
		{"a model too long", exited(func(e *v1.RunReportEntry) { e.Model = strings.Repeat("m", v1.MaxModelLen+1) }), false},
		{"a model on a started", v1.RunReportEntry{Kind: v1.ReportStarted, LeaseEpoch: 1, At: time.Now(), Model: "m", Handle: &v1.RuntimeHandle{Runtime: "tmux", Ref: "r"}}, true},
		{"a model too long on a started", v1.RunReportEntry{Kind: v1.ReportStarted, LeaseEpoch: 1, At: time.Now(), Model: strings.Repeat("m", v1.MaxModelLen+1), Handle: &v1.RuntimeHandle{Runtime: "tmux", Ref: "r"}}, false},
		{"a model on a stopped", v1.RunReportEntry{Kind: v1.ReportStopped, LeaseEpoch: 1, At: time.Now(), Model: "m"}, false},
		{"an answer on an exited", exited(func(e *v1.RunReportEntry) { e.Answer = "done:\n\tall green" }), true},
		{"an answer at the limit", exited(func(e *v1.RunReportEntry) { e.Answer = strings.Repeat("a", v1.MaxAnswerLen) }), true},
		{"an answer too long", exited(func(e *v1.RunReportEntry) { e.Answer = strings.Repeat("a", v1.MaxAnswerLen+1) }), false},
		{"an answer that is not UTF-8", exited(func(e *v1.RunReportEntry) { e.Answer = "a\xffb" }), false},
		{"an answer that does not print", exited(func(e *v1.RunReportEntry) { e.Answer = "a\x1b[31mb" }), false},
		{"an answer on a started", v1.RunReportEntry{Kind: v1.ReportStarted, LeaseEpoch: 1, At: time.Now(), Answer: "a", Handle: &v1.RuntimeHandle{Runtime: "tmux", Ref: "r"}}, false},
		{"an answer on a stopped", v1.RunReportEntry{Kind: v1.ReportStopped, LeaseEpoch: 1, At: time.Now(), Answer: "a"}, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if err := validateEntry(c.entry, time.Now()); c.valid != (err == nil) {
				t.Fatalf("validateEntry = %v, want valid=%v", err, c.valid)
			}
		})
	}
}

func TestValidateEntry_ConfigDigest(t *testing.T) {
	started := func(digest string) v1.RunReportEntry {
		return v1.RunReportEntry{
			Kind: v1.ReportStarted, LeaseEpoch: 1, At: time.Now(), ConfigDigest: digest,
			Handle: &v1.RuntimeHandle{Runtime: "tmux", Ref: "cf-x"},
		}
	}
	good := "sha256:" + strings.Repeat("a", 64)
	exited := v1.RunReportEntry{Kind: v1.ReportExited, Cause: v1.ExitCauseExit, LeaseEpoch: 1, At: time.Now(), ConfigDigest: good}
	cases := []struct {
		name  string
		entry v1.RunReportEntry
		valid bool
	}{
		{"a started without digest", started(""), true},
		{"a started with a digest", started(good), true},
		{"a digest that is not sha256:<hex>", started("sha256:xyz"), false},
		{"a digest in upper case", started("sha256:" + strings.Repeat("A", 64)), false},
		{"on a kind other than started", exited, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if err := validateEntry(c.entry, time.Now()); c.valid != (err == nil) {
				t.Fatalf("validateEntry = %v, want valid=%v", err, c.valid)
			}
		})
	}
}

func TestValidateReport_AnExitedWithTheLargestAnswerTheWorkerSendsFits(t *testing.T) {
	for _, unit := range []string{"<", " ", "&", "\"", "\n"} {
		e := v1.RunReportEntry{
			Kind: v1.ReportExited, LeaseEpoch: 1, At: time.Now(), Cause: v1.ExitCauseExit,
			WorkerEpoch: "01J00000000000000000000000", WorkerSeq: 1,
			Model:   strings.Repeat("<", v1.MaxModelLen),
			Message: strings.Repeat("<", v1.MaxReportMessageLen),
			Answer:  v1.BoundAnswer(strings.Repeat(unit, 20000)),
		}
		if err := validateReport(v1.RunReportRequest{Entries: []v1.RunReportEntry{e}}, time.Now()); err != nil {
			t.Errorf("%q: %v", unit, err)
		}
	}
}
