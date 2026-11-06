package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	v1 "github.com/Autarquico/codefabric/internal/api/v1"
	"github.com/Autarquico/codefabric/internal/client"
)

// shipper plays a Worker's structured logs by hand, through the SDK with the
// Worker's own credentials: what the Controller stores is exactly what a test says.
type shipper struct {
	c   *client.Client
	seq uint64
}

func (s *shipper) ship(t *testing.T, run, level, msg string) {
	t.Helper()
	s.seq++
	entry := v1.LogEntry{WorkerEpoch: "E1", WorkerSeq: s.seq, Timestamp: time.Now(), Level: level, Message: msg, RunID: run}
	got, err := s.c.ShipLogs(t.Context(), v1.LogBatch{Entries: []v1.LogEntry{entry}})
	if err != nil || got.Applied != 1 {
		t.Fatalf("ship %q: %+v, %v", msg, got, err)
	}
}

// logMessages is the messages of a `cf logs --json` page, in the order shown.
func logMessages(t *testing.T, args ...string) []string {
	t.Helper()
	page := ok[v1.LogPage](t, append([]string{"logs"}, args...)...)
	var msgs []string
	for _, l := range page.Logs {
		msgs = append(msgs, l.Message)
	}
	return msgs
}

func sameMessages(got []string, want ...string) bool {
	return strings.Join(got, "|") == strings.Join(want, "|")
}

// twoWorkers has "alpha" (with a Run) and "beta" as logging Workers.
func twoWorkers(t *testing.T) (alpha, beta *shipper, run v1.Run) {
	t.Helper()
	host, _ := bringUpController(t)
	r := newWorkerRun(t, host)
	invite := workerInvite(t, host)
	box := machine(t)
	ok[map[string]any](t, "worker", "join", invite, "--name", "beta")
	c, err := client.Open(box.WorkerCredentialsDir())
	if err != nil {
		t.Fatal(err)
	}
	use(t, host)
	return &shipper{c: r.worker}, &shipper{c: c}, r.run
}

func TestScenario_LogsOutputStorage_Filtros(t *testing.T) {
	alpha, beta, _ := twoWorkers(t)
	alpha.ship(t, "", "info", "a-info")
	alpha.ship(t, "", "error", "a-error")
	beta.ship(t, "", "debug", "b-debug")
	beta.ship(t, "", "warn", "b-warn")

	// Past this second, so that --since (to the second) tells the lines apart.
	mark := time.Now().Truncate(time.Second).Add(time.Second)
	time.Sleep(time.Until(mark.Add(100 * time.Millisecond)))
	alpha.ship(t, "", "warn", "a-late")
	beta.ship(t, "", "error", "b-late")

	if got := logMessages(t, "--worker", "alpha"); !sameMessages(got, "a-info", "a-error", "a-late") {
		t.Errorf("--worker alpha = %v", got)
	}
	if got := logMessages(t, "--worker", "beta", "--level", "warn"); !sameMessages(got, "b-warn", "b-late") {
		t.Errorf("--worker beta --level warn = %v", got)
	}
	if got := logMessages(t, "--worker", "alpha", "--since", mark.UTC().Format(time.RFC3339)); !sameMessages(got, "a-late") {
		t.Errorf("--worker alpha --since mark = %v", got)
	}
	if got := logMessages(t, "--worker", "beta", "--level", "warn", "--since", mark.UTC().Format(time.RFC3339)); !sameMessages(got, "b-late") {
		t.Errorf("--worker beta --level warn --since mark = %v", got)
	}
	if got := logMessages(t, "--worker", "alpha", "--since", "1h", "--limit", "2"); !sameMessages(got, "a-error", "a-late") {
		t.Errorf("--limit 2 is the last two: %v", got)
	}

	// Paging from a cursor goes forward in the order of the sequence.
	first := ok[v1.LogPage](t, "logs", "--worker", "alpha", "--limit", "1")
	if len(first.Logs) != 1 || first.Logs[0].Message != "a-late" {
		t.Fatalf("--limit 1 = %+v", first)
	}
	all := ok[v1.LogPage](t, "logs", "--worker", "alpha")
	after := fmt.Sprint(all.Logs[0].Sequence)
	if got := logMessages(t, "--worker", "alpha", "--after", after, "--limit", "1"); !sameMessages(got, "a-error") {
		t.Errorf("--after the first, 1 = %v", got)
	}
	for i := 1; i < len(all.Logs); i++ {
		if all.Logs[i].Sequence <= all.Logs[i-1].Sequence {
			t.Errorf("not in the order of the sequence: %d after %d", all.Logs[i].Sequence, all.Logs[i-1].Sequence)
		}
	}
}

func TestLogsOutputStorage_RunOutputAndRunLogsAreTwoThings(t *testing.T) {
	alpha, _, run := twoWorkers(t)
	alpha.ship(t, run.ID, "info", "structured-line")
	alpha.ship(t, "", "info", "elsewhere")

	// --run alone: the output of the Run, none of its structured logs.
	if code, out, errOut := cf(t, "", "logs", "--run", run.Short); code != 0 || strings.Contains(out, "structured-line") {
		t.Errorf("--run alone: exit %d %q %s", code, out, errOut)
	}
	for _, args := range [][]string{{"--run", run.Short, "--structured"}, {"--run", run.Short, "--level", "info"}} {
		if got := logMessages(t, args...); !sameMessages(got, "structured-line") {
			t.Errorf("%v = %v", args, got)
		}
	}
	if got := logMessages(t, "--worker", "alpha"); !sameMessages(got, "structured-line", "elsewhere") {
		t.Errorf("without --run: %v", got)
	}
}

func TestLogsOutputStorage_HumanOutputIsSanitisedAndRawIsNot(t *testing.T) {
	alpha, _, _ := twoWorkers(t)
	alpha.ship(t, "", "info", "\x1b]0;pwned\x07\x1b[31mred\x1b[0m\rtwo\nforged 1 line")

	code, out, errOut := cf(t, "", "logs", "--worker", "alpha")
	if code != 0 || strings.ContainsAny(out, "\x1b\x07\r\x00") || strings.Count(out, "\n") != 1 || !strings.Contains(out, "redtwo forged 1 line") {
		t.Errorf("sanitised: exit %d %q %s", code, out, errOut)
	}
	if code, out, _ = cf(t, "", "logs", "--worker", "alpha", "--raw"); code != 0 || !strings.Contains(out, "\x1b[31mred") {
		t.Errorf("--raw: exit %d %q", code, out)
	}
}

func TestScenario_LogsOutputStorage_Follow(t *testing.T) {
	alpha, _, _ := twoWorkers(t)
	alpha.ship(t, "", "info", "before")

	live := cfLive(t, "logs", "--follow", "--worker", "alpha", "--tail", "1")
	live.waitFor(t, "the tail", func(out string) bool { return strings.Contains(out, "before") })
	start := time.Now()
	alpha.ship(t, "", "info", "arrives-live")
	live.waitFor(t, "the line", func(out string) bool { return strings.Contains(out, "arrives-live") })
	if took := time.Since(start); took > 2*time.Second {
		t.Errorf("the line took %s to reach the follow, want under 2s", took)
	}
	if strings.Count(live.out.String(), "before") != 1 {
		t.Errorf("the tail repeated: %q", live.out.String())
	}
	if code := live.stop(t); code != 0 {
		t.Errorf("exit %d: %s", code, live.err.String())
	}

	// --json --follow is JSON Lines, one log per line.
	jl := cfLive(t, "logs", "--follow", "--json", "--worker", "alpha", "--tail", "1")
	jl.waitFor(t, "the tail", func(out string) bool { return strings.Contains(out, "arrives-live") })
	alpha.ship(t, "", "info", "second")
	jl.waitFor(t, "the second", func(out string) bool { return strings.Contains(out, "second") })
	jl.stop(t)
	sc := bufio.NewScanner(strings.NewReader(jl.out.String()))
	var msgs []string
	for sc.Scan() {
		var l v1.Log
		if err := json.Unmarshal(sc.Bytes(), &l); err != nil {
			t.Fatalf("line %q: %v", sc.Text(), err)
		}
		msgs = append(msgs, l.Message)
	}
	if !sameMessages(msgs, "arrives-live", "second") {
		t.Errorf("JSON Lines = %v", msgs)
	}
}

// The tail of a Run's output reads at most 4 MiB, however long its last line.
func TestLogsOutputStorage_TailOfARunWithAHugeLineIsBounded(t *testing.T) {
	host, _ := bringUpController(t)
	r := newWorkerRun(t, host)
	use(t, host)
	const piece = 64 << 10
	offset := uint64(0)
	for range 80 { // 5 MiB with no newline
		r.upload(t, offset, strings.Repeat("x", piece))
		offset += piece
	}
	r.upload(t, offset, "end\n")

	code, out, errOut := cf(t, "", "logs", "--run", r.run.Short, "--tail", "1")
	if code != 0 || len(out) != 4<<20 || !strings.HasSuffix(out, "xxend\n") || !strings.Contains(errOut, "only the last 4 MiB") {
		t.Fatalf("exit %d, %d bytes, stderr %q", code, len(out), errOut)
	}
}

// A clipped tail keeps its bytes exactly in --raw and --json, even when they are not UTF-8.
func TestLogsOutputStorage_ClippedTailKeepsNonUTF8BytesInRawAndJSON(t *testing.T) {
	host, _ := bringUpController(t)
	r := newWorkerRun(t, host)
	use(t, host)
	const piece = 64 << 10
	offset := uint64(0)
	for range 80 { // 5 MiB of continuation bytes with no newline
		r.upload(t, offset, strings.Repeat("\x80", piece))
		offset += piece
	}
	r.upload(t, offset, "end\n")

	code, out, _ := cf(t, "", "logs", "--run", r.run.Short, "--tail", "1", "--raw")
	if code != 0 || len(out) != 4<<20 || !strings.HasPrefix(out, "\x80\x80") {
		t.Fatalf("raw: exit %d, %d bytes", code, len(out))
	}
	page := ok[v1.RunOutput](t, "logs", "--run", r.run.Short, "--tail", "1")
	if len(page.Data) != 4<<20 || page.Data[0] != 0x80 {
		t.Fatalf("json: %d bytes", len(page.Data))
	}
}
