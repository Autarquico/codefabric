package harness

import (
	"context"
	"io"

	"github.com/Autarquico/codefabric/internal/domain"
)

// Adapter is one harness (§7.2): what CodeFabric knows of a program that does
// the work of a Run. Every method but ID and Capabilities is optional: an
// adapter that does not support one returns an error that is
// [ErrUnsupported], usually by embedding [Unsupported].
type Adapter interface {
	// ID is the name a Run asks for in requirements.harness. It matches
	// ^[a-z][a-z0-9-]{0,31}$.
	ID() string
	// Capabilities is what the adapter supports. It is a constant: the same
	// value on every call.
	Capabilities() Capabilities

	// Detect tells whether the harness is installed on the host. A harness
	// that is not there is Installed=false with a nil error; an error means
	// the detection itself failed.
	Detect(ctx context.Context, host Host) (Detection, error)
	// Install installs the harness, or under DryRun says how it would.
	Install(ctx context.Context, host Host, opts InstallOptions) (InstallPlan, error)
	Version(ctx context.Context, host Host) (string, error)

	// SnapshotConfig reads the native configuration of the host.
	SnapshotConfig(ctx context.Context, host Host, opts SnapshotOptions) (NativeSnapshot, error)
	// ApplyConfig writes a snapshot to opts.Target, with the ownership rules
	// of the package documentation.
	ApplyConfig(ctx context.Context, host Host, snap NativeSnapshot, opts ApplyOptions) (ApplyResult, error)
	// DiffConfig compares a snapshot with the configuration of the host.
	DiffConfig(ctx context.Context, host Host, snap NativeSnapshot) (ConfigDiff, error)
	ValidateConfig(ctx context.Context, host Host) (Validation, error)

	// Start builds the process of a Run without starting it. It is
	// idempotent: the same spec gives the same Launch and leaves the same
	// files. A mode outside Capabilities().Modes is [ErrUnsupportedMode].
	Start(ctx context.Context, host Host, run RunSpec) (Launch, error)
	// Resume is Start from a checkpoint, with its guarantees.
	Resume(ctx context.Context, host Host, run RunSpec, cp Checkpoint) (Launch, error)
	Checkpoint(ctx context.Context, host Host, h Handle) (Checkpoint, error)
	// Stop asks the harness to stop. It is a courtesy: the authoritative stop
	// is the signal to the process group, and [ErrUnsupported] means that is
	// the only one.
	Stop(ctx context.Context, host Host, h Handle) error

	// Events parses src into events, in order, and closes the channel when
	// src ends or ctx is done. src belongs to the caller: when ctx is done
	// the adapter stops sending and returns, and the caller closes src to
	// unblock a read in progress. Lines it does not understand are skipped,
	// never turned into events; a tolerant parser cannot recover JSON
	// interleaved with other output on the same stream.
	Events(ctx context.Context, h Handle, src io.ReadCloser) (<-chan Event, error)
	SendInput(ctx context.Context, host Host, h Handle, in Input) error
	// CollectUsage reads the usage of a Run from what it left in h.
	CollectUsage(ctx context.Context, host Host, h Handle) ([]UsageEvent, error)
}

// RunSpec is what Start and Resume are given to build the process of a Run.
type RunSpec struct {
	RunID      domain.ID
	LeaseEpoch uint64
	Mode       domain.Mode
	Prompt     string
	// Model is the model the Run asks for; empty leaves it to the harness.
	Model string
	// RunDir is the directory of the Run on the Worker, where the adapter may
	// leave what its process needs.
	RunDir string
	// ConfigDir is where the configuration of the Run was materialised
	// (§8.12); empty when the adapter has no ConfigIsolation.
	ConfigDir string
	// Worktree is where the process runs.
	Worktree string
	// OutputPath is the file the wrapper writes stdout and stderr to.
	OutputPath string
	// EventsPath is the file of the structured stream, when there is one
	// apart from OutputPath (see Launch.StructuredStdout).
	EventsPath string
}

// Handle names a Run that has started. It is a value the Worker rebuilds
// from disk, with no pid in it: the process belongs to the Runtime.
type Handle struct {
	RunID      domain.ID
	LeaseEpoch uint64
	Mode       domain.Mode
	RunDir     string
	ConfigDir  string
	Worktree   string
	OutputPath string
	EventsPath string
}

// Detection is what Detect found.
type Detection struct {
	Installed bool
	// Path is the absolute path of the binary, when Installed.
	Path string
}

// InstallOptions are the options of Install.
type InstallOptions struct {
	// DryRun returns the plan without running it or changing the host.
	DryRun bool
}

// InstallPlan is the steps of an install, in order.
type InstallPlan struct {
	Steps []InstallStep
}

// InstallStep is one command of an install.
type InstallStep struct {
	// Summary says what the step does, for a person.
	Summary string
	Argv    []string
}

// Input is what SendInput gives a Run that waits for it.
type Input struct {
	Text string
}
