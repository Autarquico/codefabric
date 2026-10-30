package runs

import (
	"strings"
	"testing"
	"time"

	v1 "github.com/Autarquico/codefabric/internal/api/v1"
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
