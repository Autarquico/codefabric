package handoff

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"os"
	"strings"
	"unicode/utf8"

	"github.com/Autarquico/codefabric/internal/domain"
)

// maxPromptRunes is how much of the last request of the user travels as the prompt
// of the Run.
const maxPromptRunes = 200

// LastUserMessage is the last thing the user asked in the transcript of a session,
// leaving out what a harness writes in the name of the user: tool results, the
// expansion of a skill, the command that invoked /handto itself. "" if there is none.
func LastUserMessage(adapter, transcript string) (string, error) {
	pick := claudeUserText
	if adapter == "codex" {
		pick = codexUserText
	}
	f, err := os.Open(transcript) //nolint:gosec // the transcript of a session found under the home of the harness
	if err != nil {
		return "", err
	}
	defer func() { _ = f.Close() }()
	var last string
	r := bufio.NewReader(f)
	for {
		line, err := r.ReadBytes('\n')
		if text := askedByUser(pick(bytes.TrimSpace(line))); text != "" {
			last = text
		}
		if errors.Is(err, io.EOF) {
			return last, nil
		}
		if err != nil {
			return "", err
		}
	}
}

// askedByUser is text if a person typed it, "" if a harness wrote it for them.
func askedByUser(text string) string {
	text = strings.TrimSpace(text)
	switch {
	case text == "", strings.HasPrefix(text, "<"), strings.HasPrefix(text, "# AGENTS.md"),
		strings.HasPrefix(text, "/handto"), strings.HasPrefix(text, "$handto"):
		return ""
	}
	return text
}

func claudeUserText(line []byte) string {
	var l struct {
		Type        string `json:"type"`
		IsMeta      bool   `json:"isMeta"`
		IsSidechain bool   `json:"isSidechain"`
		Message     struct {
			Content json.RawMessage `json:"content"`
		} `json:"message"`
	}
	if json.Unmarshal(line, &l) != nil || l.Type != "user" || l.IsMeta || l.IsSidechain {
		return ""
	}
	var s string
	if json.Unmarshal(l.Message.Content, &s) == nil {
		return s
	}
	var blocks []struct{ Type, Text string }
	if json.Unmarshal(l.Message.Content, &blocks) != nil {
		return ""
	}
	var parts []string
	for _, b := range blocks {
		if b.Type == "text" {
			parts = append(parts, b.Text)
		}
	}
	return strings.Join(parts, "\n")
}

func codexUserText(line []byte) string {
	var l struct {
		Type    string `json:"type"`
		Payload struct {
			Type    string `json:"type"`
			Role    string `json:"role"`
			Content []struct{ Type, Text string }
		} `json:"payload"`
	}
	if json.Unmarshal(line, &l) != nil || l.Type != "response_item" || l.Payload.Type != "message" || l.Payload.Role != "user" {
		return ""
	}
	var parts []string
	for _, b := range l.Payload.Content {
		if b.Type == "input_text" {
			parts = append(parts, b.Text)
		}
	}
	return strings.Join(parts, "\n")
}

// Truncate shortens text to its first maxPromptRunes characters, on one line.
func Truncate(text string) string {
	text = strings.Join(strings.Fields(text), " ")
	if utf8.RuneCountInString(text) <= maxPromptRunes {
		return text
	}
	return string([]rune(text)[:maxPromptRunes]) + "…"
}

// Prompt is what the Run is told when it continues the session.
func Prompt(lastRequest string) string {
	const lead = "Continue the work of this session where it was left."
	if lastRequest == "" {
		return lead
	}
	return lead + " The last thing the user asked: " + Truncate(lastRequest)
}

const nameChars = 24

// TaskName is a slug of the first words of the request, ended by the start of the id of
// the session so that two sessions that begin alike do not collide. The prefix keeps it
// from reading as an ID.
func TaskName(lastRequest, sessionID string) string {
	var b strings.Builder
	dash := true
	for _, r := range strings.ToLower(lastRequest) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			b.WriteRune(r)
			dash = false
		case !dash:
			b.WriteByte('-')
			dash = true
		}
		if b.Len() >= nameChars {
			break
		}
	}
	slug := strings.Trim(b.String(), "-")
	if slug == "" {
		slug = "session"
	}
	suffix := strings.ToLower(strings.ReplaceAll(sessionID, "-", ""))
	if len(suffix) > 6 {
		suffix = suffix[:6]
	}
	name := "handoff-" + slug + "-" + suffix
	if len(name) > domain.MaxSlugLen {
		name = strings.Trim(name[:domain.MaxSlugLen], "-")
	}
	return name
}
