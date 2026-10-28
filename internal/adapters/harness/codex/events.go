package codex

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"

	"github.com/Autarquico/codefabric/internal/adapters/harness"
)

// line is the part of a `codex exec --json` line the adapter reads. The shape
// is ThreadEvent of codex-rs/exec/src/exec_events.rs (rust-v0.160.1); every
// field is optional so that a missing one is nil, never 0.
type line struct {
	Type     string `json:"type"`
	ThreadID string `json:"thread_id"`
	Usage    *struct {
		InputTokens      *int64 `json:"input_tokens"`
		CachedInput      *int64 `json:"cached_input_tokens"`
		CacheWriteInput  *int64 `json:"cache_write_input_tokens"`
		OutputTokens     *int64 `json:"output_tokens"`
		ReasoningOutputs *int64 `json:"reasoning_output_tokens"`
	} `json:"usage"`
	// Error is the payload of turn.failed; Message the one of a top-level error.
	Error *struct {
		Message string `json:"message"`
	} `json:"error"`
	Message string `json:"message"`
	Item    *struct {
		Type string `json:"type"`
		Text string `json:"text"`
	} `json:"item"`
}

// parser turns lines into events. It keeps the last agent message, which is
// the summary of the turn that ends next.
type parser struct {
	summary string
}

// parse returns the events of one line; a line that is not JSON or not
// understood gives none.
func (p *parser) parse(raw []byte) []harness.Event {
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 || raw[0] != '{' {
		return nil
	}
	var l line
	if json.Unmarshal(raw, &l) != nil {
		return nil
	}
	switch l.Type {
	case "thread.started":
		if l.ThreadID == "" {
			return nil
		}
		return []harness.Event{{Kind: harness.EventSessionStarted, SessionID: l.ThreadID}}
	case "item.completed":
		if l.Item != nil && l.Item.Type == "agent_message" {
			p.summary = l.Item.Text
		}
		return nil
	case "turn.completed":
		var evs []harness.Event
		if l.Usage != nil {
			// Codex counts the cached tokens inside input_tokens and the
			// reasoning tokens inside output_tokens (non_cached_input and
			// blended_total of codex-rs/protocol); UsageEvent counts input
			// without the cache (§14.4).
			evs = append(evs, harness.Event{Kind: harness.EventUsage, Usage: &harness.UsageEvent{
				InputTokens:      uncached(l.Usage.InputTokens, l.Usage.CachedInput),
				CacheReadTokens:  l.Usage.CachedInput,
				CacheWriteTokens: l.Usage.CacheWriteInput,
				OutputTokens:     l.Usage.OutputTokens,
			}})
		}
		evs = append(evs, harness.Event{Kind: harness.EventResult, Result: &harness.Result{Summary: p.summary}})
		p.summary = ""
		return evs
	case "turn.failed":
		msg := ""
		if l.Error != nil {
			msg = l.Error.Message
		}
		p.summary = ""
		return []harness.Event{
			{Kind: harness.EventError, Message: msg},
			{Kind: harness.EventResult, Result: &harness.Result{Failed: true, Summary: msg}},
		}
	case "error":
		return []harness.Event{{Kind: harness.EventError, Message: l.Message}}
	}
	return nil
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

// Events reads the JSONL of `codex exec --json`. Lines that are not JSON,
// that are cut, or whose type is unknown are skipped. Warnings (item.completed
// of type "error") are not events: EventError means the Run failed, and the
// text stays in the log. A read error ends the stream like EOF.
func (Adapter) Events(ctx context.Context, _ harness.Handle, src io.ReadCloser) (<-chan harness.Event, error) {
	if src == nil {
		return nil, errors.New("codex: events: there is no source to read")
	}
	out := make(chan harness.Event)
	go func() {
		defer close(out)
		var p parser
		forEachLine(ctx, src, func(raw []byte) bool {
			for _, ev := range p.parse(raw) {
				select {
				case out <- ev:
				case <-ctx.Done():
					return false
				}
			}
			return true
		})
	}()
	return out, nil
}

// uncached is input without cached, never below zero; unknown when input is.
func uncached(input, cached *int64) *int64 {
	if input == nil || cached == nil {
		return input
	}
	n := max(*input-*cached, 0)
	return &n
}
