package harness

import (
	"bytes"
	"encoding/json"
)

// ContinuePrompt is what a headless process that resumes a session already
// holding its prompt is asked: the original prompt is not repeated, but the
// CLI needs one to run.
const ContinuePrompt = "Continue where you left off."

// TranscriptHolds tells whether a transcript of JSON lines contains text,
// JSON-escaped, anywhere (it does not tell where). A text that the harness
// wrote with other escapes than ours is not found: the caller sends it again,
// which is the safe side.
func TranscriptHolds(transcript []byte, text string) (bool, error) {
	var needle bytes.Buffer
	enc := json.NewEncoder(&needle)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(text); err != nil {
		return false, err
	}
	quoted := bytes.TrimSpace(needle.Bytes())
	return bytes.Contains(transcript, quoted[1:len(quoted)-1]), nil
}
