package harness

import (
	"fmt"
	"slices"

	"github.com/Autarquico/codefabric/internal/domain"
)

// Capabilities is what an adapter supports (§7.2). Each false field means
// its methods return [ErrUnsupported].
type Capabilities struct {
	// Modes are the execution modes it supports (§7.4); the only source of
	// them. A Run that asks for another is refused at eligibility.
	Modes []domain.Mode
	// Resume restarts a native session from a Checkpoint.
	Resume bool
	// NativeCheckpoint exports portable native state.
	NativeCheckpoint bool
	// NeedsInputSignal detects or declares that the Run waits for input.
	NeedsInputSignal bool
	// StructuredInput is SendInput without an attach.
	StructuredInput bool
	// UsageEvents is CollectUsage and usage in Events.
	UsageEvents    bool
	ModelSelection bool
	// ConfigIsolation reads the configuration from a directory per Run
	// (§8.12): ApplyConfig to a TargetRun and RunSpec.ConfigDir.
	ConfigIsolation bool
	// PreapprovedPermissions means the harness runs with its permission
	// prompts skipped: whatever it is asked, it does.
	PreapprovedPermissions bool
	// Install is Install, with DryRun planning without changing the host.
	Install bool
	// GracefulStop is Stop: a courteous stop the harness honours. Without
	// it the signal to the process group is the only stop.
	GracefulStop bool
}

// Supports tells whether m is one of Modes.
func (c Capabilities) Supports(m domain.Mode) bool {
	return slices.Contains(c.Modes, m)
}

// CheckMode is nil when m is supported and [ErrUnsupportedMode] otherwise.
func (c Capabilities) CheckMode(m domain.Mode) error {
	if c.Supports(m) {
		return nil
	}
	return fmt.Errorf("%w: %q (supports %v)", ErrUnsupportedMode, m, c.Modes)
}
