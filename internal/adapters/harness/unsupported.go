package harness

import (
	"context"
	"errors"
	"io"
)

// ErrUnsupported is what an adapter returns for what it does not support
// (§7.2), always with a zero value.
var ErrUnsupported = errors.New("harness: unsupported")

// ErrUnsupportedMode is a mode outside Capabilities().Modes
// (CF_HARNESS_UNAVAILABLE). It is ErrUnsupported.
var ErrUnsupportedMode = &UnsupportedError{Op: "mode"}

// UnsupportedError is an operation an adapter does not support. It is
// ErrUnsupported.
type UnsupportedError struct {
	// Harness is the ID of the adapter, when known.
	Harness string
	Op      string
}

// NewUnsupportedError is the error of an operation op that is not supported.
func NewUnsupportedError(op string) *UnsupportedError {
	return &UnsupportedError{Op: op}
}

func (e *UnsupportedError) Error() string {
	if e.Harness == "" {
		return "harness: " + e.Op + " is unsupported"
	}
	return "harness " + e.Harness + ": " + e.Op + " is unsupported"
}

// Is makes it ErrUnsupported.
func (e *UnsupportedError) Is(target error) bool { return target == ErrUnsupported }

// Unsupported implements every optional method of Adapter as unsupported.
// An adapter embeds it and overrides what it supports.
type Unsupported struct{}

// Detect is unsupported.
func (Unsupported) Detect(context.Context, Host) (Detection, error) {
	return Detection{}, NewUnsupportedError("detect")
}

// Install is unsupported.
func (Unsupported) Install(context.Context, Host, InstallOptions) (InstallPlan, error) {
	return InstallPlan{}, NewUnsupportedError("install")
}

// Version is unsupported.
func (Unsupported) Version(context.Context, Host) (string, error) {
	return "", NewUnsupportedError("version")
}

// SnapshotConfig is unsupported.
func (Unsupported) SnapshotConfig(context.Context, Host, SnapshotOptions) (NativeSnapshot, error) {
	return NativeSnapshot{}, NewUnsupportedError("snapshot config")
}

// ApplyConfig is unsupported.
func (Unsupported) ApplyConfig(context.Context, Host, NativeSnapshot, ApplyOptions) (ApplyResult, error) {
	return ApplyResult{}, NewUnsupportedError("apply config")
}

// DiffConfig is unsupported.
func (Unsupported) DiffConfig(context.Context, Host, NativeSnapshot) (ConfigDiff, error) {
	return ConfigDiff{}, NewUnsupportedError("diff config")
}

// ValidateConfig is unsupported.
func (Unsupported) ValidateConfig(context.Context, Host) (Validation, error) {
	return Validation{}, NewUnsupportedError("validate config")
}

// Start is unsupported.
func (Unsupported) Start(context.Context, Host, RunSpec) (Launch, error) {
	return Launch{}, NewUnsupportedError("start")
}

// Resume is unsupported.
func (Unsupported) Resume(context.Context, Host, RunSpec, Checkpoint) (Launch, error) {
	return Launch{}, NewUnsupportedError("resume")
}

// Checkpoint is unsupported.
func (Unsupported) Checkpoint(context.Context, Host, Handle) (Checkpoint, error) {
	return Checkpoint{}, NewUnsupportedError("checkpoint")
}

// Stop is unsupported: the process group is the only way to stop the Run.
func (Unsupported) Stop(context.Context, Host, Handle) error {
	return NewUnsupportedError("stop")
}

// Events is unsupported. It leaves src to the caller, as every Events does.
func (Unsupported) Events(context.Context, Handle, io.ReadCloser) (<-chan Event, error) {
	return nil, NewUnsupportedError("events")
}

// SendInput is unsupported.
func (Unsupported) SendInput(context.Context, Host, Handle, Input) error {
	return NewUnsupportedError("send input")
}

// CollectUsage is unsupported.
func (Unsupported) CollectUsage(context.Context, Host, Handle) ([]UsageEvent, error) {
	return nil, NewUnsupportedError("collect usage")
}
