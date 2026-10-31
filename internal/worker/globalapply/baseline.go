package globalapply

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"

	"github.com/Autarquico/codefabric/internal/adapters/harness"
	"github.com/Autarquico/codefabric/internal/support/atomicfile"
)

const baselineName = "baseline.json"

// baseline is what the Worker applied to one harness, by component: the base
// of the three-way drift. It lives in the data directory of the Worker, apart
// from the host configuration it describes.
type baseline struct {
	Version    int                      `json:"version"`
	Components map[string]baselineEntry `json:"components"`
}

type baselineEntry struct {
	WorkflowID      string            `json:"workflow_id,omitempty"`
	WorkflowVersion string            `json:"workflow_version,omitempty"`
	Ownership       harness.Ownership `json:"ownership"`
	// Desired is the digest of the component as the Workflow has it
	// (unrendered): what tells a new version from an old one.
	Desired string `json:"desired"`
	// Applied is the fingerprint of the host right after the apply
	// (harness.GlobalInspector): what tells an edit on the host from none.
	Applied string `json:"applied"`
}

func baselinePath(harnessDir string) string { return filepath.Join(harnessDir, baselineName) }

func readBaseline(harnessDir string) (baseline, error) {
	b, err := os.ReadFile(baselinePath(harnessDir)) //nolint:gosec // under the data directory
	if errors.Is(err, fs.ErrNotExist) {
		return baseline{Version: 1, Components: map[string]baselineEntry{}}, nil
	}
	if err != nil {
		return baseline{}, fmt.Errorf("globalapply: %w", err)
	}
	var bl baseline
	if err := json.Unmarshal(b, &bl); err != nil || bl.Version != 1 {
		return baseline{}, fmt.Errorf("globalapply: %s is not a version 1 baseline", baselinePath(harnessDir))
	}
	if bl.Components == nil {
		bl.Components = map[string]baselineEntry{}
	}
	return bl, nil
}

func writeBaseline(harnessDir string, bl baseline) error {
	b, err := json.MarshalIndent(bl, "", "  ")
	if err != nil {
		return fmt.Errorf("globalapply: %w", err)
	}
	if err := os.MkdirAll(harnessDir, 0o700); err != nil {
		return fmt.Errorf("globalapply: %w", err)
	}
	if err := atomicfile.Write(baselinePath(harnessDir), append(b, '\n'), 0o600); err != nil {
		return fmt.Errorf("globalapply: %w", err)
	}
	return nil
}
