package claude

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/Autarquico/codefabric/internal/adapters/harness"
)

const (
	testSessionID = "11111111-2222-4333-8444-555555555555"
	newSessionID  = "aaaaaaaa-bbbb-4ccc-8ddd-eeeeeeeeeeee"
	sessionData   = "testdata/session"
)

func sessionRef() harness.SessionRef {
	return harness.SessionRef{ConfigDir: filepath.Join(sessionData, "config"), Cwd: "/Users/x/repo", SessionID: testSessionID}
}

func readFile(p string) ([]byte, error) {
	return os.ReadFile(p) //nolint:gosec // a test path
}

func readData(t *testing.T, parts ...string) string {
	t.Helper()
	b, err := readFile(filepath.Join(append([]string{sessionData}, parts...)...))
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func exportSession(t *testing.T, opts harness.ExportOptions) ([]byte, harness.SessionManifest) {
	t.Helper()
	var buf bytes.Buffer
	m, err := Adapter{}.Export(context.Background(), sessionRef(), opts, &buf)
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
		b, _ := io.ReadAll(tr)
		names = append(names, hdr.Name)
		content[hdr.Name] = string(b)
	}
}

func TestSession_LocateFindsTranscriptAndSubagents(t *testing.T) {
	files, err := Adapter{}.Locate(context.Background(), sessionRef())
	if err != nil {
		t.Fatal(err)
	}
	if filepath.Base(files.Transcript) != testSessionID+".jsonl" || len(files.Attachments) != 2 {
		t.Fatalf("files = %+v", files)
	}
	ref := sessionRef()
	ref.SessionID = "00000000-0000-4000-8000-000000000000"
	if _, err := (Adapter{}).Locate(context.Background(), ref); !errors.Is(err, harness.ErrSessionNotFound) {
		t.Fatalf("err = %v, want ErrSessionNotFound", err)
	}
	ref.SessionID = "../x"
	if _, err := (Adapter{}).Locate(context.Background(), ref); err == nil {
		t.Fatal("a session id that climbs was accepted")
	}
}

func TestSession_ExportIsDeterministicAndLeavesOutLocalState(t *testing.T) {
	first, m := exportSession(t, harness.ExportOptions{})
	second, _ := exportSession(t, harness.ExportOptions{})
	if !bytes.Equal(first, second) {
		t.Fatal("two exports of the same session differ")
	}
	names, content := untar(t, first)
	want := []string{
		"manifest.json",
		testSessionID + ".jsonl",
		testSessionID + "/subagents/agent-a.jsonl",
		testSessionID + "/subagents/agent-a.meta.json",
	}
	if len(names) != len(want) {
		t.Fatalf("entries = %v, want %v", names, want)
	}
	for i := range want {
		if names[i] != want[i] {
			t.Fatalf("entries = %v, want %v", names, want)
		}
	}
	if content["manifest.json"] != readData(t, "golden", "manifest.json") {
		t.Fatalf("manifest = %s", content["manifest.json"])
	}
	if content[testSessionID+".jsonl"] != readData(t, "config", "projects", "-Users-x-repo", testSessionID+".jsonl") {
		t.Fatal("the transcript changed")
	}
	if m.Lines != 5 || m.Cut {
		t.Fatalf("manifest = %+v", m)
	}
}

func TestSession_ExportCutsAfterTheLastToolUse(t *testing.T) {
	archive, m := exportSession(t, harness.ExportOptions{CutAtToolUse: true})
	_, content := untar(t, archive)
	if got := content[testSessionID+".jsonl"]; got != readData(t, "golden", "cut.jsonl") {
		t.Fatalf("transcript = %s", got)
	}
	if content["manifest.json"] != readData(t, "golden", "manifest-cut.json") || !m.Cut || m.Lines != 3 {
		t.Fatalf("manifest = %s", content["manifest.json"])
	}
}

func TestSession_ExportRefusesWhatIsTooLarge(t *testing.T) {
	_, err := Adapter{}.Export(context.Background(), sessionRef(), harness.ExportOptions{SessionLimit: harness.SessionLimit{MaxBytes: 100}}, io.Discard)
	if !errors.Is(err, harness.ErrSessionTooLarge) {
		t.Fatalf("err = %v, want ErrSessionTooLarge", err)
	}
}

func TestSession_ImportRewritesOnlyCwdAndSessionID(t *testing.T) {
	archive, _ := exportSession(t, harness.ExportOptions{})
	cfg := t.TempDir()
	res, err := Adapter{}.Import(context.Background(), harness.SessionImport{
		ConfigDir: cfg, Cwd: "/home/x/repo", NewSessionID: newSessionID,
	}, bytes.NewReader(archive))
	if err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(cfg, "projects", "-home-x-repo")
	got, err := readFile(filepath.Join(dir, newSessionID+".jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != readData(t, "golden", "imported.jsonl") || res.Transcript != filepath.Join(dir, newSessionID+".jsonl") {
		t.Fatalf("transcript = %s", got)
	}
	agent, err := readFile(filepath.Join(dir, newSessionID, "subagents", "agent-a.jsonl"))
	if err != nil || string(agent) != readData(t, "golden", "imported-agent-a.jsonl") {
		t.Fatalf("subagent = %s, %v", agent, err)
	}
	meta, _ := readFile(filepath.Join(dir, newSessionID, "subagents", "agent-a.meta.json"))
	if string(meta) != readData(t, "config", "projects", "-Users-x-repo", testSessionID, "subagents", "agent-a.meta.json") {
		t.Fatal("a file that is not a transcript changed")
	}
	if res.SessionID != newSessionID || res.Manifest.Cwd != "/home/x/repo" {
		t.Fatalf("result = %+v", res)
	}
	entries, _ := os.ReadDir(dir)
	if len(entries) != 2 {
		t.Fatalf("the destination has leftovers: %v", entries)
	}
}

func TestSession_ImportKeepsTheIDWhenNotAskedForAnother(t *testing.T) {
	archive, _ := exportSession(t, harness.ExportOptions{})
	cfg := t.TempDir()
	res, err := Adapter{}.Import(context.Background(), harness.SessionImport{ConfigDir: cfg, Cwd: "/home/x/repo"}, bytes.NewReader(archive))
	if err != nil || res.SessionID != testSessionID {
		t.Fatalf("res = %+v, err = %v", res, err)
	}
	got, _ := readFile(res.Transcript)
	if bytes.Contains(got, []byte("/Users/x/repo\"")) || !bytes.Contains(got, []byte(testSessionID)) {
		t.Fatalf("transcript = %s", got)
	}
}

func TestSession_ImportDoesNotOverwriteASession(t *testing.T) {
	archive, _ := exportSession(t, harness.ExportOptions{})
	cfg := t.TempDir()
	in := harness.SessionImport{ConfigDir: cfg, Cwd: "/home/x/repo"}
	if _, err := (Adapter{}).Import(context.Background(), in, bytes.NewReader(archive)); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(cfg, "projects", "-home-x-repo", testSessionID+".jsonl")
	if err := os.WriteFile(path, []byte("mine\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := (Adapter{}).Import(context.Background(), in, bytes.NewReader(archive)); !errors.Is(err, harness.ErrSessionExists) {
		t.Fatalf("err = %v, want ErrSessionExists", err)
	}
	if b, _ := readFile(path); string(b) != "mine\n" {
		t.Fatalf("the session was changed: %s", b)
	}
	in.Overwrite = true
	if _, err := (Adapter{}).Import(context.Background(), in, bytes.NewReader(archive)); err != nil {
		t.Fatal(err)
	}
	if b, _ := readFile(path); string(b) == "mine\n" {
		t.Fatal("Overwrite did not replace the session")
	}
}

func TestSession_ImportRefusesALineThatIsNotJSON(t *testing.T) {
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	put := func(name, data string) {
		if err := tw.WriteHeader(&tar.Header{Name: name, Mode: 0o644, Size: int64(len(data))}); err != nil {
			t.Fatal(err)
		}
		_, _ = tw.Write([]byte(data))
	}
	put("manifest.json", `{"adapter":"claude-code","session_id":"`+testSessionID+`","cwd":"/a","lines":2}`)
	put(testSessionID+".jsonl", "{\"type\":\"user\",\"cwd\":\"/a\"}\nthis is not json\n")
	_ = tw.Close()
	_ = gz.Close()

	cfg := t.TempDir()
	_, err := Adapter{}.Import(context.Background(), harness.SessionImport{ConfigDir: cfg, Cwd: "/home/x/repo"}, &buf)
	if err == nil {
		t.Fatal("a line that is not JSON was imported")
	}
	if _, statErr := os.Stat(filepath.Join(cfg, "projects")); !os.IsNotExist(statErr) {
		t.Fatalf("the destination was touched: %v", statErr)
	}
}

func sessionTranscript(t *testing.T) []byte {
	t.Helper()
	return []byte(readData(t, "config", "projects", "-Users-x-repo", testSessionID+".jsonl"))
}

// CODE-14: the tail of a live or killed session.
func TestSession_ScanToleratesATailAfterTheCut(t *testing.T) {
	whole := sessionTranscript(t)
	wantCut, _, err := scanTranscript(whole, true)
	if err != nil {
		t.Fatal(err)
	}
	for name, tail := range map[string]string{"incomplete": `{"type":"user","cwd":"/Users/x/re`, "not JSON": "garbage\n"} {
		got, m, err := scanTranscript(append(bytes.Clone(whole), tail...), true)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if !bytes.Equal(got, wantCut) || !m.Cut || len(m.Warnings) != 0 {
			t.Errorf("%s: the cut changed: %s %+v", name, got, m)
		}
	}
}

func TestSession_ScanWithNoCutLeavesOutAnIncompleteLastLine(t *testing.T) {
	whole := sessionTranscript(t)
	want, wantManifest, err := scanTranscript(whole, false)
	if err != nil {
		t.Fatal(err)
	}
	got, m, err := scanTranscript(append(bytes.Clone(whole), `{"type":"user","cwd":"/Users/x/re`...), false)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, want) || m.Lines != wantManifest.Lines || len(m.Warnings) != 1 {
		t.Errorf("got %d lines, warnings %q", m.Lines, m.Warnings)
	}
}

func TestSession_ScanWithNoCutRefusesAnyOtherLineThatIsNotJSON(t *testing.T) {
	whole := sessionTranscript(t)
	for name, raw := range map[string][]byte{
		"complete last line": append(bytes.Clone(whole), "garbage\n"...),
		"middle line":        append(append(bytes.Clone(whole), "garbage\n"...), whole...),
	} {
		if _, _, err := scanTranscript(raw, false); err == nil {
			t.Errorf("%s: it was accepted", name)
		}
	}
	if _, _, err := scanTranscript(append([]byte("garbage\n"), whole...), true); err == nil {
		t.Error("a line that is not JSON before the cut was accepted")
	}
}

// CODE-17: the subagents of the session replaced are not the new one's.
func TestSession_ImportOverwriteRemovesSubagentsTheNewArchiveLacks(t *testing.T) {
	withSubagents, _ := exportSession(t, harness.ExportOptions{})
	cfg := t.TempDir()
	in := harness.SessionImport{ConfigDir: cfg, Cwd: "/home/x/repo", Overwrite: true}
	if _, err := (Adapter{}).Import(context.Background(), in, bytes.NewReader(withSubagents)); err != nil {
		t.Fatal(err)
	}
	subagents := filepath.Join(cfg, "projects", "-home-x-repo", testSessionID)
	if _, err := os.Stat(subagents); err != nil {
		t.Fatalf("the fixture has no subagents: %v", err)
	}
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for name, data := range map[string]string{
		"manifest.json":          `{"adapter":"claude-code","session_id":"` + testSessionID + `","cwd":"/a","lines":1}`,
		testSessionID + ".jsonl": "{\"type\":\"user\",\"cwd\":\"/a\"}\n",
	} {
		if err := tw.WriteHeader(&tar.Header{Name: name, Mode: 0o644, Size: int64(len(data))}); err != nil {
			t.Fatal(err)
		}
		_, _ = tw.Write([]byte(data))
	}
	_ = tw.Close()
	_ = gz.Close()
	if _, err := (Adapter{}).Import(context.Background(), in, &buf); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(subagents); !os.IsNotExist(err) {
		t.Fatalf("the old subagents are still there: %v", err)
	}
}

// CODE-16: ErrSessionExists says which session it is.
func TestSession_ImportTellsTheIDOfTheSessionThatExists(t *testing.T) {
	archive, _ := exportSession(t, harness.ExportOptions{})
	in := harness.SessionImport{ConfigDir: t.TempDir(), Cwd: "/home/x/repo"}
	if _, err := (Adapter{}).Import(context.Background(), in, bytes.NewReader(archive)); err != nil {
		t.Fatal(err)
	}
	res, err := (Adapter{}).Import(context.Background(), in, bytes.NewReader(archive))
	if !errors.Is(err, harness.ErrSessionExists) || res.SessionID != testSessionID {
		t.Fatalf("res = %+v, err = %v", res, err)
	}
}
