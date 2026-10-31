package runs

import (
	"strings"
	"testing"
	"time"

	v1 "github.com/Autarquico/codefabric/internal/api/v1"
	"github.com/Autarquico/codefabric/internal/domain"
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

func TestValidateEntry_EndReasonAndModel(t *testing.T) {
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
		{"another end reason", exited(func(e *v1.RunReportEntry) { e.EndReason = domain.EndBudget }), false},
		{"input_required of a lost wrapper", exited(func(e *v1.RunReportEntry) { e.EndReason, e.Cause = domain.EndInputRequired, v1.ExitCauseWrapperLost }), false},
		{"a model too long", exited(func(e *v1.RunReportEntry) { e.Model = strings.Repeat("m", v1.MaxModelLen+1) }), false},
		{"a model on a started", v1.RunReportEntry{Kind: v1.ReportStarted, LeaseEpoch: 1, At: time.Now(), Model: "m", Handle: &v1.RuntimeHandle{Runtime: "tmux", Ref: "r"}}, true},
		{"a model too long on a started", v1.RunReportEntry{Kind: v1.ReportStarted, LeaseEpoch: 1, At: time.Now(), Model: strings.Repeat("m", v1.MaxModelLen+1), Handle: &v1.RuntimeHandle{Runtime: "tmux", Ref: "r"}}, false},
		{"a model on a stopped", v1.RunReportEntry{Kind: v1.ReportStopped, LeaseEpoch: 1, At: time.Now(), Model: "m"}, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if err := validateEntry(c.entry, time.Now()); c.valid != (err == nil) {
				t.Fatalf("validateEntry = %v, want valid=%v", err, c.valid)
			}
		})
	}
}
