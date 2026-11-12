package harness

import (
	"bytes"
	"encoding/json"
	"strings"
)

// ContinuePrompt is what a headless process that resumes a session already
// holding its prompt is asked: the original prompt is not repeated, but the
// CLI needs one to run.
const ContinuePrompt = "Continue where you left off."

// TranscriptHolds tells whether a transcript of JSON lines has a message of the
// user that is exactly text (surrounding space aside): what the assistant said,
// what a tool printed and a longer message that mentions the text do not count.
// A line that is not JSON (a last one that was cut) is not a message. The
// message shapes are those of Claude Code (message.role, a string or text
// blocks) and of Codex (payload.role, input_text blocks, or a user_message event).
func TranscriptHolds(transcript []byte, text string) (bool, error) {
	want := strings.TrimSpace(text)
	for line := range bytes.SplitSeq(transcript, []byte("\n")) {
		if !bytes.Contains(line, []byte(`"user`)) {
			continue
		}
		var entry struct {
			Message userMessage `json:"message"`
			Payload struct {
				userMessage
				Type string          `json:"type"`
				Text json.RawMessage `json:"message"`
			} `json:"payload"`
		}
		if json.Unmarshal(line, &entry) != nil {
			continue
		}
		var event string
		if entry.Payload.Type == "user_message" {
			_ = json.Unmarshal(entry.Payload.Text, &event)
		}
		if entry.Message.sent(want) || entry.Payload.userMessage.sent(want) || event != "" && strings.TrimSpace(event) == want {
			return true, nil
		}
	}
	return false, nil
}

// userMessage is a message of a transcript: its content is a string or blocks of text.
type userMessage struct {
	Role    string          `json:"role"`
	Content json.RawMessage `json:"content"`
}

func (m userMessage) sent(want string) bool {
	if m.Role != "user" {
		return false
	}
	var whole string
	if json.Unmarshal(m.Content, &whole) == nil {
		return strings.TrimSpace(whole) == want
	}
	var blocks []struct {
		Text string `json:"text"`
	}
	if json.Unmarshal(m.Content, &blocks) != nil {
		return false
	}
	for _, b := range blocks {
		if b.Text != "" && strings.TrimSpace(b.Text) == want {
			return true
		}
	}
	return false
}
