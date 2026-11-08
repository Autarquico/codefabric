package cli

import (
	"bytes"
	"strings"
	"testing"
	"time"

	v1 "github.com/Autarquico/codefabric/internal/api/v1"
)

func TestPrintStatus_TheWorkersSectionAppearsOnlyWithABacklog(t *testing.T) {
	st := v1.Status{ControllerID: "c1", Version: "v1", Database: v1.DatabaseStatus{OK: true}}

	var plain bytes.Buffer
	if err := printStatus(&plain, "host:7000", st); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(plain.String(), "Workers") {
		t.Errorf("no Worker has a backlog, yet:\n%s", plain.String())
	}

	at := time.Date(2026, 11, 8, 10, 0, 0, 0, time.UTC)
	st.Workers = []v1.StatusWorker{
		{ID: "worker_1", Name: "laptop", JournalBacklogBytes: 1536},
		{ID: "worker_2", Name: "box", JournalBacklogBytes: 3 << 30, JournalTruncatedAt: &at},
	}
	var out bytes.Buffer
	if err := printStatus(&out, "host:7000", st); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"Workers     journal backlog", "laptop  1.5 KiB", "box  3.0 GiB  (logs or output discarded 2026-11-08T10:00:00Z)"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("output lacks %q:\n%s", want, out.String())
		}
	}
	if !strings.HasPrefix(out.String(), plain.String()) {
		t.Errorf("the controller lines changed:\n%s", out.String())
	}
}

func TestHumanBytes(t *testing.T) {
	for n, want := range map[uint64]string{0: "0 B", 1023: "1023 B", 1024: "1.0 KiB", 5 << 20: "5.0 MiB", 1 << 40: "1.0 TiB"} {
		if got := humanBytes(n); got != want {
			t.Errorf("humanBytes(%d) = %q, want %q", n, got, want)
		}
	}
}

func TestPrintWorker_ShowsTheBacklogAndTheLastDiscardEvenWithNoBacklog(t *testing.T) {
	at := time.Date(2026, 11, 8, 10, 0, 0, 0, time.UTC)
	var out bytes.Buffer
	if err := printWorker(&out, v1.Worker{Name: "box", JournalBacklogBytes: 0, JournalTruncatedAt: &at}); err != nil {
		t.Fatal(err)
	}
	if want := "Journal       backlog 0 B, logs or output last discarded"; !strings.Contains(out.String(), want) {
		t.Errorf("output lacks %q:\n%s", want, out.String())
	}
	out.Reset()
	if err := printWorker(&out, v1.Worker{Name: "box", JournalBacklogBytes: 1536}); err != nil {
		t.Fatal(err)
	}
	if want := "Journal       backlog 1.5 KiB\n"; !strings.Contains(out.String(), want) {
		t.Errorf("output lacks %q:\n%s", want, out.String())
	}
}
