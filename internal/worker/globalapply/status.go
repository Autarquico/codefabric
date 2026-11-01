package globalapply

import (
	"context"
	"slices"

	"github.com/Autarquico/codefabric/internal/adapters/harness"
)

// Applied is a component this Worker applied, as the host holds it now.
type Applied struct {
	Name       string
	WorkflowID string
	Ownership  harness.Ownership
	// Drift: the host differs from what the last apply left.
	Drift bool
	// Missing: the host no longer has it at all.
	Missing bool
}

// Applied lists what the baseline says was applied, compared with the host.
// It needs no Workflow, so the Worker can say it with no Controller.
func (e *Engine) Applied(ctx context.Context) ([]Applied, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	in, err := e.inspector()
	if err != nil {
		return nil, err
	}
	bl, err := readBaseline(e.harnessDir())
	if err != nil {
		return nil, err
	}
	if len(bl.Components) == 0 {
		return nil, nil
	}
	actual, err := in.GlobalFingerprint(ctx, e.Host, nil)
	if err != nil {
		return nil, err
	}
	var out []Applied
	for _, name := range slices.Sorted(keys(bl.Components)) {
		b, found := bl.Components[name], actual[name]
		out = append(out, Applied{
			Name: name, WorkflowID: b.WorkflowID, Ownership: b.Ownership,
			Missing: found == "", Drift: found != "" && found != b.Applied,
		})
	}
	return out, nil
}
