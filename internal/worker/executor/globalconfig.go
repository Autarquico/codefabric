package executor

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/Autarquico/codefabric/internal/adapters/harness"
	"github.com/Autarquico/codefabric/internal/domain"
	"github.com/Autarquico/codefabric/internal/worker/state"
)

// GlobalConfig is the global configuration of the harnesses of this Worker, which
// a Run of a harness without ConfigIsolation runs with (§8.12). *applier.Applier
// implements it.
type GlobalConfig interface {
	// Lock serialises whoever reads or writes the global configuration of a
	// harness; unlock releases it.
	Lock(harnessID string) (unlock func())
	// Holds is nil when the global configuration of the harness already is snap,
	// with nothing edited, skipped or left to apply; otherwise it says why not.
	Holds(ctx context.Context, harnessID string, snap harness.NativeSnapshot) error
}

// hold takes the lock of the global configuration of the harness the Run r
// names, when it has no isolation, for as short as the caller needs: checking and
// launching such a Run does not overlap an apply to it. A harness with isolation
// takes nothing, so it adds no contention.
func (e *Executor) hold(r state.Run) (unlock func()) {
	if e.o.GlobalConfig == nil {
		return func() {}
	}
	id := parseSpec(r.Spec).Requirements.Harness
	if a, ok := e.harnesses.Get(id); !ok || a.Capabilities().ConfigIsolation {
		return func() {}
	}
	return e.o.GlobalConfig.Lock(id)
}

// HarnessBusy is true while a Run of the harness is assigned, accepted or alive
// on this Worker. *Executor is the applier.RunWatcher.
func (e *Executor) HarnessBusy(ctx context.Context, harnessID string) (bool, error) {
	live, err := state.LiveRuns(ctx, e.o.DB.Reader())
	if err != nil {
		return false, err
	}
	return slices.ContainsFunc(live, func(r state.Run) bool { return parseSpec(r.Spec).Requirements.Harness == harnessID }), nil
}

// globalWorkflowVerdict is the answer for a Run that froze a Workflow and runs
// with a harness without ConfigIsolation: it uses the global configuration as it
// is, so it is taken only if that already is the Workflow the Run froze. The Run
// is not materialised and reports no effective configuration digest: none was
// built for it.
func (e *Executor) globalWorkflowVerdict(ctx context.Context, r state.Run, harnessID string) verdict {
	snap, err := e.globalWorkflowSnapshot(ctx, r, harnessID)
	if err == nil {
		unlock := e.hold(r)
		err = e.globalWorkflowHolds(ctx, r, harnessID, snap)
		unlock()
	}
	if err != nil {
		return verdict{domain.CodeHarnessUnavailable, err.Error()}
	}
	return verdict{}
}

// globalWorkflowSnapshot fetches what the Run froze. It takes no lock: it is the
// slow part, and it reads nothing of the host.
func (e *Executor) globalWorkflowSnapshot(ctx context.Context, r state.Run, harnessID string) (harness.NativeSnapshot, error) {
	frozen := parseSpec(r.Spec).Workflow
	if e.o.GlobalConfig == nil || e.o.Workflows == nil {
		return harness.NativeSnapshot{}, fmt.Errorf("the harness %q cannot isolate the configuration of a Workflow and this Worker does not apply Workflows to it", harnessID)
	}
	snap, err := e.workflowSnapshot(ctx, r, frozen, harnessID)
	if err != nil {
		return harness.NativeSnapshot{}, fmt.Errorf("reading the Workflow %s: %w", frozen, err)
	}
	return snap, nil
}

// globalWorkflowHolds checks snap against the global configuration. The caller
// holds the lock of the harness.
func (e *Executor) globalWorkflowHolds(ctx context.Context, r state.Run, harnessID string, snap harness.NativeSnapshot) error {
	if err := e.o.GlobalConfig.Holds(ctx, harnessID, snap); err != nil {
		_, digest, _ := strings.Cut(parseSpec(r.Spec).Workflow, "@")
		return fmt.Errorf("the harness %q has no configuration isolation and the Workflow %s is not what this Worker has applied to it: %w", harnessID, digest, err)
	}
	return nil
}

// configRefused is a Run that cannot be launched because the global
// configuration is no longer what it was accepted for, or cannot be read for good.
type configRefused struct{ err error }

func (c *configRefused) Error() string { return c.err.Error() }
func (c *configRefused) Unwrap() error { return c.err }

// launchUnderGlobalConfig runs launch while the global configuration is what the
// Run froze, so that an apply cannot land between the check and the launch. The
// snapshot is fetched before the lock is taken.
func (e *Executor) launchUnderGlobalConfig(ctx context.Context, r state.Run, launch func() error) error {
	spec := parseSpec(r.Spec)
	a, ok := e.harnesses.Get(spec.Requirements.Harness)
	checked := ok && spec.Workflow != "" && !a.Capabilities().ConfigIsolation
	var snap harness.NativeSnapshot
	if checked {
		var err error
		if snap, err = e.globalWorkflowSnapshot(ctx, r, a.ID()); err != nil {
			if errors.Is(err, errPrepareLater) {
				return err
			}
			return &configRefused{err}
		}
	}
	defer e.hold(r)()
	if checked {
		if err := e.globalWorkflowHolds(ctx, r, a.ID(), snap); err != nil {
			return &configRefused{err}
		}
	}
	return launch()
}
