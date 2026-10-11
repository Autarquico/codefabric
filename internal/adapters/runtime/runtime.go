// Package runtime is the port of the process sessions that harnesses run in
// (§6.1). The domain never depends on what a session is called or where it
// lives: a Handle is opaque outside the Runtime that made it.
//
// Import it under another name (cfruntime), so that it does not hide the
// standard library's runtime.
package runtime

import (
	"context"
	"fmt"
)

// NameFor is the name of the session of a Run under a lease epoch (§6.4). It is
// derived, never stored apart: the same Run and epoch name the same session,
// which is what lets Start adopt a session instead of making a second one.
func NameFor(runID string, leaseEpoch uint64) string {
	return fmt.Sprintf("cf-%s-e%d", runID, leaseEpoch)
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
	// Stop destroys the session. Stopping one that does not exist is not an error.
	// Ending the processes in an orderly way is the caller's: this is the last resort.
	Stop(ctx context.Context, h Handle) error
	Status(ctx context.Context, h Handle) (Status, error)
	// HandleFor is the handle Start returns for a Name, without starting anything.
	HandleFor(name string) Handle
}
