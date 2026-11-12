package claude

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/Autarquico/codefabric/internal/adapters/harness"
)

var _ harness.SessionPorter = Adapter{}

const (
	manifestName = "manifest.json"
	subagentsDir = "subagents"
)

// maxArchiveEntries bounds the files of an archive: the byte limit does not
// count the empty ones.
const maxArchiveEntries = 10_000

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
	if err := checkSize(files, opts.MaxBytes); err != nil {
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
	manifestJSON, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		return harness.SessionManifest{}, err
	}

	gz := gzip.NewWriter(w)
	tw := tar.NewWriter(gz)
	put := func(name string, data []byte) error {
		hdr := &tar.Header{Name: name, Mode: 0o644, Size: int64(len(data)), ModTime: time.Unix(0, 0)}
		if err := tw.WriteHeader(hdr); err != nil {
			return err
		}
		_, err := tw.Write(data)
		return err
	}
	if err := put(manifestName, manifestJSON); err != nil {
		return harness.SessionManifest{}, err
	}
	if err := put(ref.SessionID+".jsonl", transcript); err != nil {
		return harness.SessionManifest{}, err
	}
	root := filepath.Join(filepath.Dir(files.Transcript), ref.SessionID)
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
		if err := put(path.Join(ref.SessionID, filepath.ToSlash(rel)), data); err != nil {
			return harness.SessionManifest{}, err
		}
	}
	if err := tw.Close(); err != nil {
		return harness.SessionManifest{}, err
	}
	if err := gz.Close(); err != nil {
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
	arc, err := readArchive(ctx, r, limitOf(in.MaxBytes))
	if err != nil {
		return harness.SessionImportResult{}, err
	}
	id := arc.manifest.SessionID
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
	body, err := rw.rewrite(arc.transcript)
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
	for _, f := range arc.attachments {
		data := f.data
		if strings.HasSuffix(f.name, ".jsonl") {
			if data, err = rw.rewrite(data); err != nil {
				return harness.SessionImportResult{}, fmt.Errorf("claude-code: %s: %w", f.name, err)
			}
		}
		dst := filepath.Join(stage, "attachments", filepath.FromSlash(f.name))
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
	if len(arc.attachments) > 0 {
		if err := os.Rename(filepath.Join(stage, "attachments"), sessionDir); err != nil {
			return harness.SessionImportResult{}, err
		}
	}
	if err := os.Rename(stagedTranscript, transcript); err != nil {
		return harness.SessionImportResult{}, err
	}
	manifest := arc.manifest
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

func limitOf(maxBytes int64) int64 {
	if maxBytes <= 0 {
		return harness.DefaultMaxSessionBytes
	}
	return maxBytes
}

func checkSize(files harness.SessionFiles, maxBytes int64) error {
	limit := limitOf(maxBytes)
	var total int64
	for _, p := range append([]string{files.Transcript}, files.Attachments...) {
		info, err := os.Stat(p)
		if err != nil {
			return fmt.Errorf("claude-code: sizing the session: %w", err)
		}
		total += info.Size()
	}
	if total > limit {
		return fmt.Errorf("%w: %d bytes, the limit is %d", harness.ErrSessionTooLarge, total, limit)
	}
	return nil
}

// scanTranscript checks that every line is JSON, counts them and, when cut,
// drops what follows the last assistant line that has a tool_use. The result
// ends every line with a newline. The last line of a session that is still
// being written, or whose process was killed mid-write, may not be JSON: with
// a cut it is tolerated when it falls after the cut; with no cut it is left
// out if it is incomplete (no newline), with a warning in the manifest. Any
// other line that is not JSON is an error.
func scanTranscript(raw []byte, cut bool) ([]byte, harness.SessionManifest, error) {
	var (
		manifest harness.SessionManifest
		lines    [][]byte
		last     = -1
	)
	for n := 1; len(raw) > 0; n++ {
		var line []byte
		var hasNewline bool
		line, raw, hasNewline = bytes.Cut(raw, []byte("\n"))
		var head struct {
			Type    string `json:"type"`
			Version string `json:"version"`
			Message struct {
				Content json.RawMessage `json:"content"`
			} `json:"message"`
		}
		if err := json.Unmarshal(line, &head); err != nil {
			if len(raw) > 0 || !(cut || !hasNewline) {
				return nil, manifest, fmt.Errorf("claude-code: line %d of the transcript is not a JSON object: %w", n, err)
			}
			// The last line: whether it survives is decided below.
			if !cut {
				manifest.Warnings = append(manifest.Warnings, fmt.Sprintf("line %d of the transcript is incomplete and was left out", n))
				break
			}
			lines = append(lines, line)
			continue
		}
		if head.Version != "" {
			manifest.Version = head.Version
		}
		if head.Type == "assistant" && hasToolUse(head.Message.Content) {
			last = len(lines)
		}
		lines = append(lines, line)
	}
	if cut {
		if last < 0 {
			return nil, manifest, errors.New("claude-code: the session has no tool_use to cut at")
		}
		lines, manifest.Cut = lines[:last+1], true
	}
	manifest.Lines = len(lines)
	var out bytes.Buffer
	for _, l := range lines {
		out.Write(l)
		out.WriteByte('\n')
	}
	return out.Bytes(), manifest, nil
}

type contentBlock struct {
	Type string `json:"type"`
}

func hasToolUse(content json.RawMessage) bool {
	var blocks []contentBlock
	if json.Unmarshal(content, &blocks) != nil {
		return false
	}
	return slices.ContainsFunc(blocks, func(b contentBlock) bool { return b.Type == "tool_use" })
}

type archiveFile struct {
	name string // relative to <id>/
	data []byte
}

type archive struct {
	manifest    harness.SessionManifest
	transcript  []byte
	attachments []archiveFile
}

// readArchive reads an archive of Export into memory, refusing what it does
// not write: other names, links and paths that climb.
func readArchive(ctx context.Context, r io.Reader, limit int64) (archive, error) {
	var arc archive
	gz, err := gzip.NewReader(r)
	if err != nil {
		return arc, fmt.Errorf("claude-code: the session archive: %w", err)
	}
	defer gz.Close() //nolint:errcheck // reading
	tr := tar.NewReader(gz)
	var haveManifest, haveTranscript bool
	remaining, entries := limit, 0
	for {
		if err := ctx.Err(); err != nil {
			return arc, err
		}
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return arc, fmt.Errorf("claude-code: the session archive: %w", err)
		}
		if entries++; entries > maxArchiveEntries {
			return arc, fmt.Errorf("%w: more than %d entries", harness.ErrSessionTooLarge, maxArchiveEntries)
		}
		if hdr.Typeflag != tar.TypeReg {
			return arc, fmt.Errorf("claude-code: the session archive has %q, which is not a file", hdr.Name)
		}
		data, err := io.ReadAll(io.LimitReader(tr, remaining+1))
		if err != nil {
			return arc, fmt.Errorf("claude-code: the session archive: %w", err)
		}
		if remaining -= int64(len(data)); remaining < 0 {
			return arc, fmt.Errorf("%w: the limit is %d bytes", harness.ErrSessionTooLarge, limit)
		}
		switch {
		case hdr.Name == manifestName && !haveManifest:
			if err := json.Unmarshal(data, &arc.manifest); err != nil {
				return arc, fmt.Errorf("claude-code: the manifest of the archive: %w", err)
			}
			if arc.manifest.Adapter != ID || !validSessionID(arc.manifest.SessionID) {
				return arc, fmt.Errorf("claude-code: the archive is not a %s session", ID)
			}
			haveManifest = true
		case !haveManifest:
			return arc, errors.New("claude-code: the archive does not start with its manifest")
		case hdr.Name == arc.manifest.SessionID+".jsonl" && !haveTranscript:
			arc.transcript, haveTranscript = data, true
		default:
			rel, ok := strings.CutPrefix(hdr.Name, arc.manifest.SessionID+"/"+subagentsDir+"/")
			if !ok || !fs.ValidPath(rel) || rel == "." {
				return arc, fmt.Errorf("claude-code: the session archive has an unexpected %q", hdr.Name)
			}
			arc.attachments = append(arc.attachments, archiveFile{name: subagentsDir + "/" + rel, data: data})
		}
	}
	if !haveTranscript {
		return arc, errors.New("claude-code: the archive has no transcript")
	}
	return arc, nil
}

// lineRewriter sets the top-level cwd of every line of JSON, and sessionId
// when it is not empty, leaving every other byte as it was.
type lineRewriter struct {
	cwd       string
	sessionID string
}

func (rw lineRewriter) rewrite(raw []byte) ([]byte, error) {
	var out bytes.Buffer
	for n := 1; len(raw) > 0; n++ {
		var line []byte
		line, raw, _ = bytes.Cut(raw, []byte("\n"))
		line, err := rw.rewriteLine(line)
		if err != nil {
			return nil, fmt.Errorf("line %d is not a JSON object: %w", n, err)
		}
		out.Write(line)
		out.WriteByte('\n')
	}
	return out.Bytes(), nil
}

type span struct {
	start, end int
	value      string
}

func (rw lineRewriter) rewriteLine(line []byte) ([]byte, error) {
	dec := json.NewDecoder(bytes.NewReader(line))
	if tok, err := dec.Token(); err != nil {
		return nil, err
	} else if tok != json.Delim('{') {
		return nil, errors.New("not an object")
	}
	var edits []span
	for dec.More() {
		tok, err := dec.Token()
		if err != nil {
			return nil, err
		}
		key, _ := tok.(string)
		var value json.RawMessage
		if err := dec.Decode(&value); err != nil {
			return nil, err
		}
		want := ""
		switch key {
		case "cwd":
			want = rw.cwd
		case "sessionId":
			want = rw.sessionID
		}
		var current string
		if want == "" || json.Unmarshal(value, &current) != nil {
			continue
		}
		end := int(dec.InputOffset())
		edits = append(edits, span{start: end - len(value), end: end, value: want})
	}
	if _, err := dec.Token(); err != nil {
		return nil, err
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return nil, errors.New("data after the object")
	}
	if len(edits) == 0 {
		return line, nil
	}
	var out bytes.Buffer
	pos := 0
	for _, e := range edits {
		out.Write(line[pos:e.start])
		enc := json.NewEncoder(&out)
		enc.SetEscapeHTML(false)
		if err := enc.Encode(e.value); err != nil {
			return nil, err
		}
		out.Truncate(out.Len() - 1) // the newline of Encode
		pos = e.end
	}
	out.Write(line[pos:])
	return out.Bytes(), nil
}
