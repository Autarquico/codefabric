package claude_test

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Autarquico/codefabric/internal/adapters/harness"
	"github.com/Autarquico/codefabric/internal/adapters/harness/claude"
)

func collect(t *testing.T, in string) []harness.Event {
	t.Helper()
	ch, err := claude.Adapter{}.Events(t.Context(), harness.Handle{}, io.NopCloser(strings.NewReader(in)))
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

func fixture(t *testing.T, name string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", name)) //nolint:gosec // testdata
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

const (
	initLine = `{"type":"system","subtype":"init","session_id":"s1","model":"claude-opus-5-5"}` + "\n"
	endLine  = `{"type":"result","subtype":"success","is_error":false,"result":"done","usage":{"input_tokens":1,"output_tokens":2}}` + "\n"
)

// The recording of a Run (hand-built from the stream-json schema: a real one
// needs a login): the model of the session, the usage per model and the exit.
func TestScenario_ClaudeCodeAdapter_Stream(t *testing.T) {
	evs := collect(t, fixture(t, "success-handbuilt.jsonl"))
	want := []harness.Event{
		{Kind: harness.EventSessionStarted, SessionID: "5b0e1c1e-2f64-4d0e-9a33-6f1a7d0c9a11", Model: "claude-opus-5-5"},
		{Kind: harness.EventUsage, Usage: &harness.UsageEvent{Model: "claude-haiku-5", InputTokens: i64(0), OutputTokens: i64(2), CacheReadTokens: i64(300)}},
		{Kind: harness.EventUsage, Usage: &harness.UsageEvent{Model: "claude-opus-5-5", InputTokens: i64(5), OutputTokens: i64(40), CacheReadTokens: i64(7168), CacheWriteTokens: i64(0)}},
		{Kind: harness.EventResult, Result: &harness.Result{Summary: "Created hello.txt"}},
	}
	if !equalEvents(evs, want) {
		t.Errorf("events = %+v", evs)
	}
	got, err := claude.Adapter{}.CollectUsage(t.Context(), nil, harness.Handle{EventsPath: "testdata/success-handbuilt.jsonl"})
	if err != nil || len(got) != 2 || got[0].Model != "claude-haiku-5" || got[0].CacheWriteTokens != nil || *got[1].CacheWriteTokens != 0 {
		t.Errorf("CollectUsage = %+v, %v", got, err)
	}
}

// A real recording of 2.1.314 without a login: is_error with subtype "success"
// is a failed Run, and the zeros it reports are known zeros.
func TestScenario_ClaudeCodeAdapter_StreamFailed(t *testing.T) {
	evs := collect(t, fixture(t, "notlogged-2.1.314.jsonl"))
	if len(evs) != 4 {
		t.Fatalf("events = %+v", evs)
	}
	if evs[0].Kind != harness.EventSessionStarted || evs[0].Model != "claude-opus-5-5" || evs[0].SessionID == "" {
		t.Errorf("session = %+v", evs[0])
	}
	if u := evs[1].Usage; evs[1].Kind != harness.EventUsage || u == nil || *u.InputTokens != 0 || *u.OutputTokens != 0 || u.Model != "" {
		t.Errorf("usage = %+v", evs[1])
	}
	msg := "Not logged in · Please run /login"
	if evs[2].Kind != harness.EventError || evs[2].Message != msg {
		t.Errorf("error = %+v", evs[2])
	}
	if r := evs[3].Result; evs[3].Kind != harness.EventResult || r == nil || !r.Failed || r.Summary != msg {
		t.Errorf("result = %+v", evs[3])
	}
}

func TestInputRequiredByToolUse(t *testing.T) {
	in := initLine +
		`{"type":"assistant","message":{"content":[{"type":"text","text":"hm"},{"type":"tool_use","id":"t","name":"AskUserQuestion","input":{"questions":[{"question":"Which db?"}]}}]}}` + "\n" + endLine
	evs := collect(t, in)
	if len(evs) != 4 || evs[1].Kind != harness.EventInputRequired || evs[1].Message != "Which db?" {
		t.Errorf("events = %+v", evs)
	}
}

func TestInputRequiredByPermissionDenied(t *testing.T) {
	in := initLine + `{"type":"system","subtype":"permission_denied","tool_name":"AskUserQuestion","tool_use_id":"t"}` + "\n" + endLine
	evs := collect(t, in)
	if len(evs) != 4 || evs[1].Kind != harness.EventInputRequired || evs[1].Message == "" {
		t.Errorf("events = %+v", evs)
	}
}

// Where a chunk of lines ends does not change what comes out: every split of
// the recording gives the events of the whole.
func TestEventsAreStatelessAcrossChunks(t *testing.T) {
	lines := strings.SplitAfter(fixture(t, "success-handbuilt.jsonl"), "\n")
	whole := collect(t, strings.Join(lines, ""))
	for i := range lines {
		split := append(collect(t, strings.Join(lines[:i], "")), collect(t, strings.Join(lines[i:], ""))...)
		if !equalEvents(split, whole) {
			t.Errorf("split at line %d: %+v, want %+v", i, split, whole)
		}
	}
}

func TestOtherDenialsAndToolsDoNotCount(t *testing.T) {
	in := initLine +
		`{"type":"system","subtype":"permission_denied","tool_name":"Bash"}` + "\n" +
		`{"type":"assistant","message":{"content":[{"type":"tool_use","id":"t","name":"Bash","input":{"command":"ls"}}]}}` + "\n" +
		`{"type":"assistant","message":{"content":"AskUserQuestion in plain text"}}` + "\n" +
		`{"type":"result","subtype":"success","is_error":false,"result":"r","permission_denials":[{"tool_name":"AskUserQuestion"}]}` + "\n"
	for _, ev := range collect(t, in) {
		if ev.Kind == harness.EventInputRequired {
			t.Errorf("unexpected %+v", ev)
		}
	}
}

func TestEventsSkipsWhatItDoesNotUnderstand(t *testing.T) {
	in := "warming up\n" + initLine + `{"type":"future","x":1}` + "\n" + `{"type":"system","subtype":"other"}` + "\n" +
		`{"type":"system","subtype":"init"}` + "\n" + endLine + `{"type":"result","sub`
	evs := collect(t, in)
	if len(evs) != 3 || evs[0].Kind != harness.EventSessionStarted || evs[1].Kind != harness.EventUsage || evs[2].Kind != harness.EventResult {
		t.Errorf("events = %+v", evs)
	}
}

func TestEventsLastLineWithoutNewline(t *testing.T) {
	evs := collect(t, strings.TrimSuffix(initLine, "\n"))
	if len(evs) != 1 || evs[0].SessionID != "s1" {
		t.Errorf("events = %+v", evs)
	}
}

func TestErrorSubtypesFail(t *testing.T) {
	evs := collect(t, `{"type":"result","subtype":"error_max_turns","is_error":false}`+"\n")
	if len(evs) != 2 || evs[0].Kind != harness.EventError || evs[0].Message != "error_max_turns" || !evs[1].Result.Failed {
		t.Errorf("events = %+v", evs)
	}
}

func TestEventsNeedsASource(t *testing.T) {
	if _, err := (claude.Adapter{}).Events(t.Context(), harness.Handle{}, nil); err == nil {
		t.Error("nil source: no error")
	}
}

func TestEventsClosesWhenItsContextEndsWithAReadBlocked(t *testing.T) {
	pr, pw := io.Pipe()
	defer func() { _ = pw.Close() }()
	ctx, cancel := context.WithCancel(t.Context())
	ch, err := claude.Adapter{}.Events(ctx, harness.Handle{}, pr)
	if err != nil {
		t.Fatal(err)
	}
	go func() { _, _ = pw.Write([]byte(initLine)) }()
	if ev := <-ch; ev.Kind != harness.EventSessionStarted {
		t.Fatalf("event = %+v", ev)
	}
	cancel()
	select {
	case _, ok := <-ch:
		if ok {
			t.Fatal("an event after ctx ended")
		}
	case <-time.After(time.Second):
		t.Fatal("the channel is still open a second after ctx ended")
	}
	_ = pr.Close()
}

func equalEvents(a, b []harness.Event) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		x, y := a[i], b[i]
		if x.Kind != y.Kind || x.SessionID != y.SessionID || x.Model != y.Model || x.Message != y.Message {
			return false
		}
		if (x.Result == nil) != (y.Result == nil) || x.Result != nil && *x.Result != *y.Result {
			return false
		}
		if (x.Usage == nil) != (y.Usage == nil) || x.Usage != nil && !equalUsage(*x.Usage, *y.Usage) {
			return false
		}
	}
	return true
}

func equalUsage(a, b harness.UsageEvent) bool {
	same := func(p, q *int64) bool { return p == nil && q == nil || p != nil && q != nil && *p == *q }
	return a.Model == b.Model && same(a.InputTokens, b.InputTokens) && same(a.OutputTokens, b.OutputTokens) &&
		same(a.CacheReadTokens, b.CacheReadTokens) && same(a.CacheWriteTokens, b.CacheWriteTokens)
}
