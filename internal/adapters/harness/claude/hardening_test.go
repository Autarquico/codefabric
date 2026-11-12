package claude_test

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/Autarquico/codefabric/internal/adapters/harness"
	"github.com/Autarquico/codefabric/internal/adapters/harness/claude"
	"github.com/Autarquico/codefabric/internal/domain"
)

const hardenedID = "11111111-2222-4333-8444-555555555555"

type entry struct {
	name, body, link string
	typ              byte
}

func file(name, body string) entry { return entry{name: name, body: body, typ: tar.TypeReg} }

func manifestOf(id string) entry {
	return file("manifest.json", `{"adapter":"claude-code","session_id":"`+id+`"}`)
}

func buildArchive(t *testing.T, entries ...entry) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for _, e := range entries {
		hdr := &tar.Header{Name: e.name, Typeflag: e.typ, Linkname: e.link, Mode: 0o644, Size: int64(len(e.body))}
		if e.typ != tar.TypeReg {
			hdr.Size = 0
		}
		if err := tw.WriteHeader(hdr); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write([]byte(e.body)); err != nil && e.typ == tar.TypeReg {
			t.Fatal(err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func wellFormed(t *testing.T, extra ...entry) []byte {
	t.Helper()
	return buildArchive(t, append([]entry{manifestOf(hardenedID), file(hardenedID+".jsonl", "{\"type\":\"user\"}\n")}, extra...)...)
}

func emptyTree(t *testing.T, dir string) {
	t.Helper()
	if entries, err := os.ReadDir(dir); err != nil || len(entries) != 0 {
		t.Fatalf("the destination was touched: %v %v", entries, err)
	}
}

// SEC-1: an id from outside never reaches an argv or a path unless it is a UUID.
func TestSession_AnIDThatIsNotAUUIDIsRefusedEverywhere(t *testing.T) {
	bad := []string{
		"--dangerously-skip-permissions", "-x", "../x", "", "ABCDEF12-2222-4333-8444-555555555555",
		hardenedID + "0", "11111111222243338444555555555555", "x/" + hardenedID,
	}
	h := hostWithBinary(t, "#!/bin/sh\n")
	for _, id := range bad {
		t.Run(id, func(t *testing.T) {
			cfg := t.TempDir()
			a := claude.Adapter{}
			if _, err := a.Locate(t.Context(), harness.SessionRef{ConfigDir: cfg, Cwd: "/x", SessionID: id}); err == nil || errors.Is(err, harness.ErrSessionNotFound) {
				t.Errorf("Locate: err = %v", err)
			}
			in := harness.SessionImport{ConfigDir: cfg, Cwd: "/x", NewSessionID: id}
			if id != "" {
				if _, err := a.Import(t.Context(), in, bytes.NewReader(wellFormed(t))); err == nil {
					t.Error("Import accepted it as the new id")
				}
			}
			cp := harness.Checkpoint{Harness: claude.ID, Format: claude.CheckpointFormat, Data: buildArchive(t, manifestOf(id), file(id+".jsonl", "{}\n"))}
			run := nextRun(t, domain.ModeHeadless, "go")
			l, err := a.Resume(t.Context(), h, run, cp)
			if err == nil || len(l.Argv) != 0 {
				t.Errorf("Resume: launch = %+v, err = %v", l, err)
			}
			emptyTree(t, cfg)
			if _, err := os.Stat(filepath.Join(run.ConfigDir, "projects")); !os.IsNotExist(err) {
				t.Errorf("Resume touched the ConfigDir of the Run: %v", err)
			}
		})
	}
}

func TestCheckpointRefusesATranscriptThatIsNotNamedByAUUID(t *testing.T) {
	root := t.TempDir()
	hd := harness.Handle{RunID: "r9", LeaseEpoch: 1, ConfigDir: filepath.Join(root, "config"), Worktree: filepath.Join(root, "wt")}
	writeSessionFile(t, hd.ConfigDir, hd.Worktree, "-x", "hello")
	if _, err := (claude.Adapter{}).Checkpoint(t.Context(), nil, hd); !errors.Is(err, harness.ErrSessionNotFound) {
		t.Fatalf("err = %v, want ErrSessionNotFound", err)
	}
}

// CODE-1: the name of the directory of a cwd past 200 characters (spike
// addendum, case 7, measured against claude 2.1.319).
func longCwd() string {
	return "/private/tmp/claude-501/-Users-alex-src-autarqui-co-codefabric/040d4f33-94f4-4897-9083-b73955d41954/scratchpad/spike/" +
		strings.Repeat("a", 120) + "/" + strings.Repeat("b", 100)
}

func prefix200(cwd string) string {
	return regexp.MustCompile(`[^a-zA-Z0-9]`).ReplaceAllString(cwd, "-")[:200]
}

func TestImportNamesTheDirectoryOfALongCwdAsClaudeCodeDoes(t *testing.T) {
	cfg, cwd := t.TempDir(), longCwd()
	res, err := claude.Adapter{}.Import(t.Context(), harness.SessionImport{ConfigDir: cfg, Cwd: cwd}, bytes.NewReader(wellFormed(t)))
	if err != nil {
		t.Fatal(err)
	}
	want := filepath.Join(cfg, "projects", prefix200(cwd)+"-lixkxq", hardenedID+".jsonl")
	if res.Transcript != want {
		t.Errorf("transcript = %s, want %s", res.Transcript, want)
	}
}

func TestLocateToleratesAnotherHashSuffix(t *testing.T) {
	cfg, cwd := t.TempDir(), longCwd()
	dir := filepath.Join(cfg, "projects", prefix200(cwd)+"-zzzzzz")
	if err := os.MkdirAll(dir, 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, hardenedID+".jsonl"), []byte("{}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	files, err := claude.Adapter{}.Locate(t.Context(), harness.SessionRef{ConfigDir: cfg, Cwd: cwd, SessionID: hardenedID})
	if err != nil || filepath.Dir(files.Transcript) != dir {
		t.Fatalf("files = %+v, err = %v", files, err)
	}
}

func TestStartResumesTheSessionOfALongCwd(t *testing.T) {
	h := hostWithBinary(t, "#!/bin/sh\n")
	run := spec(domain.ModeHeadless)
	run.ConfigDir, run.Worktree = t.TempDir(), longCwd()
	dir := filepath.Join(run.ConfigDir, "projects", prefix200(run.Worktree)+"-lixkxq")
	if err := os.MkdirAll(dir, 0o750); err != nil {
		t.Fatal(err)
	}
	id := claude.SessionID("r1", 1)
	if err := os.WriteFile(filepath.Join(dir, id+".jsonl"), []byte("{}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	l, err := claude.Adapter{}.Start(t.Context(), h, run)
	if err != nil || !slices.Contains(l.Argv, "--resume="+id) {
		t.Fatalf("argv = %q, err = %v", l.Argv, err)
	}
}

// CODE-2: an Import that died between its renames leaves <id>/ with no
// transcript, and the next one still installs the session.
func TestImportRetriesAfterAnInterruptedImport(t *testing.T) {
	cfg := t.TempDir()
	dir := filepath.Join(cfg, "projects", "-home-x-repo")
	if err := os.MkdirAll(filepath.Join(dir, hardenedID, "subagents"), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, hardenedID, "subagents", "stale.jsonl"), []byte("{}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	arc := wellFormed(t, file(hardenedID+"/subagents/agent-1.jsonl", "{}\n"))
	if _, err := (claude.Adapter{}).Import(t.Context(), harness.SessionImport{ConfigDir: cfg, Cwd: "/home/x/repo"}, bytes.NewReader(arc)); err != nil {
		t.Fatal(err)
	}
	got, err := filepath.Glob(filepath.Join(dir, hardenedID, "subagents", "*"))
	if err != nil || len(got) != 1 || filepath.Base(got[0]) != "agent-1.jsonl" {
		t.Fatalf("subagents = %v, %v", got, err)
	}
}

// CODE-5 and SEC-2: what readArchive refuses leaves the ConfigDir untouched.
func TestImportRefusesWhatItDoesNotWrite(t *testing.T) {
	many := []entry{manifestOf(hardenedID), file(hardenedID+".jsonl", "{}\n")}
	for i := 0; i <= 10_000; i++ {
		many = append(many, file(hardenedID+"/subagents/"+"d/"+strconv.Itoa(i), ""))
	}
	cases := []struct {
		name    string
		archive []byte
		max     int64
		want    error
	}{
		{"a symlink", buildArchive(t, manifestOf(hardenedID), file(hardenedID+".jsonl", "{}\n"),
			entry{name: hardenedID + "/subagents/l", typ: tar.TypeSymlink, link: "/etc/passwd"}), 0, nil},
		{"a path that climbs", wellFormed(t, file(hardenedID+"/subagents/../../x", "x")), 0, nil},
		{"an unknown name", wellFormed(t, file("other.txt", "x")), 0, nil},
		{"outside subagents", wellFormed(t, file(hardenedID+"/tool-results/x", "x")), 0, nil},
		{"the transcript before the manifest", buildArchive(t, file(hardenedID+".jsonl", "{}\n"), manifestOf(hardenedID)), 0, nil},
		{"no transcript", buildArchive(t, manifestOf(hardenedID)), 0, nil},
		{"a manifest of another harness", buildArchive(t, file("manifest.json", `{"adapter":"codex","session_id":"`+hardenedID+`"}`), file(hardenedID+".jsonl", "{}\n")), 0, nil},
		{"over the byte limit", wellFormed(t, file(hardenedID+"/subagents/big", strings.Repeat("x", 100))), 64, harness.ErrSessionTooLarge},
		{"too many entries", buildArchive(t, many...), 0, harness.ErrSessionTooLarge},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			cfg := t.TempDir()
			in := harness.SessionImport{ConfigDir: cfg, Cwd: "/home/x/repo"}
			in.MaxBytes = c.max
			_, err := claude.Adapter{}.Import(t.Context(), in, bytes.NewReader(c.archive))
			if err == nil || (c.want != nil && !errors.Is(err, c.want)) {
				t.Fatalf("err = %v, want %v", err, c.want)
			}
			emptyTree(t, cfg)
		})
	}
}

// CODE-9: Export refuses a transcript that is not JSON lines.
func TestExportRefusesATranscriptThatIsNotJSONLines(t *testing.T) {
	cfg, cwd := t.TempDir(), "/home/x/repo"
	dir := filepath.Join(cfg, "projects", "-home-x-repo")
	if err := os.MkdirAll(dir, 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, hardenedID+".jsonl"), []byte("{}\nnope\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	ref := harness.SessionRef{ConfigDir: cfg, Cwd: cwd, SessionID: hardenedID}
	for _, cut := range []bool{false, true} {
		var buf bytes.Buffer
		if _, err := (claude.Adapter{}).Export(t.Context(), ref, harness.ExportOptions{CutAtToolUse: cut}, &buf); err == nil {
			t.Errorf("cut=%v: a line that is not JSON was exported", cut)
		}
	}
}

// CODE-10: an empty ConfigDir is the one Claude Code would use.
func TestAnEmptyConfigDirFollowsClaudeConfigDir(t *testing.T) {
	cfg := t.TempDir()
	t.Setenv(claude.ConfigHomeEnv, cfg)
	res, err := claude.Adapter{}.Import(t.Context(), harness.SessionImport{Cwd: "/home/x/repo"}, bytes.NewReader(wellFormed(t)))
	if err != nil || !strings.HasPrefix(res.Transcript, cfg) {
		t.Fatalf("res = %+v, err = %v", res, err)
	}
}
