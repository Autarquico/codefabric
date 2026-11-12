// Package session is how a client moves the session of a harness that lives in its own
// home (Claude Code under ~/.claude, Codex under ~/.codex) to and from a Run: it
// finds a session by id, says whether it is open, exports it for a seed (cut at the last
// tool call when it is the session that calls, and blocked when it holds what looks like a
// secret), remembers what was handed over, and imports a session that comes back
// (`cf task pull`). The files are read and written by the adapters of the harnesses; what
// is here is what only a client knows.
package session

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"time"

	"github.com/Autarquico/codefabric/internal/adapters/harness"
	"github.com/Autarquico/codefabric/internal/support/atomicfile"
)

// ErrNotFound is a session that none of the harnesses has in its home.
var ErrNotFound = errors.New("session: not found")

// LiveError is a session that a process has open: exporting it would copy a
// conversation that goes on, and importing over it would break the one that writes.
type LiveError struct{ ID string }

func (e *LiveError) Error() string {
	return fmt.Sprintf("session %s is open in a harness: close it (/exit) and try again", e.ID)
}

// HandedOffError is a session whose marker says its work is somewhere else already.
type HandedOffError struct {
	ID, Task string
}

func (e *HandedOffError) Error() string {
	return fmt.Sprintf("session %s was handed off to task %s and its work goes on there: continuing it from here would make two writers; --force hands it off again", e.ID, e.Task)
}

// Local is the sessions of this machine. The zero value is not usable: see New.
type Local struct {
	State State
	// ConfigDirs replaces the home of a harness (by adapter id): the tests', or a
	// CLAUDE_CONFIG_DIR the environment does not tell.
	ConfigDirs map[string]string

	adapters []access
	getenv   func(string) string
	clock    func() time.Time
	// pidAlive says whether a process exists.
	pidAlive func(int) bool
}

// New is the sessions of this machine, with the state of the handoffs under stateDir.
func New(stateDir string) *Local {
	return &Local{State: NewState(stateDir), adapters: accesses(), getenv: os.Getenv, clock: time.Now, pidAlive: pidAlive}
}

func (l *Local) now() time.Time { return l.clock() }

// Ref is a session found on this machine.
type Ref struct {
	Adapter string
	ID      string
	// Cwd is the directory the session ran in; Transcript the file of the conversation.
	Cwd        string
	Transcript string
}

func (l *Local) access(adapter string) (access, string, error) {
	for _, a := range l.adapters {
		if a.id != adapter {
			continue
		}
		dir := l.ConfigDirs[a.id]
		if dir == "" {
			var err error
			if dir, err = a.home(l.getenv); err != nil {
				return access{}, "", err
			}
		}
		return a, dir, nil
	}
	return access{}, "", fmt.Errorf("session: no harness %q keeps sessions here", adapter)
}

// Find locates the session id in the home of the harness named, or of any of them
// if adapter is empty. An id in more than one is an error that asks for the harness.
func (l *Local) Find(ctx context.Context, id, adapter string) (Ref, error) {
	if !safeID.MatchString(id) {
		return Ref{}, fmt.Errorf("session: %q is not a session id", id)
	}
	var found []Ref
	for _, a := range l.adapters {
		if adapter != "" && a.id != adapter {
			continue
		}
		if err := ctx.Err(); err != nil {
			return Ref{}, err
		}
		_, dir, err := l.access(a.id)
		if err != nil {
			return Ref{}, err
		}
		path, cwd, ok, err := a.find(dir, id)
		if err != nil {
			return Ref{}, fmt.Errorf("session: looking for %s in %s: %w", id, dir, err)
		}
		if ok {
			found = append(found, Ref{Adapter: a.id, ID: id, Cwd: cwd, Transcript: path})
		}
	}
	switch len(found) {
	case 0:
		return Ref{}, fmt.Errorf("%w: no harness on this machine has the session %s", ErrNotFound, id)
	case 1:
		return found[0], nil
	}
	return Ref{}, fmt.Errorf("session: %s exists for more than one harness: say which", id)
}

// Live says whether a process has the session open. Claude Code lists its open
// sessions, so for it the answer is exact. Codex does not, and its session counts as open
// while its rollout was written to in the last two minutes.
func (l *Local) Live(ref Ref) (bool, error) {
	a, dir, err := l.access(ref.Adapter)
	if err != nil {
		return false, err
	}
	return a.live(l, dir, ref.ID, ref.Transcript)
}

// ExportRequest is what Export is asked for.
type ExportRequest struct {
	SessionID string
	// Adapter narrows the search to one harness; empty looks in all.
	Adapter string
	// Caller says the session is the one that runs this command (the session of
	// /handto): it is open by definition, which is allowed, and the export ends at the
	// last tool call, which is that very command.
	Caller bool
	// AllowSensitive exports a session that holds what looks like a secret.
	AllowSensitive bool
	// Force exports a session that was handed off already.
	Force bool
}

// Export is a session ready to be a seed.
type Export struct {
	Ref Ref
	// Archive is the portable archive of the adapter; Digest its sha256 as
	// "sha256:<hex>"; Version the version of the harness that wrote the session, when it
	// says so.
	Archive  []byte
	Digest   string
	Version  string
	Manifest harness.SessionManifest
	// Findings are what looks like a secret, kept when AllowSensitive let it go.
	Findings []Finding
}

// Export finds the session, refuses it if it is open (unless it is the caller's), was
// handed off (unless Force) or holds what looks like a secret (unless AllowSensitive),
// and writes its archive.
func (l *Local) Export(ctx context.Context, req ExportRequest) (Export, error) {
	ref, err := l.Find(ctx, req.SessionID, req.Adapter)
	if err != nil {
		return Export{}, err
	}
	a, dir, err := l.access(ref.Adapter)
	if err != nil {
		return Export{}, err
	}
	rec, err := l.State.Get(ref.ID)
	if err != nil {
		return Export{}, err
	}
	if rec.HandedOff != nil && !req.Force {
		return Export{}, &HandedOffError{ID: ref.ID, Task: rec.HandedOff.Task}
	}
	caller := req.Caller || a.caller(l.getenv) == ref.ID
	if !caller {
		live, err := a.live(l, dir, ref.ID, ref.Transcript)
		if err != nil {
			return Export{}, err
		}
		if live {
			return Export{}, &LiveError{ID: ref.ID}
		}
	}
	var archive bytes.Buffer
	manifest, err := a.porter.Export(ctx, harness.SessionRef{ConfigDir: dir, Cwd: ref.Cwd, SessionID: ref.ID},
		harness.ExportOptions{CutAtToolUse: caller}, &archive)
	if err != nil {
		return Export{}, err
	}
	findings, err := ScanArchive(archive.Bytes())
	if err != nil {
		return Export{}, err
	}
	if len(findings) > 0 && !req.AllowSensitive {
		return Export{Ref: ref, Findings: findings}, &SensitiveError{Findings: findings}
	}
	sum := sha256.Sum256(archive.Bytes())
	return Export{
		Ref: ref, Archive: archive.Bytes(), Digest: "sha256:" + hex.EncodeToString(sum[:]),
		Version: manifest.Version, Manifest: manifest, Findings: findings,
	}, nil
}

// ImportRequest is what Import is asked for.
type ImportRequest struct {
	// Archive is the archive of a session, as Export wrote it.
	Archive []byte
	// Cwd is the working directory of the checkout the session is installed for.
	Cwd string
	// SameClient says the Run was seeded from this machine: the session comes back under
	// its own id, over what is here if that is its prefix and beside it if it is not.
	// Any other session gets an id of its own.
	SameClient bool
}

// ImportResult is where the session went.
type ImportResult struct {
	Adapter   string
	SessionID string
	// Transcript is the file written; Backup the copy of the one it replaced, if any.
	Transcript string
	Backup     string
	// NewID says the session is under another id than it had.
	NewID    bool
	Warnings []string
}

// Import installs a session that comes back from a Run. For a seed of this client the
// session returns under its own id: a local transcript that is a prefix of the incoming
// one is replaced atomically, leaving `.bak`; one that is not (the session went on here
// meanwhile) is not touched and the incoming one gets a new id, with a warning. A session
// that is open under that id is an error and nothing changes.
func (l *Local) Import(ctx context.Context, req ImportRequest) (ImportResult, error) {
	manifest, err := readManifest(req.Archive)
	if err != nil {
		return ImportResult{}, err
	}
	a, dir, err := l.access(manifest.Adapter)
	if err != nil {
		return ImportResult{}, err
	}
	in := harness.SessionImport{ConfigDir: dir, Cwd: req.Cwd}
	install := func(in harness.SessionImport, warnings ...string) (ImportResult, error) {
		res, err := a.porter.Import(ctx, in, bytes.NewReader(req.Archive))
		if err != nil {
			return ImportResult{}, err
		}
		return ImportResult{
			Adapter: a.id, SessionID: res.SessionID, Transcript: res.Transcript,
			NewID: res.SessionID != manifest.SessionID, Warnings: warnings,
		}, nil
	}
	fresh := func(why string) (ImportResult, error) {
		in.NewSessionID = a.newID()
		return install(in, why)
	}
	if !req.SameClient {
		return fresh("the session was not seeded from this machine: it is installed under a new id")
	}

	local, err := a.porter.Locate(ctx, harness.SessionRef{ConfigDir: dir, Cwd: req.Cwd, SessionID: manifest.SessionID})
	if errors.Is(err, harness.ErrSessionNotFound) {
		return install(in)
	}
	if err != nil {
		return ImportResult{}, err
	}
	// Open session: only the one that is here matters; a found one in another cwd is not
	// this id's session to replace.
	if live, err := a.live(l, dir, manifest.SessionID, local.Transcript); err != nil {
		return ImportResult{}, err
	} else if live {
		return ImportResult{}, &LiveError{ID: manifest.SessionID}
	}
	prefix, err := l.isPrefix(ctx, a, local.Transcript, req)
	if err != nil {
		return ImportResult{}, err
	}
	if !prefix {
		return fresh(fmt.Sprintf("the local session %s went on after it was handed off and was not touched: the session that comes back is installed under a new id", manifest.SessionID))
	}
	backup := local.Transcript + ".bak"
	old, err := os.ReadFile(local.Transcript)
	if err != nil {
		return ImportResult{}, err
	}
	if err := atomicfile.Write(backup, old, 0o600); err != nil {
		return ImportResult{}, fmt.Errorf("session: keeping a copy of %s: %w", local.Transcript, err)
	}
	in.Overwrite = true
	res, err := install(in)
	res.Backup = backup
	return res, err
}

// isPrefix says whether the transcript at path is the beginning of the incoming one as it
// would be installed for req.Cwd: the adapter does the rewriting of cwd, so the
// comparison is made on what it writes, in a directory of its own.
func (l *Local) isPrefix(ctx context.Context, a access, path string, req ImportRequest) (bool, error) {
	stage, err := os.MkdirTemp("", "cf-session-stage-")
	if err != nil {
		return false, err
	}
	defer func() { _ = os.RemoveAll(stage) }()
	res, err := a.porter.Import(ctx, harness.SessionImport{ConfigDir: stage, Cwd: req.Cwd}, bytes.NewReader(req.Archive))
	if err != nil {
		return false, err
	}
	incoming, err := os.ReadFile(res.Transcript)
	if err != nil {
		return false, err
	}
	local, err := os.ReadFile(path)
	if err != nil {
		return false, err
	}
	return bytes.HasPrefix(incoming, local), nil
}
