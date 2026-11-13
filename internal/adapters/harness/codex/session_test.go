package codex_test

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/Autarquico/codefabric/internal/adapters/harness"
	"github.com/Autarquico/codefabric/internal/adapters/harness/codex"
	"github.com/Autarquico/codefabric/internal/domain"
)

// The fixture is a rollout with the shape of codex-cli 0.167.0 (spike F1, case
// C1), written by hand and redacted: an id and cwd that are not anyone's. It
// has two tool calls, the last of them a function_call, and two turn_context.
const (
	fixtureID  = "01a1b2c3-d4e5-7f60-8a7b-9c0d1e2f3a4b"
	fixtureCwd = "/home/u/repo"
	newID      = "01a1ffff-0000-7000-8000-00000000abcd"
	otherID    = "01a1eeee-0000-7000-9000-000000000001"
	fixtureDir = "testdata/session/config"
	rolloutRel = "sessions/2026/11/12/rollout-2026-11-12T09-30-15-" + fixtureID + ".jsonl"
	// cutLines is the lines up to the function_call, the last tool call.
	cutLines = 8
)

func fixtureRef() harness.SessionRef {
	return harness.SessionRef{ConfigDir: fixtureDir, Cwd: fixtureCwd, SessionID: fixtureID}
}

func fixtureLines(t *testing.T) []string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(fixtureDir, rolloutRel)) //nolint:gosec // testdata
	if err != nil {
		t.Fatal(err)
	}
	return strings.SplitAfter(strings.TrimSuffix(string(b), "\n"), "\n")
}

func export(t *testing.T, opts harness.ExportOptions) ([]byte, harness.SessionManifest) {
	t.Helper()
	var buf bytes.Buffer
	m, err := codex.Adapter{}.Export(t.Context(), fixtureRef(), opts, &buf)
	if err != nil {
		t.Fatal(err)
	}
	return buf.Bytes(), m
}

func untar(t *testing.T, archive []byte) (names []string, content map[string]string) {
	t.Helper()
	gz, err := gzip.NewReader(bytes.NewReader(archive))
	if err != nil {
		t.Fatal(err)
	}
	tr := tar.NewReader(gz)
	content = map[string]string{}
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return names, content
		}
		if err != nil {
			t.Fatal(err)
		}
		b, err := io.ReadAll(tr)
		if err != nil {
			t.Fatal(err)
		}
		names = append(names, hdr.Name)
		content[hdr.Name] = string(b)
	}
}

func tarOf(t *testing.T, manifest harness.SessionManifest, transcript string) []byte {
	t.Helper()
	var buf bytes.Buffer
	if err := harness.WriteSessionArchive(&buf, manifest, []byte(transcript), nil); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func TestLocateFindsTheRolloutByItsId(t *testing.T) {
	files, err := codex.Adapter{}.Locate(t.Context(), fixtureRef())
	if err != nil {
		t.Fatal(err)
	}
	if want := filepath.Join(fixtureDir, rolloutRel); files.Transcript != want || len(files.Attachments) != 0 {
		t.Errorf("files = %+v, want %s", files, want)
	}
	ref := fixtureRef()
	ref.SessionID = otherID
	if _, err := (codex.Adapter{}).Locate(t.Context(), ref); !errors.Is(err, harness.ErrSessionNotFound) {
		t.Errorf("another id: %v", err)
	}
	ref.ConfigDir = t.TempDir()
	if _, err := (codex.Adapter{}).Locate(t.Context(), ref); !errors.Is(err, harness.ErrSessionNotFound) {
		t.Errorf("a CODEX_HOME with no sessions: %v", err)
	}
}

func TestSessionIdsAreUUIDv7InLowercase(t *testing.T) {
	for _, id := range []string{
		"", "../../etc/passwd", "-" + fixtureID[1:], strings.ToUpper(fixtureID),
		"11111111-2222-4333-8444-555555555555", // a v4: Codex names its threads with v7
		fixtureID + "x", "01a1b2c3-d4e5-7f60-0a7b-9c0d1e2f3a4b",
	} {
		ref := fixtureRef()
		ref.SessionID = id
		if _, err := (codex.Adapter{}).Locate(t.Context(), ref); err == nil || errors.Is(err, harness.ErrSessionNotFound) {
			t.Errorf("Locate(%q): %v, want a refusal", id, err)
		}
		in := harness.SessionImport{ConfigDir: t.TempDir(), Cwd: "/wt", NewSessionID: id}
		if _, err := (codex.Adapter{}).Import(t.Context(), in, bytes.NewReader(nil)); err == nil {
			t.Errorf("Import accepted the new id %q", id)
		}
	}
}

func TestExportIsDeterministicAndHoldsTheRolloutAsItIs(t *testing.T) {
	a, m := export(t, harness.ExportOptions{})
	b, _ := export(t, harness.ExportOptions{})
	if !bytes.Equal(a, b) {
		t.Error("the same session exported to different bytes")
	}
	names, content := untar(t, a)
	if !slices.Equal(names, []string{"manifest.json", fixtureID + ".jsonl"}) {
		t.Errorf("entries = %v", names)
	}
	if got, want := content[fixtureID+".jsonl"], strings.Join(fixtureLines(t), "")+"\n"; got != want {
		t.Errorf("the transcript is not the rollout:\n%s", got)
	}
	want := harness.SessionManifest{Adapter: "codex", Version: "0.167.0", SessionID: fixtureID, Cwd: fixtureCwd, Lines: 12}
	if m.Adapter != want.Adapter || m.Version != want.Version || m.SessionID != want.SessionID || m.Cwd != want.Cwd || m.Lines != want.Lines || m.Cut {
		t.Errorf("manifest = %+v, want %+v", m, want)
	}
	var inArchive harness.SessionManifest
	if err := json.Unmarshal([]byte(content["manifest.json"]), &inArchive); err != nil || inArchive.SessionID != fixtureID {
		t.Errorf("manifest.json = %s, %v", content["manifest.json"], err)
	}
}

// running is a rollout whose last call has no output yet, with a line Codex
// writes after it while it runs: the shape of the session of $handto once the
// call of cf handoff is on disk.
func running(t *testing.T) harness.SessionRef {
	t.Helper()
	dir := t.TempDir()
	body := strings.Join(fixtureLines(t)[:cutLines], "") +
		`{"timestamp":"2026-11-12T09:30:15.800Z","ordinal":9,"type":"event_msg","payload":{"type":"exec_command_begin","call_id":"call_2"}}` + "\n"
	writeRollout(t, dir, "2026/11/12", fixtureID, body)
	ref := fixtureRef()
	ref.ConfigDir = dir
	return ref
}

func TestExportCutsAtTheCallWithNoOutput(t *testing.T) {
	var buf bytes.Buffer
	m, err := codex.Adapter{}.Export(t.Context(), running(t), harness.ExportOptions{CutAtToolUse: true}, &buf)
	if err != nil {
		t.Fatal(err)
	}
	_, content := untar(t, buf.Bytes())
	want := strings.Join(fixtureLines(t)[:cutLines], "")
	if got := content[fixtureID+".jsonl"]; got != want {
		t.Errorf("cut transcript:\n%s\nwant:\n%s", got, want)
	}
	if !strings.HasSuffix(strings.TrimSpace(want), `"name":"shell","arguments":"{\"command\":[\"go\",\"test\"]}"}}`) {
		t.Fatalf("the cut does not end at the function_call:\n%s", want)
	}
	if !m.Cut || m.Lines != cutLines {
		t.Errorf("manifest = %+v", m)
	}
}

// Every call has its output: the call of $handto is not on disk yet, and the session goes whole.
func TestExportForTheCallerKeepsAnAnsweredSessionWhole(t *testing.T) {
	_, m := export(t, harness.ExportOptions{CutAtToolUse: true})
	if m.Cut || m.Lines != len(fixtureLines(t)) {
		t.Errorf("manifest = %+v", m)
	}
}

func TestExportWithNoToolCallGoesWhole(t *testing.T) {
	dir := t.TempDir()
	lines := fixtureLines(t)
	plain := strings.Join([]string{lines[0], lines[1], lines[2], lines[10]}, "")
	writeRollout(t, dir, "2026/11/12", fixtureID, plain)
	ref := fixtureRef()
	ref.ConfigDir = dir
	m, err := (codex.Adapter{}).Export(t.Context(), ref, harness.ExportOptions{CutAtToolUse: true}, io.Discard)
	if err != nil || m.Cut || m.Lines != 4 {
		t.Errorf("a session with no tool call: %+v %v", m, err)
	}
}

func TestExportToleratesAnIncompleteLastLineButNotOtherGarbage(t *testing.T) {
	dir := t.TempDir()
	whole := strings.Join(fixtureLines(t), "") + "\n"
	ref := fixtureRef()
	ref.ConfigDir = dir

	writeRollout(t, dir, "2026/11/12", fixtureID, whole+`{"timestamp":"2026-11-12T09:31:00Z","ordinal":13,"type":"event_`)
	var buf bytes.Buffer
	m, err := codex.Adapter{}.Export(t.Context(), ref, harness.ExportOptions{}, &buf)
	if err != nil || m.Lines != 12 || len(m.Warnings) != 1 {
		t.Errorf("a live tail: %+v, %v", m, err)
	}
	if _, err := (codex.Adapter{}).Export(t.Context(), ref, harness.ExportOptions{CutAtToolUse: true}, io.Discard); err != nil {
		t.Errorf("a live tail after the cut: %v", err)
	}

	lines := fixtureLines(t)
	writeRollout(t, dir, "2026/11/12", fixtureID, lines[0]+"garbage\n"+strings.Join(lines[1:], "")+"\n")
	if _, err := (codex.Adapter{}).Export(t.Context(), ref, harness.ExportOptions{}, io.Discard); err == nil {
		t.Error("garbage in the middle was exported")
	}
}

func TestExportRefusesARolloutThatIsNotOfThatSession(t *testing.T) {
	dir := t.TempDir()
	lines := fixtureLines(t)
	writeRollout(t, dir, "2026/11/12", fixtureID, strings.Replace(lines[0], `"id":"`+fixtureID, `"id":"`+otherID, 1)+strings.Join(lines[1:], "")+"\n")
	ref := fixtureRef()
	ref.ConfigDir = dir
	if _, err := (codex.Adapter{}).Export(t.Context(), ref, harness.ExportOptions{}, io.Discard); err == nil {
		t.Error("a rollout that names another session was exported")
	}
	writeRollout(t, dir, "2026/11/12", fixtureID, strings.Join(lines[1:], "")+"\n")
	if _, err := (codex.Adapter{}).Export(t.Context(), ref, harness.ExportOptions{}, io.Discard); err == nil {
		t.Error("a rollout with no session_meta first was exported")
	}
}

func TestExportLimit(t *testing.T) {
	_, err := codex.Adapter{}.Export(t.Context(), fixtureRef(), harness.ExportOptions{SessionLimit: harness.SessionLimit{MaxBytes: 100}}, io.Discard)
	if !errors.Is(err, harness.ErrSessionTooLarge) {
		t.Errorf("err = %v", err)
	}
}

func importFixture(t *testing.T, in harness.SessionImport) (harness.SessionImportResult, error) {
	t.Helper()
	archive, _ := export(t, harness.ExportOptions{})
	return codex.Adapter{}.Import(t.Context(), in, bytes.NewReader(archive))
}

// Only payload.cwd of session_meta and turn_context changes: every other byte
// of every line is the one Codex wrote.
func TestImportSetsTheCwdOfTheMetaLinesAndNothingElse(t *testing.T) {
	dest := t.TempDir()
	res, err := importFixture(t, harness.SessionImport{ConfigDir: dest, Cwd: "/srv/wt"})
	if err != nil {
		t.Fatal(err)
	}
	want := filepath.Join(dest, rolloutRel)
	if res.SessionID != fixtureID || res.Transcript != want || res.Manifest.Cwd != "/srv/wt" {
		t.Errorf("result = %+v, want %s", res, want)
	}
	got, err := os.ReadFile(want) //nolint:gosec // a file of the test
	if err != nil {
		t.Fatal(err)
	}
	original := strings.Join(fixtureLines(t), "") + "\n"
	rewrittenCwd := strings.ReplaceAll(original, `"cwd":"`+fixtureCwd+`"`, `"cwd":"/srv/wt"`)
	if string(got) != rewrittenCwd {
		t.Errorf("the rollout differs from the original by more than the cwd:\n%s", got)
	}
	if n := strings.Count(string(got), "/srv/wt"); n != 3 {
		t.Errorf("%d cwd rewritten, want 3 (session_meta and two turn_context)", n)
	}
	if !strings.Contains(string(got), `"runtime_workspace_roots":["`+fixtureCwd+`"]`) {
		t.Error("a field that is not payload.cwd was rewritten")
	}
	if info, err := os.Stat(want); err != nil || info.Mode().Perm() != 0o600 {
		t.Errorf("mode of the rollout: %v, %v", info, err)
	}
	if left, _ := filepath.Glob(filepath.Join(dest, "sessions/2026/11/12/.cf-import-*")); len(left) != 0 {
		t.Errorf("left over: %v", left)
	}
}

func TestImportEscapesTheCwdAsJSON(t *testing.T) {
	dest := t.TempDir()
	cwd := `/tmp/a "b" <c> & ü`
	res, err := importFixture(t, harness.SessionImport{ConfigDir: dest, Cwd: cwd})
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := os.ReadFile(res.Transcript) //nolint:gosec // a file of the test
	first, _, _ := strings.Cut(string(raw), "\n")
	var meta struct {
		Payload struct{ Cwd string }
	}
	if err := json.Unmarshal([]byte(first), &meta); err != nil || meta.Payload.Cwd != cwd {
		t.Errorf("cwd = %q, %v", meta.Payload.Cwd, err)
	}
	if !strings.Contains(first, `<c> & ü`) {
		t.Errorf("the cwd was HTML-escaped: %s", first)
	}
}

func TestImportWithAnotherIdRenamesTheSession(t *testing.T) {
	dest := t.TempDir()
	res, err := importFixture(t, harness.SessionImport{ConfigDir: dest, Cwd: "/wt", NewSessionID: newID})
	if err != nil {
		t.Fatal(err)
	}
	want := filepath.Join(dest, "sessions/2026/11/12/rollout-2026-11-12T09-30-15-"+newID+".jsonl")
	if res.SessionID != newID || res.Transcript != want || res.Manifest.SessionID != newID {
		t.Fatalf("result = %+v, want %s", res, want)
	}
	raw, _ := os.ReadFile(want) //nolint:gosec // a file of the test
	lines := strings.SplitAfter(string(raw), "\n")
	meta := lines[0]
	if !strings.Contains(meta, `"id":"`+newID+`"`) || !strings.Contains(meta, `"session_id":"`+newID+`"`) || strings.Contains(meta, fixtureID) {
		t.Errorf("session_meta = %s", meta)
	}
	original := fixtureLines(t)
	if got := strings.Join(lines[1:], ""); strings.Contains(got, newID) {
		t.Errorf("the id reached a line that is not the session_meta: %s", got)
	}
	if len(lines)-1 != len(original) { // the last is empty after the final newline
		t.Errorf("%d lines, want %d", len(lines)-1, len(original))
	}
	if _, err := (codex.Adapter{}).Locate(t.Context(), harness.SessionRef{ConfigDir: dest, SessionID: newID}); err != nil {
		t.Errorf("Locate of the new id: %v", err)
	}
	if _, err := (codex.Adapter{}).Locate(t.Context(), harness.SessionRef{ConfigDir: dest, SessionID: fixtureID}); !errors.Is(err, harness.ErrSessionNotFound) {
		t.Errorf("Locate of the old id: %v", err)
	}
}

func TestImportDoesNotOverwriteUnlessAsked(t *testing.T) {
	dest := t.TempDir()
	in := harness.SessionImport{ConfigDir: dest, Cwd: "/one"}
	if _, err := importFixture(t, in); err != nil {
		t.Fatal(err)
	}
	in.Cwd = "/two"
	res, err := importFixture(t, in)
	if !errors.Is(err, harness.ErrSessionExists) || res.SessionID != fixtureID {
		t.Fatalf("second import: %+v, %v", res, err)
	}
	if raw, _ := os.ReadFile(filepath.Join(dest, rolloutRel)); !strings.Contains(string(raw), "/one") || strings.Contains(string(raw), "/two") { //nolint:gosec // a file of the test
		t.Error("a refused Import changed the session")
	}
	in.Overwrite = true
	if _, err := importFixture(t, in); err != nil {
		t.Fatal(err)
	}
	if raw, _ := os.ReadFile(filepath.Join(dest, rolloutRel)); !strings.Contains(string(raw), "/two") { //nolint:gosec // a file of the test
		t.Error("Overwrite did not replace the session")
	}
}

// Overwrite replaces the session even if its rollout is under another date.
func TestImportOverwriteRemovesTheRolloutOfAnotherDay(t *testing.T) {
	dest := t.TempDir()
	writeRollout(t, dest, "2026/01/02", fixtureID, "stale\n")
	if _, err := importFixture(t, harness.SessionImport{ConfigDir: dest, Cwd: "/wt", Overwrite: true}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dest, "sessions/2026/01/02/rollout-2026-01-02T00-00-00-"+fixtureID+".jsonl")); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("the stale rollout is still there: %v", err)
	}
}

func TestImportRefusesWhatExportDoesNotWrite(t *testing.T) {
	lines := fixtureLines(t)
	whole := strings.Join(lines, "") + "\n"
	manifest := harness.SessionManifest{Adapter: "codex", SessionID: fixtureID}
	other := manifest
	other.Adapter = "claude-code"
	badID := manifest
	badID.SessionID = "../x"
	mismatch := manifest
	mismatch.SessionID = otherID
	cases := map[string][]byte{
		"not an archive":       []byte("nope"),
		"another adapter":      tarOf(t, other, whole),
		"an id that climbs":    tarOf(t, badID, whole),
		"manifest and rollout": tarOf(t, mismatch, whole),
		"no session_meta":      tarOf(t, manifest, strings.Join(lines[1:], "")+"\n"),
		"garbage line":         tarOf(t, manifest, lines[0]+"garbage\n"),
	}
	for name, archive := range cases {
		dest := t.TempDir()
		_, err := codex.Adapter{}.Import(t.Context(), harness.SessionImport{ConfigDir: dest, Cwd: "/wt"}, bytes.NewReader(archive))
		if err == nil {
			t.Errorf("%s: no error", name)
		}
		if entries, _ := os.ReadDir(dest); len(entries) != 0 {
			t.Errorf("%s: left %d entries in the CODEX_HOME", name, len(entries))
		}
	}
	dest := t.TempDir()
	archive, _ := export(t, harness.ExportOptions{})
	in := harness.SessionImport{ConfigDir: dest, Cwd: "/wt", SessionLimit: harness.SessionLimit{MaxBytes: 100}}
	if _, err := (codex.Adapter{}).Import(t.Context(), in, bytes.NewReader(archive)); !errors.Is(err, harness.ErrSessionTooLarge) {
		t.Errorf("limit: %v", err)
	}
}

func TestCutThenImportThenExportIsTheCut(t *testing.T) {
	var out bytes.Buffer
	if _, err := (codex.Adapter{}).Export(t.Context(), running(t), harness.ExportOptions{CutAtToolUse: true}, &out); err != nil {
		t.Fatal(err)
	}
	cut := out.Bytes()
	dest := t.TempDir()
	if _, err := (codex.Adapter{}).Import(t.Context(), harness.SessionImport{ConfigDir: dest, Cwd: fixtureCwd}, bytes.NewReader(cut)); err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	ref := harness.SessionRef{ConfigDir: dest, SessionID: fixtureID}
	if _, err := (codex.Adapter{}).Export(t.Context(), ref, harness.ExportOptions{}, &buf); err != nil {
		t.Fatal(err)
	}
	_, content := untar(t, buf.Bytes())
	if got, want := content[fixtureID+".jsonl"], strings.Join(fixtureLines(t)[:cutLines], ""); got != want {
		t.Errorf("round trip:\n%s", got)
	}
}

// writeRollout writes a rollout of the session under <dir>/sessions/<day>/.
func writeRollout(t *testing.T, dir, day, id, body string) {
	t.Helper()
	d := filepath.Join(dir, "sessions", filepath.FromSlash(day))
	if err := os.MkdirAll(d, 0o750); err != nil {
		t.Fatal(err)
	}
	name := "rollout-" + strings.NewReplacer("/", "-").Replace(day) + "T00-00-00-" + id + ".jsonl"
	if err := os.WriteFile(filepath.Join(d, name), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

// --- Checkpoint and Resume ---

func runWithSession(t *testing.T, mode domain.Mode) (harness.Handle, harness.RunSpec) {
	t.Helper()
	root := t.TempDir()
	run := spec(mode)
	run.ConfigDir, run.Worktree = filepath.Join(root, "config"), filepath.Join(root, "wt")
	body := strings.Join(fixtureLines(t), "") + "\n"
	writeRolloutAt(t, run.ConfigDir, body)
	return harness.Handle{RunID: run.RunID, LeaseEpoch: run.LeaseEpoch, Mode: mode, ConfigDir: run.ConfigDir, Worktree: run.Worktree}, run
}

// writeRolloutAt puts the fixture where Codex would have written it.
func writeRolloutAt(t *testing.T, configDir, body string) {
	t.Helper()
	dir := filepath.Join(configDir, "sessions", "2026", "11", "12")
	if err := os.MkdirAll(dir, 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "rollout-2026-11-12T09-30-15-"+fixtureID+".jsonl"), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestCheckpointOfARunWithoutSessionIdFindsItsOnlyRollout(t *testing.T) {
	h, _ := runWithSession(t, domain.ModeHeadless)
	cp, err := codex.Adapter{}.Checkpoint(t.Context(), nil, h)
	if err != nil {
		t.Fatal(err)
	}
	if cp.Harness != codex.ID || cp.Format != codex.CheckpointFormat || len(cp.Data) == 0 {
		t.Fatalf("checkpoint = %+v", cp)
	}
	again, _ := codex.Adapter{}.Checkpoint(t.Context(), nil, h)
	if !bytes.Equal(cp.Data, again.Data) {
		t.Error("two checkpoints of the same session differ")
	}
	h.SessionID = fixtureID
	withID, err := codex.Adapter{}.Checkpoint(t.Context(), nil, h)
	if err != nil || !bytes.Equal(withID.Data, cp.Data) {
		t.Errorf("with the id of the Handle: %v", err)
	}
}

func TestCheckpointRefusals(t *testing.T) {
	h, _ := runWithSession(t, domain.ModeHeadless)
	h.SessionID = otherID
	if _, err := (codex.Adapter{}).Checkpoint(t.Context(), nil, h); !errors.Is(err, harness.ErrSessionNotFound) {
		t.Errorf("a Handle whose session is not there: %v", err)
	}
	h.SessionID = ""
	writeRollout(t, h.ConfigDir, "2026/11/13", otherID, "x\n")
	if _, err := (codex.Adapter{}).Checkpoint(t.Context(), nil, h); err == nil {
		t.Error("two sessions and no id in the Handle: no error")
	}
	h.ConfigDir = ""
	if _, err := (codex.Adapter{}).Checkpoint(t.Context(), nil, h); err == nil {
		t.Error("no config dir: no error")
	}
	h.ConfigDir = t.TempDir()
	if _, err := (codex.Adapter{}).Checkpoint(t.Context(), nil, h); !errors.Is(err, harness.ErrSessionNotFound) {
		t.Errorf("no sessions: %v", err)
	}
}

func checkpointOf(t *testing.T, mode domain.Mode) harness.Checkpoint {
	t.Helper()
	h, _ := runWithSession(t, mode)
	cp, err := codex.Adapter{}.Checkpoint(t.Context(), nil, h)
	if err != nil {
		t.Fatal(err)
	}
	return cp
}

func TestResumeHeadlessInstallsTheSessionAndResumesIt(t *testing.T) {
	cp := checkpointOf(t, domain.ModeHeadless)
	host := startHost(t)
	run := spec(domain.ModeHeadless)
	run.ConfigDir, run.Worktree, run.Prompt, run.Model = t.TempDir(), "/srv/wt", "keep going", "gpt-x"
	l, err := codex.Adapter{}.Resume(t.Context(), host, run, cp)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{
		codexPath(t, host), "exec", "--json", "--sandbox", "workspace-write", "-c", `approval_policy="never"`,
		"--skip-git-repo-check", "-C", "/srv/wt", "resume", "--json", "-c", `approval_policy="never"`,
		"--skip-git-repo-check", "-m", "gpt-x", fixtureID, "--", "keep going",
	}
	if !slices.Equal(l.Argv, want) {
		t.Errorf("argv = %v\nwant   %v", l.Argv, want)
	}
	if l.SessionID != fixtureID || !l.StructuredStdout || !slices.Equal(l.Env, []string{"CODEX_HOME=" + run.ConfigDir}) {
		t.Errorf("launch = %+v", l)
	}
	files, err := codex.Adapter{}.Locate(t.Context(), harness.SessionRef{ConfigDir: run.ConfigDir, SessionID: fixtureID})
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := os.ReadFile(files.Transcript) //nolint:gosec // a file of the test
	if !strings.Contains(string(raw), `"cwd":"/srv/wt"`) || strings.Contains(string(raw), `"cwd":"`+fixtureCwd) {
		t.Errorf("the session was not moved to the cwd of the Run:\n%s", raw)
	}
}

func TestResumeInteractive(t *testing.T) {
	cp := checkpointOf(t, domain.ModeInteractive)
	host := startHost(t)
	run := spec(domain.ModeInteractive)
	run.ConfigDir = t.TempDir()
	run.Prompt = ""
	l, err := codex.Adapter{}.Resume(t.Context(), host, run, cp)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{codexPath(t, host), "resume", "--sandbox", "workspace-write", "-a", "never", "-C", "/wt", fixtureID}
	if !slices.Equal(l.Argv, want) || l.StructuredStdout || l.SessionID != fixtureID {
		t.Errorf("launch = %+v\nwant argv %v", l, want)
	}
	run.ConfigDir, run.Prompt = t.TempDir(), "and now this"
	l, err = codex.Adapter{}.Resume(t.Context(), host, run, cp)
	if err != nil || !slices.Equal(l.Argv, append(want, "--", "and now this")) {
		t.Errorf("with a prompt: %v, %v", l.Argv, err)
	}
}

func TestResumePromptThatLooksLikeAnOptionStaysAfterTheSeparator(t *testing.T) {
	run := spec(domain.ModeHeadless)
	run.ConfigDir, run.Prompt = t.TempDir(), "--dangerously-bypass-approvals-and-sandbox"
	l, err := codex.Adapter{}.Resume(t.Context(), startHost(t), run, checkpointOf(t, domain.ModeHeadless))
	if err != nil {
		t.Fatal(err)
	}
	n := len(l.Argv)
	if l.Argv[n-3] != fixtureID || l.Argv[n-2] != "--" || l.Argv[n-1] != run.Prompt {
		t.Errorf("argv = %v", l.Argv)
	}
}

// A Resume repeated in the same CODEX_HOME resumes the session that is there
// and does not ask its prompt twice.
func TestResumeIsIdempotentAndDoesNotRepeatItsPrompt(t *testing.T) {
	cp := checkpointOf(t, domain.ModeHeadless)
	host := startHost(t)
	run := spec(domain.ModeHeadless)
	run.ConfigDir, run.Prompt = t.TempDir(), "keep going"
	first, err := codex.Adapter{}.Resume(t.Context(), host, run, cp)
	if err != nil {
		t.Fatal(err)
	}
	again, err := codex.Adapter{}.Resume(t.Context(), host, run, cp)
	if err != nil || !slices.Equal(first.Argv, again.Argv) {
		t.Fatalf("the repeat: %v, %v", again.Argv, err)
	}
	// The process ran and Codex appended the prompt to the rollout.
	files, _ := codex.Adapter{}.Locate(t.Context(), harness.SessionRef{ConfigDir: run.ConfigDir, SessionID: fixtureID})
	f, err := os.OpenFile(files.Transcript, os.O_APPEND|os.O_WRONLY, 0) //nolint:gosec // a file of the test
	if err != nil {
		t.Fatal(err)
	}
	_, _ = f.WriteString(`{"type":"response_item","payload":{"type":"message","role":"user","content":[{"type":"input_text","text":"keep going"}]}}` + "\n")
	_ = f.Close()

	headless, err := codex.Adapter{}.Resume(t.Context(), host, run, cp)
	if err != nil || headless.Argv[len(headless.Argv)-1] != harness.ContinuePrompt || headless.Argv[len(headless.Argv)-2] != "--" {
		t.Errorf("headless: %v, %v", headless.Argv, err)
	}
	run.Mode = domain.ModeInteractive
	inter, err := codex.Adapter{}.Resume(t.Context(), host, run, cp)
	if err != nil || inter.Argv[len(inter.Argv)-1] != fixtureID {
		t.Errorf("interactive: %v, %v", inter.Argv, err)
	}
}

func TestResumeFromTwoCheckpointsLaunchesTwoDifferentResumes(t *testing.T) {
	cp := checkpointOf(t, domain.ModeHeadless)
	other := harness.Checkpoint{Harness: codex.ID, Format: codex.CheckpointFormat, Data: otherCheckpointData(t)}
	host := startHost(t)
	run := spec(domain.ModeHeadless)
	run.ConfigDir = t.TempDir()
	a, err := codex.Adapter{}.Resume(t.Context(), host, run, cp)
	if err != nil {
		t.Fatal(err)
	}
	run.ConfigDir = t.TempDir()
	b, err := codex.Adapter{}.Resume(t.Context(), host, run, other)
	if err != nil || slices.Equal(a.Argv, b.Argv) || b.SessionID != otherID {
		t.Errorf("second resume: %+v, %v", b, err)
	}
}

// otherCheckpointData is the checkpoint of a session with another id.
func otherCheckpointData(t *testing.T) []byte {
	t.Helper()
	whole := strings.Join(fixtureLines(t), "") + "\n"
	renamed := strings.ReplaceAll(whole, fixtureID, otherID)
	return tarOf(t, harness.SessionManifest{Adapter: codex.ID, SessionID: otherID}, renamed)
}

func TestResumeRefusals(t *testing.T) {
	cp := checkpointOf(t, domain.ModeHeadless)
	host := startHost(t)
	good := func() harness.RunSpec {
		r := spec(domain.ModeHeadless)
		r.ConfigDir = t.TempDir()
		return r
	}
	cases := map[string]struct {
		mutate func(*harness.RunSpec)
		cp     harness.Checkpoint
	}{
		"no prompt headless":    {func(r *harness.RunSpec) { r.Prompt = "" }, cp},
		"no config dir":         {func(r *harness.RunSpec) { r.ConfigDir = "" }, cp},
		"model like an option":  {func(r *harness.RunSpec) { r.Model = "-x" }, cp},
		"unknown mode":          {func(r *harness.RunSpec) { r.Mode = "batch" }, cp},
		"another harness":       {func(*harness.RunSpec) {}, harness.Checkpoint{Harness: "claude-code", Format: cp.Format, Data: cp.Data}},
		"another format":        {func(*harness.RunSpec) {}, harness.Checkpoint{Harness: cp.Harness, Format: "x/v9", Data: cp.Data}},
		"data that is no tar":   {func(*harness.RunSpec) {}, harness.Checkpoint{Harness: cp.Harness, Format: cp.Format, Data: []byte("nope")}},
		"the binary is missing": {func(*harness.RunSpec) {}, cp},
	}
	for name, c := range cases {
		run := good()
		c.mutate(&run)
		h := host
		if name == "the binary is missing" {
			h = hostWith(t, "")
		}
		if _, err := (codex.Adapter{}).Resume(t.Context(), h, run, c.cp); err == nil {
			t.Errorf("%s: no error", name)
		}
	}
}

func rolloutBody(t *testing.T) string {
	t.Helper()
	return strings.Join(fixtureLines(t), "") + "\n"
}
