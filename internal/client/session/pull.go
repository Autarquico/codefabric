package session

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/Autarquico/codefabric/internal/adapters/harness/claude"
	"github.com/Autarquico/codefabric/internal/adapters/harness/codex"
)

// SeededSession is the session of this machine that was handed off with the seed whose
// digest is given, also after its pull cleared the marker: it is how a pull knows that the Task was seeded from here and under
// which id its session comes back.
func (s State) SeededSession(digest string) (id string, found bool, err error) {
	entries, err := os.ReadDir(s.dir)
	if errors.Is(err, os.ErrNotExist) {
		return "", false, nil
	}
	if err != nil {
		return "", false, fmt.Errorf("session: reading %s: %w", s.dir, err)
	}
	for _, e := range entries {
		name, ok := strings.CutSuffix(e.Name(), ".json")
		if !ok || !safeID.MatchString(name) {
			continue
		}
		raw, err := os.ReadFile(filepath.Join(s.dir, e.Name()))
		if err != nil {
			continue
		}
		var r Record
		if json.Unmarshal(raw, &r) == nil && r.Seed == digest {
			return name, true, nil
		}
	}
	return "", false, nil
}

// CheckImport says, before anything else changes, whether Import would be refused
// because the session it would replace is open.
func (l *Local) CheckImport(ctx context.Context, req ImportRequest) error {
	if !req.SameClient {
		return nil
	}
	manifest, err := readManifest(req.Archive)
	if err != nil {
		return err
	}
	a, dir, err := l.access(manifest.Adapter)
	if err != nil {
		return err
	}
	path, _, found, err := a.find(dir, manifest.SessionID)
	if err != nil {
		return err
	}
	if !found {
		return ctx.Err()
	}
	live, err := a.live(l, dir, manifest.SessionID, path)
	if err != nil {
		return err
	}
	if live {
		return &LiveError{ID: manifest.SessionID}
	}
	return ctx.Err()
}

// Settle records that a session came back for task: it is bound to it, and when it
// returned under the id it was handed off with, that marker goes away.
func (l *Local) Settle(res ImportResult, original, task string) error {
	if err := l.State.Link(res.SessionID, task); err != nil {
		return err
	}
	if original != "" && res.SessionID == original {
		return l.State.ClearHandedOff(original)
	}
	return nil
}

// ResumeCommand is the command that continues a session of a harness, as a person types
// it.
func ResumeCommand(adapter, id string) []string {
	switch adapter {
	case claude.ID:
		return []string{"claude", "--resume=" + id}
	case codex.ID:
		return []string{"codex", "resume", id}
	}
	return nil
}
