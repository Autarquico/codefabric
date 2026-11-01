package doctor

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"

	"github.com/Autarquico/codefabric/internal/adapters/harness"
	"github.com/Autarquico/codefabric/internal/config"
	"github.com/Autarquico/codefabric/internal/support/atomicfile"
)

// Settings are the choices of a Worker that stay on its machine: the
// Controller never sees them.
type Settings struct {
	// Name is the name this Worker joined with: what the actions call it.
	Name string `json:"name,omitempty"`
	// DiskMinFree is a Threshold as text; empty is DefaultMinFree.
	DiskMinFree string `json:"disk_min_free,omitempty"`
	// Harnesses are the ids of the harnesses this Worker offers; nil (absent)
	// is every installed one, as before the join wizard. The command adapter
	// is not a harness of this list: it is always there.
	Harnesses []string `json:"harnesses,omitzero"`
	// SetupPending says the join has not finished: the Worker reports the
	// "setup" component failing and takes no Runs. Absent means done.
	SetupPending bool `json:"setup_pending,omitempty"`
	// Wizard is the progress of the join wizard, kept to resume it.
	Wizard *Progress `json:"wizard,omitempty"`
}

// Progress is where the join wizard stopped and the answers it needs to go on.
// What each Step means belongs to the wizard; here it is only kept.
type Progress struct {
	Step            int      `json:"step"`
	Name            string   `json:"name,omitempty"`
	DiskMinFree     string   `json:"disk_min_free,omitempty"`
	Harnesses       []string `json:"harnesses,omitempty"`
	InitialWorkflow string   `json:"initial_workflow,omitempty"`
}

// SettingsFile is the Worker's config, next to its credentials.
func SettingsFile(p config.Paths) string {
	return filepath.Join(p.ConfigDir, "worker", "config.json")
}

// LoadSettings reads the Worker config; a machine without one has the defaults.
func LoadSettings(p config.Paths) (Settings, error) {
	b, err := os.ReadFile(SettingsFile(p)) //nolint:gosec // the config directory of the Worker
	if errors.Is(err, fs.ErrNotExist) {
		return Settings{}, nil
	}
	if err != nil {
		return Settings{}, fmt.Errorf("doctor: %w", err)
	}
	var s Settings
	if err := json.Unmarshal(b, &s); err != nil {
		return Settings{}, fmt.Errorf("doctor: %s is not a Worker config: %w", SettingsFile(p), err)
	}
	return s, nil
}

// SaveSettings replaces the Worker config.
func SaveSettings(p config.Paths, s Settings) error {
	b, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return fmt.Errorf("doctor: %w", err)
	}
	if err := os.MkdirAll(filepath.Dir(SettingsFile(p)), 0o700); err != nil {
		return fmt.Errorf("doctor: %w", err)
	}
	if err := atomicfile.Write(SettingsFile(p), append(b, '\n'), 0o600); err != nil {
		return fmt.Errorf("doctor: %w", err)
	}
	return nil
}

// MinFree is the disk threshold in effect: override if given, else the config,
// else the default.
func MinFree(p config.Paths, override string) (Threshold, error) {
	if override == "" {
		s, err := LoadSettings(p)
		if err != nil {
			return Threshold{}, err
		}
		override = s.DiskMinFree
	}
	if override == "" {
		override = DefaultMinFree
	}
	return ParseThreshold(override)
}

// WorkerName is the name the Worker joined with, or fallback on a machine whose
// config does not have it.
func WorkerName(p config.Paths, fallback string) string {
	if s, err := LoadSettings(p); err == nil && s.Name != "" {
		return s.Name
	}
	return fallback
}

// Offered keeps the adapters this Worker offers: those whose id is in
// Settings.Harnesses, or all of them if the config does not choose. The command
// adapter is never left out. A config that cannot be read leaves them all.
func Offered(p config.Paths, adapters []harness.Adapter) []harness.Adapter {
	s, err := LoadSettings(p)
	if err != nil || s.Harnesses == nil {
		return adapters
	}
	var out []harness.Adapter
	for _, a := range adapters {
		if a.ID() == "command" || slices.Contains(s.Harnesses, a.ID()) {
			out = append(out, a)
		}
	}
	return out
}

// SetupPending says whether the join of this Worker is unfinished. A config
// that cannot be read counts as done: the Worker must not be locked out of Runs
// by a file the daemon cannot interpret, which the doctor reports on its own.
func SetupPending(p config.Paths) bool {
	s, err := LoadSettings(p)
	return err == nil && s.SetupPending
}
