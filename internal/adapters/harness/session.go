package harness

import (
	"context"
	"errors"
	"io"
)

// ErrSessionNotFound is a session the harness has no files for.
var ErrSessionNotFound = errors.New("harness: session not found")

// ErrSessionExists is an Import that would overwrite a session of the
// destination, without being asked to.
var ErrSessionExists = errors.New("harness: the session already exists")

// ErrSessionTooLarge is a session over the size limit of an Export or Import.
var ErrSessionTooLarge = errors.New("harness: the session is too large")

// DefaultMaxSessionBytes is the limit of SessionLimit.MaxBytes when it is 0.
const DefaultMaxSessionBytes int64 = 256 << 20

// SessionPorter moves the native session of a harness between hosts. It is
// separate from Checkpoint and Resume: it works on the files of a session
// that is not running, from the Worker or from a client. An adapter that can
// do it implements it next to Adapter.
type SessionPorter interface {
	// Locate finds the files of a session, or is [ErrSessionNotFound].
	Locate(ctx context.Context, ref SessionRef) (SessionFiles, error)
	// Export writes the session to w as a portable archive that is the same
	// for the same session.
	Export(ctx context.Context, ref SessionRef, opts ExportOptions, w io.Writer) (SessionManifest, error)
	// Import installs an archive made by Export under the cwd of the
	// destination.
	Import(ctx context.Context, in SessionImport, r io.Reader) (SessionImportResult, error)
}

// SessionRef names a session on this host.
type SessionRef struct {
	// ConfigDir is the configuration directory of the harness; empty is the
	// one of the user.
	ConfigDir string
	// Cwd is the working directory the session ran in.
	Cwd       string
	SessionID string
}

// SessionFiles are the files of a session, as absolute paths.
type SessionFiles struct {
	// Transcript is the file of the conversation.
	Transcript string
	// Attachments are the files that belong to it, sorted.
	Attachments []string
}

// SessionLimit bounds the bytes of a session, before compression.
type SessionLimit struct {
	// MaxBytes is the limit; 0 is [DefaultMaxSessionBytes].
	MaxBytes int64
}

// ExportOptions are the options of Export.
type ExportOptions struct {
	SessionLimit
	// CutAtToolUse ends the transcript at the last tool call of the model,
	// before its result: the session a handoff continues from. A session
	// with no tool call is an error.
	CutAtToolUse bool
}

// SessionManifest says what an archive holds.
type SessionManifest struct {
	Adapter string `json:"adapter"`
	// Version is the version of the harness that wrote the session, when the
	// session says it.
	Version   string `json:"version,omitempty"`
	SessionID string `json:"session_id"`
	// Cwd is the working directory of the origin.
	Cwd string `json:"cwd"`
	// Lines are the lines of the transcript in the archive.
	Lines int  `json:"lines"`
	Cut   bool `json:"cut,omitempty"`
	// Warnings are what Export left out or tolerated, e.g. an incomplete last
	// line.
	Warnings []string `json:"warnings,omitempty"`
}

// SessionImport is what Import is given besides the archive.
type SessionImport struct {
	SessionLimit
	// ConfigDir is the configuration directory to install under; empty is the
	// one of the user.
	ConfigDir string
	// Cwd is the working directory of the destination.
	Cwd string
	// NewSessionID installs the session under another id; empty keeps it.
	NewSessionID string
	// Overwrite replaces a session that is already there.
	Overwrite bool
}

// SessionImportResult is what Import installed.
type SessionImportResult struct {
	SessionID string
	// Transcript is the path of the transcript written.
	Transcript string
	Manifest   SessionManifest
}
