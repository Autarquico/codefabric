package codex_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/Autarquico/codefabric/internal/adapters/harness"
	"github.com/Autarquico/codefabric/internal/adapters/harness/codex"
)

func eventsFile(t *testing.T, content string) harness.Handle {
	t.Helper()
	p := filepath.Join(t.TempDir(), "events.jsonl")
	if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return harness.Handle{EventsPath: p}
}

func TestCollectUsageWithoutUsage(t *testing.T) {
	h := eventsFile(t, `{"type":"thread.started","thread_id":"t"}`+"\n"+`{"type":"turn.completed"}`+"\n")
	got, err := codex.Adapter{}.CollectUsage(t.Context(), nil, h)
	if err != nil || got == nil || len(got) != 0 {
		t.Errorf("got %#v, %v", got, err)
	}
}

func TestCollectUsageAbsentFieldIsNil(t *testing.T) {
	h := eventsFile(t, `{"type":"turn.completed","usage":{"input_tokens":10,"output_tokens":0}}`+"\n")
	got, err := codex.Adapter{}.CollectUsage(t.Context(), nil, h)
	if err != nil || len(got) != 1 {
		t.Fatalf("got %+v, %v", got, err)
	}
	u := got[0]
	if *u.InputTokens != 10 || *u.OutputTokens != 0 || u.CacheReadTokens != nil || u.CacheWriteTokens != nil || u.RequestCount != nil {
		t.Errorf("usage = %+v", u)
	}
}

func TestCollectUsageNeedsEventsPath(t *testing.T) {
	if _, err := (codex.Adapter{}).CollectUsage(t.Context(), nil, harness.Handle{}); err == nil {
		t.Error("an empty events path is an error")
	}
	if _, err := (codex.Adapter{}).CollectUsage(t.Context(), nil, harness.Handle{EventsPath: "/no/such/file"}); err == nil {
		t.Error("a missing file is an error")
	}
}
