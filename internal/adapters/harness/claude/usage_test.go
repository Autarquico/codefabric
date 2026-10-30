package claude_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/Autarquico/codefabric/internal/adapters/harness"
	"github.com/Autarquico/codefabric/internal/adapters/harness/claude"
)

func eventsFile(t *testing.T, content string) harness.Handle {
	t.Helper()
	p := filepath.Join(t.TempDir(), "events.jsonl")
	if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return harness.Handle{EventsPath: p}
}

func TestCollectUsageWithoutAResultIsEmpty(t *testing.T) {
	got, err := claude.Adapter{}.CollectUsage(t.Context(), nil, eventsFile(t, initLine))
	if err != nil || got == nil || len(got) != 0 {
		t.Errorf("got %#v, %v", got, err)
	}
}

// Without modelUsage the usage of the result is one event; what it leaves out
// is unknown and what it says is 0 is 0.
func TestCollectUsageFromUsageKeepsUnknownApartFromZero(t *testing.T) {
	h := eventsFile(t, `{"type":"result","subtype":"success","usage":{"input_tokens":10,"output_tokens":0}}`+"\n")
	got, err := claude.Adapter{}.CollectUsage(t.Context(), nil, h)
	if err != nil || len(got) != 1 {
		t.Fatalf("got %+v, %v", got, err)
	}
	u := got[0]
	if *u.InputTokens != 10 || *u.OutputTokens != 0 || u.CacheReadTokens != nil || u.CacheWriteTokens != nil || u.Model != "" {
		t.Errorf("usage = %+v", u)
	}
}

func TestCollectUsageNeedsEventsPath(t *testing.T) {
	if _, err := (claude.Adapter{}).CollectUsage(t.Context(), nil, harness.Handle{}); err == nil {
		t.Error("an empty events path is an error")
	}
	if _, err := (claude.Adapter{}).CollectUsage(t.Context(), nil, harness.Handle{EventsPath: "/no/such/file"}); err == nil {
		t.Error("a missing file is an error")
	}
}
