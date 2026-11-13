package codex

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"time"

	"github.com/Autarquico/codefabric/internal/adapters/harness"
)

// sessionIDPattern is the only shape of a session id that reaches a path or an
// argv: the canonical lowercase UUID v7 Codex names its threads with, which
// cannot start with '-'.
var sessionIDPattern = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-7[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`)

func validSessionID(id string) bool { return sessionIDPattern.MatchString(id) }

// rolloutPattern is the name of a rollout: rollout-<UTC time>-<id>.jsonl.
var rolloutPattern = regexp.MustCompile(`^rollout-\d{4}-\d{2}-\d{2}T\d{2}-\d{2}-\d{2}-([0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12})\.jsonl$`)

const rolloutTimeLayout = "2006-01-02T15-04-05"

// Types of the lines of a rollout the adapter looks at.
const (
	lineSessionMeta = "session_meta"
	lineTurnContext = "turn_context"
	lineResponse    = "response_item"
)

// isToolCall says whether a response_item payload is the model calling a tool.
func isToolCall(payloadType string) bool {
	return payloadType == "custom_tool_call" || payloadType == "function_call"
}

// isToolResult says whether a response_item payload is the output of a tool call.
func isToolResult(payloadType string) bool {
	return payloadType == "custom_tool_call_output" || payloadType == "function_call_output"
}

// sessionConfigDir is dir, or what Codex uses when it is empty: CODEX_HOME, or
// ~/.codex.
func sessionConfigDir(dir string) (string, error) {
	if dir != "" {
		return dir, nil
	}
	if env := os.Getenv(ConfigHomeEnv); env != "" {
		return env, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("codex: no configuration directory: %w", err)
	}
	return filepath.Join(home, ".codex"), nil
}

// rollout is a rollout file of a CODEX_HOME.
type rollout struct {
	path string
	id   string
}

// rollouts lists the rollouts under <configDir>/sessions/YYYY/MM/DD, sorted by
// path. It reads the directories itself instead of globbing: the config dir is
// a path of the host and may hold glob characters.
func rollouts(configDir string) ([]rollout, error) {
	var found []rollout
	err := filepath.WalkDir(filepath.Join(configDir, "sessions"), func(p string, d fs.DirEntry, err error) error {
		switch {
		case errors.Is(err, os.ErrNotExist) && p == filepath.Join(configDir, "sessions"):
			return nil
		case err != nil:
			return err
		case d.Type().IsRegular():
			if m := rolloutPattern.FindStringSubmatch(d.Name()); m != nil {
				found = append(found, rollout{path: p, id: m[1]})
			}
		}
		return nil
	})
	return found, err
}

// Locate finds the rollout of the session under <configDir>/sessions. Codex
// does not key sessions by cwd, so ref.Cwd is not used. There are no
// attachments: the rollout is the session.
func (Adapter) Locate(ctx context.Context, ref harness.SessionRef) (harness.SessionFiles, error) {
	if err := ctx.Err(); err != nil {
		return harness.SessionFiles{}, err
	}
	if !validSessionID(ref.SessionID) {
		return harness.SessionFiles{}, fmt.Errorf("codex: %q is not a session id", ref.SessionID)
	}
	configDir, err := sessionConfigDir(ref.ConfigDir)
	if err != nil {
		return harness.SessionFiles{}, err
	}
	all, err := rollouts(configDir)
	if err != nil {
		return harness.SessionFiles{}, fmt.Errorf("codex: looking for the session %s: %w", ref.SessionID, err)
	}
	var paths []string
	for _, r := range all {
		if r.id == ref.SessionID {
			paths = append(paths, r.path)
		}
	}
	switch len(paths) {
	case 0:
		return harness.SessionFiles{}, fmt.Errorf("%w: %s in %s", harness.ErrSessionNotFound, ref.SessionID, configDir)
	case 1:
		return harness.SessionFiles{Transcript: paths[0]}, nil
	}
	return harness.SessionFiles{}, fmt.Errorf("codex: %d rollouts of the session %s in %s", len(paths), ref.SessionID, configDir)
}

// Export writes a tar.gz of {manifest.json, <id>.jsonl}: the rollout as Codex
// wrote it, with no owners, times or gzip header, so the same session gives the
// same bytes. Cut ends it at the last custom_tool_call or function_call.
func (a Adapter) Export(ctx context.Context, ref harness.SessionRef, opts harness.ExportOptions, w io.Writer) (harness.SessionManifest, error) {
	files, err := a.Locate(ctx, ref)
	if err != nil {
		return harness.SessionManifest{}, err
	}
	if err := harness.CheckSessionSize(ID, files, opts.Limit()); err != nil {
		return harness.SessionManifest{}, err
	}
	raw, err := os.ReadFile(files.Transcript)
	if err != nil {
		return harness.SessionManifest{}, fmt.Errorf("codex: reading the session: %w", err)
	}
	transcript, manifest, err := harness.ScanTranscript(ID, raw, opts.CutAtToolUse, inspectLine)
	if err != nil {
		return harness.SessionManifest{}, err
	}
	meta, err := readMeta(transcript)
	if err != nil {
		return harness.SessionManifest{}, err
	}
	if meta.ID != ref.SessionID {
		return harness.SessionManifest{}, fmt.Errorf("codex: the rollout of %s says it is the session %q", ref.SessionID, meta.ID)
	}
	manifest.Adapter, manifest.SessionID, manifest.Cwd = ID, ref.SessionID, meta.Cwd
	if err := harness.WriteSessionArchive(w, manifest, transcript, nil); err != nil {
		return harness.SessionManifest{}, err
	}
	return manifest, nil
}

// inspectLine reads the head of a line of a rollout.
func inspectLine(line []byte) (harness.TranscriptLine, error) {
	var head struct {
		Type    string `json:"type"`
		Payload struct {
			Type       string `json:"type"`
			CLIVersion string `json:"cli_version"`
		} `json:"payload"`
	}
	if err := json.Unmarshal(line, &head); err != nil {
		return harness.TranscriptLine{}, err
	}
	return harness.TranscriptLine{
		Version:    head.Payload.CLIVersion,
		ToolCall:   head.Type == lineResponse && isToolCall(head.Payload.Type),
		ToolResult: head.Type == lineResponse && isToolResult(head.Payload.Type),
	}, nil
}

// sessionMeta is what the first line of a rollout says of the session.
type sessionMeta struct {
	ID        string    `json:"id"`
	Cwd       string    `json:"cwd"`
	Timestamp time.Time `json:"timestamp"`
}

// readMeta reads the session_meta that must be the first line of a rollout.
func readMeta(transcript []byte) (sessionMeta, error) {
	first, _, _ := bytes.Cut(transcript, []byte("\n"))
	var line struct {
		Type    string      `json:"type"`
		Payload sessionMeta `json:"payload"`
	}
	if err := json.Unmarshal(first, &line); err != nil || line.Type != lineSessionMeta {
		return sessionMeta{}, errors.New("codex: the rollout does not start with its session_meta")
	}
	if line.Payload.Timestamp.IsZero() {
		return sessionMeta{}, errors.New("codex: the session_meta has no timestamp")
	}
	return line.Payload, nil
}

// Import installs an archive of Export under sessions/YYYY/MM/DD/ of the
// ConfigDir, named after the time of its session_meta. In the session_meta and
// every turn_context it sets payload.cwd to the cwd of the destination and,
// with a new id, the id of the session_meta; the rest of every line is kept
// byte for byte. It writes a file of its own and renames it, so a rollout that
// is there is whole.
func (Adapter) Import(ctx context.Context, in harness.SessionImport, r io.Reader) (harness.SessionImportResult, error) {
	if in.NewSessionID != "" && !validSessionID(in.NewSessionID) {
		return harness.SessionImportResult{}, fmt.Errorf("codex: %q is not a session id", in.NewSessionID)
	}
	configDir, err := sessionConfigDir(in.ConfigDir)
	if err != nil {
		return harness.SessionImportResult{}, err
	}
	arc, err := harness.ReadSessionArchive(ctx, r, in.Limit(), harness.ArchiveRules{Adapter: ID, ValidID: validSessionID})
	if err != nil {
		return harness.SessionImportResult{}, err
	}
	meta, err := readMeta(arc.Transcript)
	if err != nil {
		return harness.SessionImportResult{}, err
	}
	oldID := arc.Manifest.SessionID
	if meta.ID != oldID {
		return harness.SessionImportResult{}, fmt.Errorf("codex: the rollout says it is the session %q, the manifest %q", meta.ID, oldID)
	}
	id := oldID
	if in.NewSessionID != "" {
		id = in.NewSessionID
	}
	existing, err := rollouts(configDir)
	if err != nil {
		return harness.SessionImportResult{}, fmt.Errorf("codex: looking for the session %s: %w", id, err)
	}
	var stale []string
	for _, e := range existing {
		if e.id == id {
			stale = append(stale, e.path)
		}
	}
	if len(stale) > 0 && !in.Overwrite {
		return harness.SessionImportResult{SessionID: id}, fmt.Errorf("%w: %s", harness.ErrSessionExists, stale[0])
	}

	body, err := harness.MapJSONLines(arc.Transcript, func(line []byte) ([]byte, error) {
		return rewriteLine(line, in.Cwd, oldID, in.NewSessionID)
	})
	if err != nil {
		return harness.SessionImportResult{}, fmt.Errorf("codex: the transcript: %w", err)
	}

	utc := meta.Timestamp.UTC()
	dir := filepath.Join(configDir, "sessions", utc.Format("2006"), utc.Format("01"), utc.Format("02"))
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return harness.SessionImportResult{}, err
	}
	staged, err := writeTemp(dir, body)
	if err != nil {
		return harness.SessionImportResult{}, err
	}
	defer os.Remove(staged) //nolint:errcheck // best effort: after the rename there is nothing to remove
	transcript := filepath.Join(dir, "rollout-"+utc.Format(rolloutTimeLayout)+"-"+id+".jsonl")
	if err := os.Rename(staged, transcript); err != nil {
		return harness.SessionImportResult{}, err
	}
	for _, p := range stale {
		if p != transcript {
			if err := os.Remove(p); err != nil {
				return harness.SessionImportResult{}, err
			}
		}
	}
	manifest := arc.Manifest
	manifest.SessionID = id
	if in.Cwd != "" {
		manifest.Cwd = in.Cwd
	}
	return harness.SessionImportResult{SessionID: id, Transcript: transcript, Manifest: manifest}, nil
}

func writeTemp(dir string, data []byte) (string, error) {
	f, err := os.CreateTemp(dir, ".cf-import-*") // 0600
	if err != nil {
		return "", err
	}
	_, err = f.Write(data)
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		_ = os.Remove(f.Name())
		return "", err
	}
	return f.Name(), nil
}

// rewriteLine sets payload.cwd (when cwd is not empty) on the session_meta and
// turn_context lines, and on the session_meta the payload.id and, if it
// repeats it, payload.session_id, when newID is not empty. Any other line is
// returned as it is.
func rewriteLine(line []byte, cwd, oldID, newID string) ([]byte, error) {
	var head struct {
		Type    string `json:"type"`
		Payload struct {
			ID        string `json:"id"`
			SessionID string `json:"session_id"`
		} `json:"payload"`
	}
	if err := json.Unmarshal(line, &head); err != nil {
		return nil, err
	}
	if head.Type != lineSessionMeta && head.Type != lineTurnContext {
		return line, nil
	}
	edits := map[string]string{}
	if cwd != "" {
		edits["payload.cwd"] = cwd
	}
	if newID != "" && head.Type == lineSessionMeta {
		edits["payload.id"] = newID
		if head.Payload.SessionID == oldID {
			edits["payload.session_id"] = newID
		}
	}
	return harness.SetJSONStrings(line, edits)
}

// onlyRollout is the session of a Run whose Handle does not carry it: each Run
// has a CODEX_HOME of its own, so it is the only rollout in it.
func onlyRollout(configDir string) (string, error) {
	all, err := rollouts(configDir)
	if err != nil {
		return "", err
	}
	ids := make([]string, 0, len(all))
	for _, r := range all {
		if validSessionID(r.id) {
			ids = append(ids, r.id)
		}
	}
	slices.Sort(ids)
	ids = slices.Compact(ids)
	switch len(ids) {
	case 0:
		return "", fmt.Errorf("%w: no session of the Run in %s", harness.ErrSessionNotFound, configDir)
	case 1:
		return ids[0], nil
	}
	return "", fmt.Errorf("codex: %d sessions in %s and the Handle does not say which is the Run's", len(ids), configDir)
}
