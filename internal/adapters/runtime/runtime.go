// Package runtime is the port of the process sessions that harnesses run in
// (§6.1). The domain never depends on what a session is called or where it
// lives: a Handle is opaque outside the Runtime that made it.
//
// Import it under another name (cfruntime), so that it does not hide the
// standard library's runtime.
package runtime

import (
	"context"

	"github.com/Autarquico/codefabric/internal/domain"
)

// NameFor is the name of the session of a Run under a lease epoch (§6.4); it
// is domain.SessionName. The same Run and epoch name the same session, which is
// what lets Start adopt a session instead of making a second one.
func NameFor(runID string, leaseEpoch uint64) string { return domain.SessionName(runID, leaseEpoch) }

// ParseName reads the Run and lease epoch of a session name NameFor wrote.
func ParseName(name string) (runID string, leaseEpoch uint64, ok bool) {
	return domain.ParseSessionName(name)
}

// Spec describes the session of one Run.
//
// Name is derived from the Run and its lease epoch, so that starting the same
// Spec twice is one session. Command is the process that leads it. Capture, if
// set, is a file the output of the terminal is appended to, for processes that
// use the terminal itself; and Ready, if set, is a file the Runtime creates once
// the session is wired, which Command may wait for.
type Spec struct {
	Name    string
	Dir     string
	Command []string
	Capture string
	Ready   string
}

// Handle is the way back to a session. Ref is private to the Runtime named by
// Runtime.
type Handle struct {
	Runtime string
	Ref     string
}

// Status is what a Runtime knows of a session.
type Status struct {
	// Alive: the session exists. The process that leads it may be about to end.
	Alive bool
}

// Runtime starts and watches sessions.
type Runtime interface {
	// Start creates the session of the Spec, or adopts the one that exists
	// under its Name: it is idempotent by Name.
	Start(ctx context.Context, spec Spec) (Handle, error)
	// Wire finishes what makes a session usable: capturing its terminal and opening
	// the gate (Spec.Capture, Spec.Ready). It is idempotent, and Start does it, but
	// a daemon that died half way through Start finds a session that exists and
	// has to complete it without starting another.
	Wire(ctx context.Context, spec Spec) error
	// Stop destroys the session. Stopping one that does not exist is not an error.
	// Ending the processes in an orderly way is the caller's: this is the last resort.
	Stop(ctx context.Context, h Handle) error
	Status(ctx context.Context, h Handle) (Status, error)
	// HandleFor is the handle Start returns for a Name, without starting anything.
	HandleFor(name string) Handle
	// List names the sessions of Runs the Runtime has, whoever started them:
	// those whose name NameFor could have written.
	List(ctx context.Context) ([]string, error)
}

// AttachBinaryProvider is implemented by a Runtime that sessions can be
// attached to from another machine: it says which program attaches, as an
// absolute path, because a non-interactive login on the machine has a PATH of
// its own. runtime is the name that handles of this Runtime carry. Both are
// empty when the Runtime cannot say (it was not checked, or the program is not
// where an absolute, plain path can name it).
type AttachBinaryProvider interface {
	AttachBinary() (runtime, path string)
}
