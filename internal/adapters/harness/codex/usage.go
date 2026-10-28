package codex

import (
	"context"
	"errors"
	"fmt"
	"os"

	"github.com/Autarquico/codefabric/internal/adapters/harness"
)

// CollectUsage reads the usage of the Run from its events file. A Run that
// reported none gives an empty slice; a field Codex did not report is nil.
func (Adapter) CollectUsage(ctx context.Context, _ harness.Host, h harness.Handle) ([]harness.UsageEvent, error) {
	if h.EventsPath == "" {
		return nil, errors.New("codex: collect usage: the handle has no events path")
	}
	f, err := os.Open(h.EventsPath)
	if err != nil {
		return nil, fmt.Errorf("codex: collect usage: %w", err)
	}
	defer func() { _ = f.Close() }()
	usage := []harness.UsageEvent{}
	var p parser
	forEachLine(ctx, f, func(raw []byte) bool {
		for _, ev := range p.parse(raw) {
			if ev.Usage != nil {
				usage = append(usage, *ev.Usage)
			}
		}
		return true
	})
	if err := ctx.Err(); err != nil {
		return nil, fmt.Errorf("codex: collect usage: %w", err)
	}
	return usage, nil
}
