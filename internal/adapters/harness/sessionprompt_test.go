package harness_test

import (
	"testing"

	"github.com/Autarquico/codefabric/internal/adapters/harness"
)

func TestTranscriptHolds_OnlyAMessageOfTheUserThatIsTheTextCounts(t *testing.T) {
	const prompt = "run tests"
	tests := []struct {
		name       string
		transcript string
		want       bool
	}{
		{"claude, a string", `{"type":"user","message":{"role":"user","content":"run tests"}}`, true},
		{"claude, text blocks", `{"type":"user","message":{"role":"user","content":[{"type":"text","text":" run tests\n"}]}}`, true},
		{"codex, input_text", `{"type":"response_item","payload":{"type":"message","role":"user","content":[{"type":"input_text","text":"run tests"}]}}`, true},
		{"codex, user_message event", `{"type":"event_msg","payload":{"type":"user_message","message":"run tests"}}`, true},
		{"the assistant said it", `{"type":"assistant","message":{"role":"assistant","content":[{"type":"text","text":"run tests"}]}}`, false},
		{"the assistant said it, codex", `{"type":"response_item","payload":{"type":"message","role":"assistant","content":[{"type":"output_text","text":"run tests"}]}}`, false},
		{"a longer message of the user", `{"type":"user","message":{"role":"user","content":"please run tests now"}}`, false},
		{"a tool result", `{"type":"user","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"t","content":"run tests"}]}}`, false},
		{"a tool call that mentions it", `{"type":"response_item","payload":{"type":"function_call","arguments":"{\"command\":\"run tests\"}"}}`, false},
		{"a cut last line", "{\"type\":\"user\",\"message\":{\"role\":\"user\",\"content\":\"run tests\"}}\n{\"type\":\"user\",\"mess", true},
		{"an empty transcript", ``, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := harness.TranscriptHolds([]byte(tc.transcript), prompt)
			if err != nil || got != tc.want {
				t.Errorf("TranscriptHolds = %v, %v; want %v", got, err, tc.want)
			}
		})
	}
}
