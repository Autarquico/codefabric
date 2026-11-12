package session

import (
	"bufio"
	"bytes"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/Autarquico/codefabric/internal/adapters/harness"
	"github.com/Autarquico/codefabric/internal/adapters/harness/claude"
	"github.com/Autarquico/codefabric/internal/adapters/harness/codex"
)

// access is what this package knows of the sessions of one harness on this machine
// that the adapter does not: where they are when only the id is known, whether one is
// open, and what an id of that harness looks like.
type access struct {
	id     string
	porter harness.SessionPorter
	// home is the directory of the harness: its environment variable, or under $HOME.
	home func(getenv func(string) string) (string, error)
	// find says where the transcript of the session is and the cwd it ran in.
	find func(dir, id string) (path, cwd string, found bool, err error)
	// live says whether a process has the session open.
	live func(l *Local, dir, id, transcript string) (bool, error)
	// owns says whether the process of the harness that has the session open is above
	// this command in the process tree, which is what makes the session the one that
	// calls. Nil if the harness tells it through the environment (caller) only.
	owns func(l *Local, dir, id string) (bool, error)
	// caller is the id of the session that runs this command, if the harness tells it
	// through the environment.
	caller func(getenv func(string) string) string
	newID  func() string
}

func accesses() []access {
	return []access{
		{
			id: claude.ID, porter: claude.Adapter{}, home: homeOf(claude.ConfigHomeEnv, ".claude"),
			find: findClaude, live: liveClaude, owns: ownsClaude, caller: func(func(string) string) string { return "" }, newID: uuid4,
		},
		{
			id: codex.ID, porter: codex.Adapter{}, home: homeOf(codex.ConfigHomeEnv, ".codex"),
			find: findCodex, live: liveCodex, caller: func(getenv func(string) string) string { return getenv("CODEX_SESSION_ID") }, newID: uuid7,
		},
	}
}

func homeOf(env, dir string) func(func(string) string) (string, error) {
	return func(getenv func(string) string) (string, error) {
		if v := getenv(env); v != "" {
			return v, nil
		}
		home, err := os.UserHomeDir()
		if err != nil {
			return "", fmt.Errorf("session: no home directory: %w", err)
		}
		return filepath.Join(home, dir), nil
	}
}

// findClaude looks for projects/*/<id>.jsonl, since the directory is named after a cwd
// that only the transcript tells. A session pulled into a second checkout (a
// worktree) has a copy under each: the one written last is the session.
func findClaude(dir, id string) (path, cwd string, found bool, err error) {
	entries, err := os.ReadDir(filepath.Join(dir, "projects"))
	if errors.Is(err, os.ErrNotExist) {
		return "", "", false, nil
	}
	if err != nil {
		return "", "", false, err
	}
	var latest time.Time
	for _, e := range entries {
		p := filepath.Join(dir, "projects", e.Name(), id+".jsonl")
		if info, err := os.Stat(p); err == nil && info.Mode().IsRegular() && (path == "" || info.ModTime().After(latest)) {
			path, latest = p, info.ModTime()
		}
	}
	if path == "" {
		return "", "", false, nil
	}
	cwd, err = firstString(path, func(line []byte) string {
		var l struct {
			Cwd string `json:"cwd"`
		}
		_ = json.Unmarshal(line, &l)
		return l.Cwd
	})
	if err != nil {
		return "", "", false, err
	}
	if cwd == "" {
		return "", "", false, fmt.Errorf("session %s does not say the directory it ran in", id)
	}
	return path, cwd, true, nil
}

// firstString is the first non-empty value pick finds in the lines of a file.
func firstString(path string, pick func(line []byte) string) (string, error) {
	f, err := os.Open(path) //nolint:gosec // a transcript found under the home of the harness
	if err != nil {
		return "", err
	}
	defer func() { _ = f.Close() }()
	r := bufio.NewReader(f)
	for {
		line, err := r.ReadBytes('\n')
		if v := pick(bytes.TrimSpace(line)); v != "" {
			return v, nil
		}
		if errors.Is(err, io.EOF) {
			return "", nil
		}
		if err != nil {
			return "", err
		}
	}
}

// liveClaude reads sessions/<pid>.json, which Claude Code keeps for each of its open
// sessions: one that names the id and whose process is there is open. A file left by
// a process that died does not count.
func liveClaude(l *Local, dir, id, _ string) (bool, error) {
	pids, err := claudePIDs(l, dir, id)
	return len(pids) > 0, err
}

// ownsClaude says whether one of the processes that have the session open is an ancestor of
// this command: the one that runs it is a tool of that Claude Code.
func ownsClaude(l *Local, dir, id string) (bool, error) {
	pids, err := claudePIDs(l, dir, id)
	for _, pid := range pids {
		if l.hasAncestor(pid) {
			return true, nil
		}
	}
	return false, err
}

// claudePIDs are the live processes whose sessions/<pid>.json names the session.
func claudePIDs(l *Local, dir, id string) ([]int, error) {
	entries, err := os.ReadDir(filepath.Join(dir, "sessions"))
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var pids []int
	for _, e := range entries {
		if !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		raw, err := os.ReadFile(filepath.Join(dir, "sessions", e.Name())) //nolint:gosec // a session file listed under the home of the harness
		if err != nil {
			continue
		}
		var s struct {
			PID       int    `json:"pid"`
			SessionID string `json:"sessionId"`
		}
		if json.Unmarshal(raw, &s) == nil && s.SessionID == id && s.PID > 0 && l.pidAlive(s.PID) {
			pids = append(pids, s.PID)
		}
	}
	return pids, nil
}

// codexQuietFor is how long a rollout must have been left alone for Codex to be taken
// as not writing it: Codex keeps no list of its open sessions (not verified in any
// version), so the only sign is that the file is still growing. A session is a false
// positive for that long after its last write, which only costs a wait.
const codexQuietFor = 2 * time.Minute

func liveCodex(l *Local, _, _, transcript string) (bool, error) {
	info, err := os.Stat(transcript)
	if err != nil {
		return false, err
	}
	return l.now().Sub(info.ModTime()) < codexQuietFor, nil
}

// findCodex looks for the rollout named after the id under sessions/YYYY/MM/DD.
func findCodex(dir, id string) (path, cwd string, found bool, err error) {
	root := filepath.Join(dir, "sessions")
	suffix := "-" + id + ".jsonl"
	err = filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		switch {
		case errors.Is(err, os.ErrNotExist) && p == root:
			return filepath.SkipAll
		case err != nil:
			return err
		case d.Type().IsRegular() && strings.HasPrefix(d.Name(), "rollout-") && strings.HasSuffix(d.Name(), suffix):
			if found {
				return fmt.Errorf("session %s has more than one rollout in %s", id, dir)
			}
			path, found = p, true
		}
		return nil
	})
	if err != nil || !found {
		return "", "", false, err
	}
	cwd, err = firstString(path, func(line []byte) string {
		var l struct {
			Payload struct {
				Cwd string `json:"cwd"`
			} `json:"payload"`
		}
		_ = json.Unmarshal(line, &l)
		return l.Payload.Cwd
	})
	return path, cwd, true, err
}

func pidAlive(pid int) bool {
	err := syscall.Kill(pid, 0)
	return err == nil || errors.Is(err, syscall.EPERM)
}

func uuid4() string {
	u := randomUUID()
	u[6], u[8] = u[6]&0x0f|0x40, u[8]&0x3f|0x80
	return formatUUID(u)
}

// uuid7 is the shape of the ids Codex names its threads with.
func uuid7() string {
	u := randomUUID()
	ms := uint64(time.Now().UnixMilli())
	for i := range 6 {
		u[i] = byte(ms >> (8 * (5 - i))) //nolint:gosec // one byte of the 48-bit timestamp, truncation is the point
	}
	u[6], u[8] = u[6]&0x0f|0x70, u[8]&0x3f|0x80
	return formatUUID(u)
}

func randomUUID() [16]byte {
	var u [16]byte
	_, _ = rand.Read(u[:])
	return u
}

func formatUUID(u [16]byte) string {
	return fmt.Sprintf("%x-%x-%x-%x-%x", u[0:4], u[4:6], u[6:8], u[8:10], u[10:16])
}
