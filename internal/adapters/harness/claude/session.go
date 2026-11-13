package claude

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/Autarquico/codefabric/internal/adapters/harness"
)

var _ harness.SessionPorter = Adapter{}

const subagentsDir = "subagents"

// Locate finds <configDir>/projects/<cwd>/<id>.jsonl and what is under
// <id>/subagents. The tool results, file history and memory are not part of
// the session.
func (Adapter) Locate(ctx context.Context, ref harness.SessionRef) (harness.SessionFiles, error) {
	if err := ctx.Err(); err != nil {
		return harness.SessionFiles{}, err
	}
	if !validSessionID(ref.SessionID) {
		return harness.SessionFiles{}, fmt.Errorf("claude-code: %q is not a session id", ref.SessionID)
	}
	configDir, err := sessionConfigDir(ref.ConfigDir)
	if err != nil {
		return harness.SessionFiles{}, err
	}
	dir := sessionProjectDir(configDir, ref.Cwd, ref.SessionID)
	files := harness.SessionFiles{Transcript: filepath.Join(dir, ref.SessionID+".jsonl")}
	if info, err := os.Stat(files.Transcript); errors.Is(err, os.ErrNotExist) || (err == nil && !info.Mode().IsRegular()) {
		return harness.SessionFiles{}, fmt.Errorf("%w: %s", harness.ErrSessionNotFound, files.Transcript)
	} else if err != nil {
		return harness.SessionFiles{}, fmt.Errorf("claude-code: looking for the session %s: %w", ref.SessionID, err)
	}

	root := filepath.Join(dir, ref.SessionID, subagentsDir)
	err = filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		switch {
		case errors.Is(err, os.ErrNotExist) && p == root:
			return nil
		case err != nil:
			return err
		case d.Type().IsRegular():
			files.Attachments = append(files.Attachments, p)
		}
		return nil
	})
	if err != nil {
		return harness.SessionFiles{}, fmt.Errorf("claude-code: listing the subagents of %s: %w", ref.SessionID, err)
	}
	slices.Sort(files.Attachments)
	return files, nil
}

// Export writes a tar.gz of {manifest.json, <id>.jsonl, <id>/subagents/**}:
// sorted, with no owners, times or gzip header, so the same session gives the
// same bytes.
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
		return harness.SessionManifest{}, fmt.Errorf("claude-code: reading the session: %w", err)
	}
	transcript, manifest, err := scanTranscript(raw, opts.CutAtToolUse)
	if err != nil {
		return harness.SessionManifest{}, err
	}
	manifest.Adapter, manifest.SessionID, manifest.Cwd = ID, ref.SessionID, ref.Cwd
	root := filepath.Join(filepath.Dir(files.Transcript), ref.SessionID)
	attachments := make([]harness.ArchiveFile, 0, len(files.Attachments))
	for _, p := range files.Attachments {
		if err := ctx.Err(); err != nil {
			return harness.SessionManifest{}, err
		}
		data, err := os.ReadFile(p) //nolint:gosec // a path Locate listed
		if err != nil {
			return harness.SessionManifest{}, fmt.Errorf("claude-code: reading a subagent file: %w", err)
		}
		rel, err := filepath.Rel(root, p)
		if err != nil {
			return harness.SessionManifest{}, err
		}
		attachments = append(attachments, harness.ArchiveFile{Name: filepath.ToSlash(rel), Data: data})
	}
	if err := harness.WriteSessionArchive(w, manifest, transcript, attachments); err != nil {
		return harness.SessionManifest{}, err
	}
	return manifest, nil
}

// Import installs an archive of Export under projects/<cwd of the
// destination>/. Per line of JSON it changes only the fields cwd and, with a
// new id, sessionId; the rest of every line is kept byte for byte. It writes
// to a directory of its own and renames, the transcript last, so a session
// that is there is whole.
func (Adapter) Import(ctx context.Context, in harness.SessionImport, r io.Reader) (harness.SessionImportResult, error) {
	if in.NewSessionID != "" && !validSessionID(in.NewSessionID) {
		return harness.SessionImportResult{}, fmt.Errorf("claude-code: %q is not a session id", in.NewSessionID)
	}
	configDir, err := sessionConfigDir(in.ConfigDir)
	if err != nil {
		return harness.SessionImportResult{}, err
	}
	arc, err := harness.ReadSessionArchive(ctx, r, in.Limit(), harness.ArchiveRules{Adapter: ID, ValidID: validSessionID, AttachmentDir: subagentsDir})
	if err != nil {
		return harness.SessionImportResult{}, err
	}
	id := arc.Manifest.SessionID
	if in.NewSessionID != "" {
		id = in.NewSessionID
	}
	dir := projectDir(configDir, in.Cwd)
	transcript := filepath.Join(dir, id+".jsonl")
	if !in.Overwrite {
		if _, err := os.Lstat(transcript); err == nil {
			return harness.SessionImportResult{SessionID: id}, fmt.Errorf("%w: %s", harness.ErrSessionExists, transcript)
		}
	}

	rw := lineRewriter{cwd: in.Cwd}
	if in.NewSessionID != "" {
		rw.sessionID = in.NewSessionID
	}
	body, err := rw.rewrite(arc.Transcript)
	if err != nil {
		return harness.SessionImportResult{}, fmt.Errorf("claude-code: the transcript: %w", err)
	}

	if err := os.MkdirAll(dir, 0o700); err != nil {
		return harness.SessionImportResult{}, err
	}
	stage, err := os.MkdirTemp(dir, ".cf-import-")
	if err != nil {
		return harness.SessionImportResult{}, err
	}
	defer os.RemoveAll(stage) //nolint:errcheck // best effort: what is left is a hidden directory
	for _, f := range arc.Attachments {
		data := f.Data
		if strings.HasSuffix(f.Name, ".jsonl") {
			if data, err = rw.rewrite(data); err != nil {
				return harness.SessionImportResult{}, fmt.Errorf("claude-code: %s: %w", f.Name, err)
			}
		}
		dst := filepath.Join(stage, "attachments", filepath.FromSlash(f.Name))
		if err := os.MkdirAll(filepath.Dir(dst), 0o700); err != nil {
			return harness.SessionImportResult{}, err
		}
		if err := os.WriteFile(dst, data, 0o600); err != nil {
			return harness.SessionImportResult{}, err
		}
	}
	stagedTranscript := filepath.Join(stage, "transcript.jsonl")
	if err := os.WriteFile(stagedTranscript, body, 0o600); err != nil {
		return harness.SessionImportResult{}, err
	}

	// With no transcript (or with Overwrite) what is in <id>/ is the leftover
	// of an Import that died between its renames, or of the session replaced.
	sessionDir := filepath.Join(dir, id)
	if err := os.RemoveAll(sessionDir); err != nil {
		return harness.SessionImportResult{}, err
	}
	if len(arc.Attachments) > 0 {
		if err := os.Rename(filepath.Join(stage, "attachments"), sessionDir); err != nil {
			return harness.SessionImportResult{}, err
		}
	}
	if err := os.Rename(stagedTranscript, transcript); err != nil {
		return harness.SessionImportResult{}, err
	}
	manifest := arc.Manifest
	manifest.SessionID, manifest.Cwd = id, in.Cwd
	return harness.SessionImportResult{SessionID: id, Transcript: transcript, Manifest: manifest}, nil
}

// sessionConfigDir is dir, or what Claude Code uses when it is empty:
// CLAUDE_CONFIG_DIR, or ~/.claude.
func sessionConfigDir(dir string) (string, error) {
	if dir != "" {
		return dir, nil
	}
	if env := os.Getenv(ConfigHomeEnv); env != "" {
		return env, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("claude-code: no configuration directory: %w", err)
	}
	return filepath.Join(home, ".claude"), nil
}

// scanTranscript is harness.ScanTranscript of the lines of Claude Code: a tool
// call is an assistant line with a tool_use, its result a user line with a
// tool_result.
func scanTranscript(raw []byte, cut bool) ([]byte, harness.SessionManifest, error) {
	return harness.ScanTranscript(ID, raw, cut, func(line []byte) (harness.TranscriptLine, error) {
		var head struct {
			Type    string `json:"type"`
			Version string `json:"version"`
			Message struct {
				Content json.RawMessage `json:"content"`
			} `json:"message"`
		}
		if err := json.Unmarshal(line, &head); err != nil {
			return harness.TranscriptLine{}, err
		}
		return harness.TranscriptLine{
			Version:    head.Version,
			ToolCall:   head.Type == "assistant" && hasBlock(head.Message.Content, "tool_use"),
			ToolResult: head.Type == "user" && hasBlock(head.Message.Content, "tool_result"),
		}, nil
	})
}

type contentBlock struct {
	Type string `json:"type"`
}

func hasBlock(content json.RawMessage, kind string) bool {
	var blocks []contentBlock
	if json.Unmarshal(content, &blocks) != nil {
		return false
	}
	return slices.ContainsFunc(blocks, func(b contentBlock) bool { return b.Type == kind })
}

// lineRewriter sets the top-level cwd of every line of JSON, and sessionId
// when it is not empty, leaving every other byte as it was.
type lineRewriter struct {
	cwd       string
	sessionID string
}

func (rw lineRewriter) rewrite(raw []byte) ([]byte, error) {
	edits := map[string]string{}
	if rw.cwd != "" {
		edits["cwd"] = rw.cwd
	}
	if rw.sessionID != "" {
		edits["sessionId"] = rw.sessionID
	}
	return harness.MapJSONLines(raw, func(line []byte) ([]byte, error) { return harness.SetJSONStrings(line, edits) })
}
