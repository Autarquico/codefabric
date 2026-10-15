package harness

import "io/fs"

// NativeSnapshot is the native configuration of a harness, as components
// (§8). It never carries a secret value: see [File].
type NativeSnapshot struct {
	// Harness is the ID of the adapter that took it.
	Harness string
	// SchemaVersion is the version of the layout of Components, owned by the
	// adapter.
	SchemaVersion int
	Components    []Component
}

// Component is one adoptable piece of a configuration: settings, a skills
// directory, a subset of a JSON file.
type Component struct {
	Name      string
	Kind      ComponentKind
	Ownership Ownership
	// Files are its files, with paths relative to the configuration root.
	Files []File
	// Digest is the treedigest/v1 of Files, over their unrendered content.
	Digest string
}

// ComponentKind is how a component maps onto the configuration directory.
type ComponentKind string

// The kinds of component.
const (
	// KindFile is one whole file.
	KindFile ComponentKind = "file"
	// KindDir is a directory, whole.
	KindDir ComponentKind = "dir"
	// KindJSONSubset is some fields of a JSON file that has others.
	KindJSONSubset ComponentKind = "json-subset"
)

// Ownership is who owns a component, which decides what ApplyConfig may do
// to it on a TargetGlobal (see the package documentation).
type Ownership string

// The owners of a component.
const (
	// Managed is owned by CodeFabric: apply writes and deletes it.
	Managed Ownership = "managed"
	// Local is owned by the person: apply never deletes or overwrites it.
	Local Ownership = "local"
)

// File is one file of a component.
type File struct {
	// Path is relative to the configuration root, with slashes.
	Path string
	Mode fs.FileMode
	// Content is unrendered: a secret appears in it as its marker
	// {{cf-secret:<ref>}}, never as its value.
	Content []byte
	// SecretRefs are the refs of the markers in Content, in order of first
	// appearance; what SecretRefs(Content) returns.
	SecretRefs []string
}

// SnapshotOptions are the options of SnapshotConfig.
type SnapshotOptions struct {
	// Components are the names to snapshot; empty is every adoptable one.
	Components []string
}

// ApplyOptions are the options of ApplyConfig.
type ApplyOptions struct {
	// Target is a TargetRun or a TargetGlobal.
	Target Target
	// LinkAuth links the auth of the host into the target, never copying it.
	LinkAuth bool
	// DryRun returns what would change without writing.
	DryRun bool
}

// Target is where ApplyConfig writes: a TargetRun or a TargetGlobal.
type Target interface {
	target()
}

// TargetRun is the configuration directory of one Run (§8.12). Dir is empty
// and ApplyConfig writes the snapshot alone into it.
type TargetRun struct {
	Dir string
}

// TargetGlobal is the native configuration under Host.Home, kept by the
// ownership rules of the package documentation.
type TargetGlobal struct{}

func (TargetRun) target()    {}
func (TargetGlobal) target() {}

// ApplyResult is what ApplyConfig did, or would do under DryRun.
type ApplyResult struct {
	Changes []Change
	// AuthLinks are the symlinks to the auth of the host it made, relative to
	// the target. They are the only symlinks a configuration digest skips;
	// any other one is an error.
	AuthLinks []string
}

// ConfigDiff is how a snapshot and the configuration of a host differ.
type ConfigDiff struct {
	Changes []Change
}

// Change is one component that differs.
type Change struct {
	Name string
	Kind ChangeKind
}

// ChangeKind is how a component differs.
type ChangeKind string

// The kinds of change, from the snapshot to the host.
const (
	// Added is in the snapshot and not on the host.
	Added ChangeKind = "added"
	// Removed is a managed component on the host that the snapshot lacks.
	Removed ChangeKind = "removed"
	// Modified is in both, with different content.
	Modified ChangeKind = "modified"
	// Drift is a managed component edited on the host since it was applied.
	// Only an adapter that knows what it applied tells it from Modified.
	Drift ChangeKind = "drift"
)

// Validation is what ValidateConfig found; no issues is valid.
type Validation struct {
	Issues []ValidationIssue
}

// ValidationIssue is one problem of a configuration.
type ValidationIssue struct {
	// Component is the one at fault; empty is the configuration as a whole.
	Component string
	Message   string
}
