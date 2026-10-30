package claude

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"maps"
	"os"
	"slices"

	"github.com/Autarquico/codefabric/internal/adapters/harness"
)

// usageEvents is the usage a result reports, its only source (the assistant
// messages are not counted too: the result already adds them up). With
// modelUsage there is one event per model, by name; without it, one event with
// no model out of usage. A field Claude Code did not report is nil.
func usageEvents(l line) []harness.Event {
	if len(l.ModelUsage) > 0 {
		var evs []harness.Event
		for _, name := range slices.SortedFunc(maps.Keys(l.ModelUsage), cmp.Compare) {
			m := l.ModelUsage[name]
			evs = append(evs, harness.Event{Kind: harness.EventUsage, Usage: &harness.UsageEvent{
				Model: name, InputTokens: m.Input, OutputTokens: m.Output,
				CacheReadTokens: m.CacheRead, CacheWriteTokens: m.CacheWrite,
			}})
		}
		return evs
	}
	if l.Usage == nil {
		return nil
	}
	return []harness.Event{{Kind: harness.EventUsage, Usage: &harness.UsageEvent{
		InputTokens: l.Usage.Input, OutputTokens: l.Usage.Output,
		CacheReadTokens: l.Usage.CacheRead, CacheWriteTokens: l.Usage.CacheWrite,
	}}}
}

// CollectUsage reads the usage of the Run from its events file. A Run that
// reported none gives an empty slice.
func (Adapter) CollectUsage(ctx context.Context, _ harness.Host, h harness.Handle) ([]harness.UsageEvent, error) {
	if h.EventsPath == "" {
		return nil, errors.New("claude-code: collect usage: the handle has no events path")
	}
	f, err := os.Open(h.EventsPath)
	if err != nil {
		return nil, fmt.Errorf("claude-code: collect usage: %w", err)
	}
	defer func() { _ = f.Close() }()
	usage := []harness.UsageEvent{}
	forEachLine(ctx, f, func(raw []byte) bool {
		for _, ev := range parse(raw) {
			if ev.Usage != nil {
				usage = append(usage, *ev.Usage)
			}
		}
		return true
	})
	if err := ctx.Err(); err != nil {
		return nil, fmt.Errorf("claude-code: collect usage: %w", err)
	}
	return usage, nil
}
