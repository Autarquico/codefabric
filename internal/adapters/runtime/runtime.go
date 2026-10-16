// Package runtime is the port of the process sessions that harnesses run in
// (§6.1). The domain never depends on what a session is called or where it
// lives: a Handle is opaque outside the Runtime that made it.
//
// Import it under another name (cfruntime), so that it does not hide the
// standard library's runtime.
package runtime

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"

	"github.com/Autarquico/codefabric/internal/domain"
	"github.com/Autarquico/codefabric/internal/support/atomicfile"
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
//
// CaptureFilter, if set, is a command the captured output goes through on its
// way to Capture: it reads the output of the terminal on its stdin, and what it
// writes on its stdout is appended to Capture. Before the Runtime opens the gate
// the filter must acknowledge in CaptureAck (see AckCapture); without its
// acknowledgement the gate stays closed and Command never gets past it.
type Spec struct {
	Name          string
	Dir           string
	Command       []string
	Capture       string
	CaptureFilter []string
	CaptureAck    string
	Ready         string
}

// captureReady is what a CaptureFilter that is filtering leaves in CaptureAck;
// anything else is why it cannot.
const captureReady = "ready"

// AckCapture is the acknowledgement of a CaptureFilter, written atomically to
// path: ready if it is filtering, and otherwise refused, a reason that must
// carry nothing secret.
func AckCapture(path string, refused error) error {
	content := captureReady
	if refused != nil {
		content = "refused: " + refused.Error()
	}
	return atomicfile.Write(path, []byte(content), 0o600)
}

// ErrCaptureRefused is a CaptureFilter that acknowledged that it cannot filter.
var ErrCaptureRefused = errors.New("runtime: the capture filter refused")

// CaptureAcked says whether the CaptureFilter acknowledged in path that it is
// filtering. It is false with no error while there is no acknowledgement (or
// an empty one, the file of a writer that is not atomic, caught midway), and
// an error wrapping ErrCaptureRefused when the filter said it cannot.
func CaptureAcked(path string) (bool, error) {
	raw, err := os.ReadFile(path) //nolint:gosec // the acknowledgement file of a Spec
	switch {
	case errors.Is(err, fs.ErrNotExist), err == nil && len(raw) == 0:
		return false, nil
	case err != nil:
		return false, fmt.Errorf("runtime: reading the acknowledgement of the capture: %w", err)
	case string(raw) == captureReady:
		return true, nil
	}
	return false, fmt.Errorf("%w: %s", ErrCaptureRefused, raw)
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
