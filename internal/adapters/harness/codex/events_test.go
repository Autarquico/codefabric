package codex_test

import (
	"context"
	"io"
	"os"
	"strings"
	"testing"

	"github.com/Autarquico/codefabric/internal/adapters/harness"
	"github.com/Autarquico/codefabric/internal/adapters/harness/codex"
)

func collect(t *testing.T, in string) []harness.Event {
	t.Helper()
	ch, err := codex.Adapter{}.Events(t.Context(), harness.Handle{}, io.NopCloser(strings.NewReader(in)))
	if err != nil {
		t.Fatal(err)
	}
	var evs []harness.Event
	for ev := range ch {
		evs = append(evs, ev)
	}
	return evs
}

func i64(v int64) *int64 { return &v }

func TestScenario_Usage(t *testing.T) {
	f, err := os.Open("testdata/exec-0.160.1.jsonl")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	ch, err := codex.Adapter{}.Events(t.Context(), harness.Handle{}, f)
	if err != nil {
		t.Fatal(err)
	}
	var evs []harness.Event
	for ev := range ch {
		evs = append(evs, ev)
	}
	if len(evs) != 3 {
		t.Fatalf("events = %+v", evs)
	}
	if evs[0].Kind != harness.EventSessionStarted || evs[0].SessionID != "01a1819d-734d-7a60-88f8-293969dcf0bd" {
		t.Errorf("session = %+v", evs[0])
	}
	u := evs[1].Usage
	if evs[1].Kind != harness.EventUsage || u == nil || *u.InputTokens != 10326 || *u.CacheReadTokens != 7168 ||
		*u.CacheWriteTokens != 0 || *u.OutputTokens != 5 || u.RequestCount != nil || u.Model != "" || evs[1].Model != "" {
		t.Errorf("usage = %+v", evs[1])
	}
	if r := evs[2].Result; evs[2].Kind != harness.EventResult || r == nil || r.Failed || r.Summary != "ok" {
		t.Errorf("result = %+v", evs[2])
	}

	got, err := codex.Adapter{}.CollectUsage(t.Context(), nil, harness.Handle{EventsPath: "testdata/exec-0.160.1.jsonl"})
	if err != nil || len(got) != 1 || *got[0].InputTokens != 10326 || *got[0].CacheReadTokens != 7168 || *got[0].OutputTokens != 5 {
		t.Errorf("CollectUsage = %+v, %v", got, err)
	}
}

func TestEventsSkipsWhatItDoesNotUnderstand(t *testing.T) {
	in := "Reading additional input from stdin...\n" +
		`{"type":"thread.started","thread_id":"t1"}` + "\n" +
		`{"type":"item.completed","item":{"id":"i","type":"error","message":"warn"}}` + "\n" +
		`{"type":"future.thing","x":1}` + "\n" +
		`{"type":"item.completed","item":{"id":"i","type":"agent_message","text":"first"}}` + "\n" +
		`{"type":"item.completed","item":{"id":"i","type":"agent_message","text":"last"}}` + "\n" +
		`{"type":"turn.completed"}` + "\n" +
		`{"type":"turn.compl`
	evs := collect(t, in)
	if len(evs) != 2 || evs[0].Kind != harness.EventSessionStarted || evs[1].Kind != harness.EventResult ||
		evs[1].Result.Summary != "last" {
		t.Errorf("events = %+v", evs)
	}
}

func TestEventsLastLineWithoutNewline(t *testing.T) {
	evs := collect(t, `{"type":"thread.started","thread_id":"t1"}`)
	if len(evs) != 1 || evs[0].SessionID != "t1" {
		t.Errorf("events = %+v", evs)
	}
}

func TestEventsFailures(t *testing.T) {
	evs := collect(t, `{"type":"error","message":"boom"}`+"\n"+`{"type":"turn.failed","error":{"message":"bad"}}`+"\n")
	if len(evs) != 3 || evs[0].Kind != harness.EventError || evs[0].Message != "boom" ||
		evs[1].Kind != harness.EventError || evs[1].Message != "bad" ||
		evs[2].Kind != harness.EventResult || !evs[2].Result.Failed || evs[2].Result.Summary != "bad" {
		t.Errorf("events = %+v", evs)
	}
}

func TestEventsContextCancelled(t *testing.T) {
	pr, pw := io.Pipe()
	defer pw.Close()
	ctx, stop := context.WithCancel(t.Context())
	ch, err := codex.Adapter{}.Events(ctx, harness.Handle{}, pr)
	if err != nil {
		t.Fatal(err)
	}
	go func() { _, _ = pw.Write([]byte(`{"type":"thread.started","thread_id":"t1"}` + "\n")) }()
	<-ch // first event received; nobody reads the next ones
	go func() { _, _ = pw.Write([]byte(`{"type":"thread.started","thread_id":"t2"}` + "\n")) }()
	stop()
	_ = pr.Close() // the caller unblocks the read
	for range ch {
	}
}
