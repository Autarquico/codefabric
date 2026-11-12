package doctor

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"

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
	// Checkpoint tunes how this Worker captures checkpoints.
	Checkpoint Checkpoint `json:"checkpoint,omitzero"`
	// Retention is how long this Worker keeps what it has already delivered.
	Retention Retention `json:"retention,omitzero"`
	// JournalMaxSize and JournalHardMax are journal.max_size and journal.hard_max
	// (§1.3) as sizes such as "1GiB"; empty is the default of each.
	JournalMaxSize string `json:"journal_max_size,omitempty"`
	JournalHardMax string `json:"journal_hard_max,omitempty"`
	// SetupPending says the join has not finished: the Worker reports the
	// "setup" component failing and takes no Runs. Absent means done.
	SetupPending bool `json:"setup_pending,omitempty"`
	// Wizard is the progress of the join wizard, kept to resume it.
	Wizard *Progress `json:"wizard,omitempty"`
}

// Checkpoint is checkpoint.* of the Worker config.
type Checkpoint struct {
	// Grace is how long a harness has to end when it is quiesced for a capture,
	// before the group is killed. Unset, the default of Appendix A.
	Grace config.Duration `json:"grace,omitempty"`
}

// Retention of a Worker, as JSON such as "7d".
type Retention struct {
	// Worktrees is how long the worktree of a published Run is kept. Unset, 7d.
	Worktrees config.Duration `json:"worktrees,omitempty"`
}

// Progress is where the join wizard stopped and the answers it needs to go on.
// What each Step means belongs to the wizard; here it is only kept.
type Progress struct {
	Step            int      `json:"step"`
	Name            string   `json:"name,omitempty"`
	DiskMinFree     string   `json:"disk_min_free,omitempty"`
	Harnesses       []string `json:"harnesses,omitempty"`
	InitialWorkflow string   `json:"initial_workflow,omitempty"`
	// PreviousIdentity is a non-secret fingerprint of the credentials the machine
	// held before the invitation was redeemed (NoIdentity if none), so that a
	// recovery can tell the new enrolment from the old one.
	PreviousIdentity string `json:"previous_identity,omitempty"`
}

// NoIdentity is the PreviousIdentity of a machine that held no credentials.
const NoIdentity = "none"

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
	if err != nil {
		return adapters
	}
	return s.Offered(adapters)
}

// Offered is [Offered] for these settings.
func (s Settings) Offered(adapters []harness.Adapter) []harness.Adapter {
	if s.Harnesses == nil {
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

// SameHarnesses says whether two Settings.Harnesses choose the same: absent
// (every installed one) is not the same as an empty choice.
func SameHarnesses(a, b []string) bool {
	if (a == nil) != (b == nil) {
		return false
	}
	x, y := slices.Clone(a), slices.Clone(b)
	slices.Sort(x)
	slices.Sort(y)
	return slices.Equal(x, y)
}

// SetupPending says whether the join of this Worker is unfinished. A config
// that cannot be read counts as done: the Worker must not be locked out of Runs
// by a file the daemon cannot interpret, which the doctor reports on its own.
func SetupPending(p config.Paths) bool {
	s, err := LoadSettings(p)
	return err == nil && s.SetupPending
}

// Defaults of journal.max_size and journal.hard_max (Appendix A).
const (
	DefaultJournalMaxSize uint64 = 1 << 30
	DefaultJournalHardMax uint64 = 5 << 30
)

// JournalLimits are the sizes at which the journal of the Worker degrades (max)
// and truncates (hard). A config that cannot be read, or whose sizes are not
// valid, gives the defaults and the reason, so the Worker keeps a bound.
func JournalLimits(p config.Paths) (maxSize, hardMax uint64, err error) {
	maxSize, hardMax = DefaultJournalMaxSize, DefaultJournalHardMax
	s, err := LoadSettings(p)
	if err != nil {
		return maxSize, hardMax, err
	}
	if s.JournalMaxSize != "" {
		if maxSize, err = parseSize("journal_max_size", s.JournalMaxSize); err != nil {
			return DefaultJournalMaxSize, DefaultJournalHardMax, err
		}
	}
	if s.JournalHardMax != "" {
		if hardMax, err = parseSize("journal_hard_max", s.JournalHardMax); err != nil {
			return DefaultJournalMaxSize, DefaultJournalHardMax, err
		}
	}
	if hardMax < maxSize {
		return DefaultJournalMaxSize, DefaultJournalHardMax, fmt.Errorf("doctor: journal_hard_max (%d bytes) is below journal_max_size (%d bytes)", hardMax, maxSize)
	}
	return maxSize, hardMax, nil
}

func parseSize(key, text string) (uint64, error) {
	upper := strings.ToUpper(strings.TrimSpace(text))
	for _, u := range sizeUnits {
		digits, ok := strings.CutSuffix(upper, u.suffix)
		if !ok {
			continue
		}
		n, err := strconv.ParseUint(strings.TrimSpace(digits), 10, 64)
		if err != nil || n == 0 || n > (1<<62)/u.factor {
			break
		}
		return n * u.factor, nil
	}
	return 0, fmt.Errorf("doctor: %s must be a size such as 1GiB, got %q", key, text)
}
