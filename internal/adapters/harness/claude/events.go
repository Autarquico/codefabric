package claude

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"slices"
	"strings"

	"github.com/Autarquico/codefabric/internal/adapters/harness"
)

// askUserQuestion is the tool with which Claude Code asks the person
// something: in a Run nobody is there to answer.
const askUserQuestion = "AskUserQuestion"

// line is the part of a `claude -p --output-format stream-json` line the
// adapter reads (the messages of the Agent SDK, Claude Code 2.1.314). Every
// field is optional so that a missing one is nil or empty, never 0.
type line struct {
	Type      string `json:"type"`
	Subtype   string `json:"subtype"`
	SessionID string `json:"session_id"`
	Model     string `json:"model"`
	// ToolName is the tool of a system/permission_denied.
	ToolName string `json:"tool_name"`
	Message  *struct {
		Content json.RawMessage `json:"content"`
	} `json:"message"`
	// The rest is the result.
	IsError    bool             `json:"is_error"`
	Result     string           `json:"result"`
	Usage      *tokens          `json:"usage"`
	ModelUsage map[string]model `json:"modelUsage"`
}

// tokens is the usage of a result, with the names of the Anthropic API.
type tokens struct {
	Input      *int64 `json:"input_tokens"`
	Output     *int64 `json:"output_tokens"`
	CacheRead  *int64 `json:"cache_read_input_tokens"`
	CacheWrite *int64 `json:"cache_creation_input_tokens"`
}

// model is the usage of one model in modelUsage, in camel case.
type model struct {
	Input      *int64 `json:"inputTokens"`
	Output     *int64 `json:"outputTokens"`
	CacheRead  *int64 `json:"cacheReadInputTokens"`
	CacheWrite *int64 `json:"cacheCreationInputTokens"`
}

// block is an element of the content of an assistant message.
type block struct {
	Type  string `json:"type"`
	Name  string `json:"name"`
	Input struct {
		Questions []struct {
			Question string `json:"question"`
		} `json:"questions"`
	} `json:"input"`
}

// parse returns the events of one line; a line that is not JSON or not
// understood gives none. It keeps nothing between lines: the executor reads
// the stream in chunks of whole lines, and where a chunk ends must not change
// what comes out. So the tool_use and the denial of the same attempt to ask
// are two input_required, which whoever stops the Run takes as one.
func parse(raw []byte) []harness.Event {
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 || raw[0] != '{' {
		return nil
	}
	var l line
	if json.Unmarshal(raw, &l) != nil {
		return nil
	}
	switch {
	case l.Type == "system" && l.Subtype == "init":
		if l.SessionID == "" {
			return nil
		}
		return []harness.Event{{Kind: harness.EventSessionStarted, SessionID: l.SessionID, Model: l.Model}}
	case l.Type == "system" && l.Subtype == "permission_denied" && l.ToolName == askUserQuestion:
		return inputRequired("")
	case l.Type == "assistant" && l.Message != nil:
		if q, ok := askedQuestion(l.Message.Content); ok {
			return inputRequired(q)
		}
	case l.Type == "result":
		return result(l)
	}
	return nil
}

func inputRequired(question string) []harness.Event {
	if question == "" {
		question = "Claude Code tried to ask the user a question (" + askUserQuestion + ")"
	}
	return []harness.Event{{Kind: harness.EventInputRequired, Message: question}}
}

// askedQuestion says whether the content has a tool_use of AskUserQuestion,
// and the first question it carries.
func askedQuestion(content json.RawMessage) (string, bool) {
	var blocks []block
	if json.Unmarshal(content, &blocks) != nil { // a string content is plain text
		return "", false
	}
	i := slices.IndexFunc(blocks, func(b block) bool { return b.Type == "tool_use" && b.Name == askUserQuestion })
	if i < 0 {
		return "", false
	}
	if qs := blocks[i].Input.Questions; len(qs) > 0 {
		return qs[0].Question, true
	}
	return "", true
}

// result is the usage of the Run, then its end. An error result is a failed
// Run; is_error says so even when the subtype is "success".
func result(l line) []harness.Event {
	evs := usageEvents(l)
	if failed := l.IsError || strings.HasPrefix(l.Subtype, "error"); failed {
		msg := l.Result
		if msg == "" {
			msg = l.Subtype
		}
		return append(evs,
			harness.Event{Kind: harness.EventError, Message: msg},
			harness.Event{Kind: harness.EventResult, Result: &harness.Result{Failed: true, Summary: msg}})
	}
	return append(evs, harness.Event{Kind: harness.EventResult, Result: &harness.Result{Summary: l.Result}})
}

// forEachLine calls fn with each line of src, the last one included even
// without its newline, until src ends, fails, ctx is done or fn returns false.
func forEachLine(ctx context.Context, src io.Reader, fn func([]byte) bool) {
	r := bufio.NewReader(src)
	for ctx.Err() == nil {
		raw, err := r.ReadBytes('\n')
		if len(raw) > 0 && !fn(raw) {
			return
		}
		if err != nil {
			return
		}
	}
}

// Events reads the stream-json of `claude -p`. Lines that are not JSON, that
// are cut, or whose type is unknown are skipped. A read error ends the stream
// like EOF.
func (Adapter) Events(ctx context.Context, _ harness.Handle, src io.ReadCloser) (<-chan harness.Event, error) {
	if src == nil {
		return nil, errors.New("claude-code: events: there is no source to read")
	}
	// The reader may stay blocked in src until the caller closes it, which
	// the contract allows; the channel the consumer sees does not wait for it.
	// Only the relay closes out, and nothing is sent after that.
	lines := make(chan []byte)
	go func() {
		defer close(lines)
		forEachLine(ctx, src, func(raw []byte) bool {
			select {
			case lines <- raw:
				return true
			case <-ctx.Done():
				return false
			}
		})
	}()
	out := make(chan harness.Event)
	go func() {
		defer close(out)
		for {
			select {
			case <-ctx.Done():
				return
			case raw, ok := <-lines:
				if !ok {
					return
				}
				for _, ev := range parse(raw) {
					select {
					case out <- ev:
					case <-ctx.Done():
						return
					}
				}
			}
		}
	}()
	return out, nil
}
