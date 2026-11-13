package session

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Autarquico/codefabric/internal/adapters/harness"
	"github.com/Autarquico/codefabric/internal/adapters/harness/claude"
	"github.com/Autarquico/codefabric/internal/adapters/harness/codex"
)

const (
	claudeID = "11111111-2222-4333-8444-555555555555"
	codexID  = "01a1b2c3-d4e5-7f60-8a7b-9c0d1e2f3a4b"
	workDir  = "/work/repo"
)

// machine is a HOME of synthetic sessions: a Local that looks in two temporary homes.
type machine struct {
	*Local
	claudeHome, codexHome string
}

func newMachine(t *testing.T) machine {
	t.Helper()
	m := machine{claudeHome: t.TempDir(), codexHome: t.TempDir()}
	m.Local = New(filepath.Join(t.TempDir(), "state"))
	m.ConfigDirs = map[string]string{claude.ID: m.claudeHome, codex.ID: m.codexHome}
	m.getenv = func(string) string { return "" }
	m.pidAlive = func(int) bool { return false }
	return m
}

func claudeLine(id, kind, content string) string {
	return fmt.Sprintf(`{"type":%q,"sessionId":%q,"cwd":%q,"version":"2.1.319","message":{"role":%q,"content":%s}}`,
		kind, id, workDir, kind, content)
}

// claudeBody is a conversation that ends in a tool call, its result and a closing text.
func claudeBody(id string, extra ...string) []string {
	lines := []string{
		claudeLine(id, "user", `"hi"`),
		claudeLine(id, "assistant", `[{"type":"tool_use","id":"toolu_1","name":"Bash","input":{"command":"cf handoff"}}]`),
		claudeLine(id, "user", `[{"type":"tool_result","tool_use_id":"toolu_1","content":"a"}]`),
		claudeLine(id, "assistant", `[{"type":"text","text":"done"}]`),
	}
	return append(lines, extra...)
}

// claudeInFlight is the tool call of cf handoff once Claude Code has written it: no result follows.
func claudeInFlight(id string) string {
	return claudeLine(id, "assistant", `[{"type":"tool_use","id":"toolu_2","name":"Bash","input":{"command":"cf handoff"}}]`)
}

func (m machine) writeClaude(t *testing.T, id string, lines []string) string {
	t.Helper()
	p := filepath.Join(m.claudeHome, "projects", "-work-repo", id+".jsonl")
	writeFile(t, p, strings.Join(lines, "\n")+"\n")
	return p
}

func (m machine) openClaude(t *testing.T, id string, pid int) {
	t.Helper()
	writeFile(t, filepath.Join(m.claudeHome, "sessions", fmt.Sprint(pid)+".json"), fmt.Sprintf(`{"pid":%d,"sessionId":%q}`, pid, id))
}

func codexLine(ordinal int, kind, payload string) string {
	return fmt.Sprintf(`{"timestamp":"2026-11-12T09:30:15.%03dZ","ordinal":%d,"type":%q,"payload":%s}`, ordinal, ordinal, kind, payload)
}

func codexBody(id string, extra ...string) []string {
	lines := []string{
		codexLine(1, "session_meta", fmt.Sprintf(`{"session_id":%q,"id":%q,"timestamp":"2026-11-12T09:30:15.120Z","cwd":%q,"cli_version":"0.167.0"}`, id, id, workDir)),
		codexLine(2, "response_item", `{"type":"message","role":"user","content":[{"type":"input_text","text":"fix"}]}`),
		codexLine(3, "response_item", `{"type":"function_call","call_id":"c1","name":"shell","arguments":"{}"}`),
		codexLine(4, "response_item", `{"type":"function_call_output","call_id":"c1","output":"ok"}`),
		codexLine(5, "response_item", `{"type":"message","role":"assistant","content":[{"type":"output_text","text":"done"}]}`),
	}
	return append(lines, extra...)
}

// codexInFlight is the tool call of $handto once Codex has written it: no output follows.
func codexInFlight() string {
	return codexLine(6, "response_item", `{"type":"function_call","call_id":"c2","name":"shell","arguments":"{}"}`)
}

// writeCodex writes the rollout and leaves it as quiet as a session that nobody has open.
func (m machine) writeCodex(t *testing.T, id string, lines []string) string {
	t.Helper()
	p := filepath.Join(m.codexHome, "sessions", "2026", "11", "12", "rollout-2026-11-12T09-30-15-"+id+".jsonl")
	writeFile(t, p, strings.Join(lines, "\n")+"\n")
	old := time.Now().Add(-time.Hour)
	if err := os.Chtimes(p, old, old); err != nil {
		t.Fatal(err)
	}
	return p
}

func writeFile(t *testing.T, p, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(p), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

func readFile(t *testing.T, p string) string {
	t.Helper()
	b, err := os.ReadFile(p) //nolint:gosec // a test path
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func export(t *testing.T, m machine, req ExportRequest) Export {
	t.Helper()
	e, err := m.Export(t.Context(), req)
	if err != nil {
		t.Fatal(err)
	}
	return e
}

func TestFindLooksInEveryHarnessAndSaysWhichHasIt(t *testing.T) {
	m := newMachine(t)
	m.writeClaude(t, claudeID, claudeBody(claudeID))
	m.writeCodex(t, codexID, codexBody(codexID))

	if ref, err := m.Find(t.Context(), claudeID, ""); err != nil || ref.Adapter != claude.ID || ref.Cwd != workDir {
		t.Fatalf("claude: %+v %v", ref, err)
	}
	if ref, err := m.Find(t.Context(), codexID, ""); err != nil || ref.Adapter != codex.ID || ref.Cwd != workDir {
		t.Fatalf("codex: %+v %v", ref, err)
	}
	if _, err := m.Find(t.Context(), "99999999-2222-4333-8444-555555555555", ""); !errors.Is(err, ErrNotFound) {
		t.Fatalf("a session nobody has: %v", err)
	}
	if _, err := m.Find(t.Context(), "../etc/passwd", ""); err == nil {
		t.Fatal("an id that is a path was accepted")
	}
}

func TestExportCutsAtTheCallersToolCallOnlyForTheCaller(t *testing.T) {
	m := newMachine(t)
	// Claude Code is the grandparent of this command; Codex tells its session through the environment.
	m.openClaude(t, claudeID, 4242)
	m.pidAlive = func(pid int) bool { return pid == 4242 }
	m.processes(map[int]int{100: 50, 50: 4242, 4242: 1}, 100)
	m.getenv = func(k string) string {
		if k == "CODEX_SESSION_ID" {
			return codexID
		}
		return ""
	}

	m.writeClaude(t, claudeID, claudeBody(claudeID, claudeInFlight(claudeID)))
	m.writeCodex(t, codexID, codexBody(codexID, codexInFlight()))
	for id, cutLines := range map[string]int{claudeID: 5, codexID: 6} {
		cut := export(t, m, ExportRequest{SessionID: id, Caller: true})
		if !cut.Manifest.Cut || cut.Manifest.Lines != cutLines {
			t.Errorf("%s: the cut should end at the call still running, line %d: %+v", id, cutLines, cut.Manifest)
		}
		if !strings.HasPrefix(cut.Digest, "sha256:") {
			t.Errorf("%s: digest %s", id, cut.Digest)
		}
	}

	// The harness has not written the caller's call yet: every earlier call has its
	// result, and cutting at one would lose it and the turns after it.
	m.writeClaude(t, claudeID, claudeBody(claudeID))
	m.writeCodex(t, codexID, codexBody(codexID))
	for id, lines := range map[string]int{claudeID: 4, codexID: 5} {
		if whole := export(t, m, ExportRequest{SessionID: id, Caller: true}); whole.Manifest.Cut || whole.Manifest.Lines != lines {
			t.Errorf("%s: a session whose last call is answered should go whole, %d lines: %+v", id, lines, whole.Manifest)
		}
	}
	m.writeClaude(t, claudeID, []string{claudeLine(claudeID, "user", `"hi"`), claudeLine(claudeID, "assistant", `[{"type":"text","text":"hello"}]`)})
	if whole := export(t, m, ExportRequest{SessionID: claudeID, Caller: true}); whole.Manifest.Cut || whole.Manifest.Lines != 2 {
		t.Errorf("a session with no tool call should go whole: %+v", whole.Manifest)
	}

	m.writeClaude(t, claudeID, claudeBody(claudeID, claudeInFlight(claudeID)))
	m.pidAlive = func(int) bool { return false }
	m.getenv = func(string) string { return "" }
	if whole := export(t, m, ExportRequest{SessionID: claudeID}); whole.Manifest.Cut {
		t.Error("cut without being the caller")
	}
}

func TestExportOfTheSessionTheEnvironmentNamesIsTheCallers(t *testing.T) {
	m := newMachine(t)
	p := m.writeCodex(t, codexID, codexBody(codexID, codexInFlight()))
	now := time.Now()
	if err := os.Chtimes(p, now, now); err != nil { // open, and written to just now
		t.Fatal(err)
	}
	if _, err := m.Export(t.Context(), ExportRequest{SessionID: codexID}); !errors.As(err, new(*LiveError)) {
		t.Fatalf("an open session that is not the caller's: %v", err)
	}
	m.getenv = func(k string) string {
		if k == "CODEX_SESSION_ID" {
			return codexID
		}
		return ""
	}
	if e := export(t, m, ExportRequest{SessionID: codexID}); !e.Manifest.Cut {
		t.Fatal("the session the environment names was not cut")
	}
}

func TestExportBlocksWhatLooksLikeASecretUnlessAllowed(t *testing.T) {
	m := newMachine(t)
	m.writeClaude(t, claudeID, claudeBody(claudeID, claudeLine(claudeID, "user", `"-----BEGIN PRIVATE KEY----- MIIE"`)))
	m.writeCodex(t, codexID, codexBody(codexID, codexLine(6, "response_item", `{"type":"message","role":"user","content":[{"type":"input_text","text":"AKIAABCDEFGHIJKLMNOP"}]}`)))

	for _, id := range []string{claudeID, codexID} {
		e, err := m.Export(t.Context(), ExportRequest{SessionID: id})
		var blocked *SensitiveError
		if !errors.As(err, &blocked) || len(blocked.Findings) == 0 || len(e.Findings) == 0 || e.Archive != nil {
			t.Fatalf("%s: %+v %v", id, e, err)
		}
		if strings.Contains(err.Error(), "MIIE") || strings.Contains(err.Error(), "AKIAABCDEFGHIJKLMNOP") {
			t.Errorf("%s: the message shows the secret: %v", id, err)
		}
		allowed := export(t, m, ExportRequest{SessionID: id, AllowSensitive: true})
		if len(allowed.Archive) == 0 || len(allowed.Findings) == 0 {
			t.Errorf("%s: AllowSensitive did not export or lost the findings: %+v", id, allowed)
		}
	}
}

func TestExportRefusesAnOpenSessionExceptTheCallers(t *testing.T) {
	m := newMachine(t)
	m.writeClaude(t, claudeID, claudeBody(claudeID, claudeInFlight(claudeID)))
	m.openClaude(t, claudeID, 4242)

	m.pidAlive = func(pid int) bool { return pid == 4242 }
	if _, err := m.Export(t.Context(), ExportRequest{SessionID: claudeID}); !errors.As(err, new(*LiveError)) {
		t.Fatalf("open session: %v", err)
	}
	if live, err := m.Live(Ref{Adapter: claude.ID, ID: claudeID}); err != nil || !live {
		t.Fatalf("Live: %v %v", live, err)
	}
	m.processes(map[int]int{100: 4242, 4242: 1}, 100)
	if e := export(t, m, ExportRequest{SessionID: claudeID, Caller: true}); !e.Manifest.Cut {
		t.Fatal("the caller's open session was not exported cut")
	}
	m.pidAlive = func(int) bool { return false } // the file is left by a process that died
	export(t, m, ExportRequest{SessionID: claudeID})
}

// processes is a synthetic process table, parent by pid, and self the process that runs the command.
func (m machine) processes(parents map[int]int, self int) {
	m.self = func() int { return self }
	m.parent = func(pid int) (int, bool) { p, ok := parents[pid]; return p, ok }
}

// A session open in another window is not the one that calls, whatever --session says:
// only the Claude Code above this command in the process tree is.
func TestExportClaimsOfTheCallerHoldOnlyForAnAncestorProcess(t *testing.T) {
	m := newMachine(t)
	m.writeClaude(t, claudeID, claudeBody(claudeID, claudeInFlight(claudeID)))
	m.openClaude(t, claudeID, 4242)
	m.pidAlive = func(pid int) bool { return pid == 4242 }

	// cf (100) runs under a shell (50) of another Claude Code (777); 4242 is a sibling tree.
	m.processes(map[int]int{100: 50, 50: 777, 777: 1, 4242: 1}, 100)
	if _, err := m.Export(t.Context(), ExportRequest{SessionID: claudeID, Caller: true}); !errors.As(err, new(*LiveError)) {
		t.Fatalf("a session open in another window was exported as the caller: %v", err)
	}

	m.processes(map[int]int{100: 50, 50: 4242, 4242: 1}, 100)
	if e := export(t, m, ExportRequest{SessionID: claudeID, Caller: true}); !e.Manifest.Cut {
		t.Fatal("the session of the Claude Code above the command was not the caller's")
	}

	m.processes(map[int]int{100: 50, 50: 100}, 100) // a loop in the table ends
	if _, err := m.Export(t.Context(), ExportRequest{SessionID: claudeID, Caller: true}); !errors.As(err, new(*LiveError)) {
		t.Fatalf("a loop: %v", err)
	}
}

func TestCodexIsOpenWhileItsRolloutIsBeingWritten(t *testing.T) {
	m := newMachine(t)
	p := m.writeCodex(t, codexID, codexBody(codexID))
	ref := Ref{Adapter: codex.ID, ID: codexID, Transcript: p}
	if live, err := m.Live(ref); err != nil || live {
		t.Fatalf("a rollout left an hour ago: %v %v", live, err)
	}
	m.clock = func() time.Time { return time.Now().Add(-59 * time.Minute) }
	if live, err := m.Live(ref); err != nil || !live {
		t.Fatalf("a rollout written a minute ago: %v %v", live, err)
	}
}

func TestExportOfASessionHandedOffNeedsForce(t *testing.T) {
	m := newMachine(t)
	m.writeClaude(t, claudeID, claudeBody(claudeID))
	if err := m.State.MarkHandedOff(claudeID, "task-1", "sha256:ab", "", time.Now()); err != nil {
		t.Fatal(err)
	}
	_, err := m.Export(t.Context(), ExportRequest{SessionID: claudeID})
	var handed *HandedOffError
	if !errors.As(err, &handed) || handed.Task != "task-1" {
		t.Fatalf("%v", err)
	}
	export(t, m, ExportRequest{SessionID: claudeID, Force: true})

	if err := m.State.ClearHandedOff(claudeID); err != nil {
		t.Fatal(err)
	}
	export(t, m, ExportRequest{SessionID: claudeID})
}

func TestStateRemembersTheTaskAndTheMarker(t *testing.T) {
	s := NewState(filepath.Join(t.TempDir(), "state"))
	if r, err := s.Get(claudeID); err != nil || r != (Record{}) {
		t.Fatalf("nothing remembered: %+v %v", r, err)
	}
	if err := s.Link(claudeID, "task-1"); err != nil {
		t.Fatal(err)
	}
	at := time.Date(2026, 11, 12, 10, 0, 0, 0, time.UTC)
	if err := s.MarkHandedOff(claudeID, "task-2", "sha256:cd", "", at); err != nil {
		t.Fatal(err)
	}
	r, _ := s.Get(claudeID)
	if r.Task != "task-2" || r.HandedOff == nil || r.HandedOff.Digest != "sha256:cd" || !r.HandedOff.At.Equal(at) {
		t.Fatalf("%+v", r)
	}
	if err := s.ClearHandedOff(claudeID); err != nil {
		t.Fatal(err)
	}
	if r, _ := s.Get(claudeID); r.Task != "task-2" || r.HandedOff != nil {
		t.Fatalf("the binding stays and the marker goes: %+v", r)
	}
	other := "22222222-3333-4444-8555-666666666666"
	if err := s.Link(other, "task-3"); err != nil {
		t.Fatal(err)
	}
	if err := s.Link(other, ""); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(s.dir, other+".json")); !os.IsNotExist(err) {
		t.Fatalf("an empty record leaves a file: %v", err)
	}
	if err := s.Link("../x", "t"); err == nil {
		t.Fatal("an id that is a path was accepted")
	}
}

// archiveOf exports lines as a session of another machine: what comes back from a Run.
func archiveOf(t *testing.T, adapter string, lines []string) []byte {
	t.Helper()
	m := newMachine(t)
	id := claudeID
	if adapter == codex.ID {
		id = codexID
		m.writeCodex(t, id, lines)
	} else {
		m.writeClaude(t, id, lines)
	}
	return export(t, m, ExportRequest{SessionID: id}).Archive
}

func TestImportOverTheSessionItIsAPrefixOfReplacesItAndKeepsABackup(t *testing.T) {
	for _, adapter := range []string{claude.ID, codex.ID} {
		t.Run(adapter, func(t *testing.T) {
			m := newMachine(t)
			var local, marker string
			var longer []string
			id := claudeID
			if adapter == codex.ID {
				id, marker = codexID, "task_complete"
				local = m.writeCodex(t, id, codexBody(id))
				longer = codexBody(id, codexLine(6, "event_msg", `{"type":"task_complete"}`))
			} else {
				marker = `"content":"more"`
				local = m.writeClaude(t, id, claudeBody(id))
				longer = claudeBody(id, claudeLine(id, "user", `"more"`))
			}
			before := readFile(t, local)

			res, err := m.Import(t.Context(), ImportRequest{Archive: archiveOf(t, adapter, longer), Cwd: workDir, SameClient: true})
			if err != nil {
				t.Fatal(err)
			}
			if res.NewID || res.SessionID != id || res.Transcript != local || res.Backup != local+".bak" || len(res.Warnings) != 0 {
				t.Fatalf("%+v", res)
			}
			if got := readFile(t, res.Backup); got != before {
				t.Errorf(".bak is not what was there:\n%s", got)
			}
			if got := readFile(t, local); !strings.HasPrefix(got, before) || !strings.Contains(got, marker) {
				t.Errorf("the session was not replaced by the longer one:\n%s", got)
			}
		})
	}
}

func TestImportOfTheSameSessionTwiceIsIdempotent(t *testing.T) {
	m := newMachine(t)
	m.writeClaude(t, claudeID, claudeBody(claudeID))
	archive := archiveOf(t, claude.ID, claudeBody(claudeID))
	for range 2 {
		res, err := m.Import(t.Context(), ImportRequest{Archive: archive, Cwd: workDir, SameClient: true})
		if err != nil || res.NewID {
			t.Fatalf("%+v %v", res, err)
		}
	}
}

func TestImportBesideASessionThatWentOnGetsANewIDAndLeavesTheLocalOneAlone(t *testing.T) {
	for _, adapter := range []string{claude.ID, codex.ID} {
		t.Run(adapter, func(t *testing.T) {
			m := newMachine(t)
			id := claudeID
			var path string
			var archive []byte
			if adapter == codex.ID {
				id = codexID
				path = m.writeCodex(t, id, codexBody(id, codexLine(6, "event_msg", `{"type":"local"}`)))
				archive = archiveOf(t, adapter, codexBody(id, codexLine(6, "event_msg", `{"type":"remote"}`)))
			} else {
				path = m.writeClaude(t, id, claudeBody(id, claudeLine(id, "user", `"local"`)))
				archive = archiveOf(t, adapter, claudeBody(id, claudeLine(id, "user", `"remote"`)))
			}
			before := readFile(t, path)

			res, err := m.Import(t.Context(), ImportRequest{Archive: archive, Cwd: workDir, SameClient: true})
			if err != nil {
				t.Fatal(err)
			}
			if !res.NewID || res.SessionID == id || len(res.Warnings) != 1 || res.Backup != "" {
				t.Fatalf("%+v", res)
			}
			if readFile(t, path) != before {
				t.Error("the local session was touched")
			}
			if got := readFile(t, res.Transcript); !strings.Contains(got, "remote") {
				t.Errorf("the incoming session is not under its new id:\n%s", got)
			}
		})
	}
}

func TestImportFromAnotherClientGetsANewID(t *testing.T) {
	m := newMachine(t)
	archive := archiveOf(t, claude.ID, claudeBody(claudeID))
	res, err := m.Import(t.Context(), ImportRequest{Archive: archive, Cwd: workDir})
	if err != nil || !res.NewID || res.SessionID == claudeID || len(res.Warnings) != 1 {
		t.Fatalf("%+v %v", res, err)
	}
	if !strings.Contains(readFile(t, res.Transcript), res.SessionID) {
		t.Error("the transcript does not say its new id")
	}
}

func TestImportOfASessionThatIsNotHereInstallsItUnderItsOwnID(t *testing.T) {
	m := newMachine(t)
	archive := archiveOf(t, codex.ID, codexBody(codexID))
	res, err := m.Import(t.Context(), ImportRequest{Archive: archive, Cwd: workDir, SameClient: true})
	if err != nil || res.NewID || res.SessionID != codexID || res.Backup != "" {
		t.Fatalf("%+v %v", res, err)
	}
}

func TestImportOverAnOpenSessionChangesNothing(t *testing.T) {
	m := newMachine(t)
	path := m.writeClaude(t, claudeID, claudeBody(claudeID))
	m.openClaude(t, claudeID, 4242)
	m.pidAlive = func(int) bool { return true }
	before := readFile(t, path)
	archive := archiveOf(t, claude.ID, claudeBody(claudeID, claudeLine(claudeID, "user", `"more"`)))

	_, err := m.Import(t.Context(), ImportRequest{Archive: archive, Cwd: workDir, SameClient: true})
	if !errors.As(err, new(*LiveError)) {
		t.Fatalf("%v", err)
	}
	if readFile(t, path) != before {
		t.Error("the open session was touched")
	}
	if _, err := os.Stat(path + ".bak"); !os.IsNotExist(err) {
		t.Error("a .bak was left")
	}
}

func TestImportRefusesWhatIsNotAnArchive(t *testing.T) {
	m := newMachine(t)
	if _, err := m.Import(t.Context(), ImportRequest{Archive: []byte("nope"), Cwd: workDir}); err == nil {
		t.Fatal("garbage was imported")
	}
}

// A session pulled into a worktree has a transcript under each checkout: the
// one written last is the session, not an ambiguity.
func TestFindClaudeTakesTheTranscriptWrittenLast(t *testing.T) {
	dir := t.TempDir()
	const id = "11111111-2222-4333-8444-555555555555"
	older := filepath.Join(dir, "projects", "-repo", id+".jsonl")
	newer := filepath.Join(dir, "projects", "-repo-worktree", id+".jsonl")
	for path, cwd := range map[string]string{older: "/repo", newer: "/repo-worktree"} {
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(`{"type":"user","cwd":"`+cwd+`"}`+"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	past := time.Now().Add(-time.Hour)
	if err := os.Chtimes(older, past, past); err != nil {
		t.Fatal(err)
	}
	path, cwd, found, err := findClaude(dir, id)
	if err != nil || !found || path != newer || cwd != "/repo-worktree" {
		t.Fatalf("findClaude = %q, %q, %v, %v; want the worktree's transcript", path, cwd, found, err)
	}
}

// The id in the manifest of a downloaded archive is not trusted to build a path.
func TestCheckImportRefusesAManifestWhoseSessionIDIsAPath(t *testing.T) {
	m := newMachine(t)
	var buf bytes.Buffer
	manifest := harness.SessionManifest{Adapter: claude.ID, SessionID: "../../../elsewhere/x", Cwd: workDir, Lines: 1}
	if err := harness.WriteSessionArchive(&buf, manifest, []byte("{}\n"), nil); err != nil {
		t.Fatal(err)
	}
	if err := m.CheckImport(t.Context(), ImportRequest{Archive: buf.Bytes(), Cwd: workDir, SameClient: true}); err == nil {
		t.Fatal("a manifest with a path as its session id was accepted")
	}
}

func TestSeededSessionOutlivesTheMarker(t *testing.T) {
	s := NewState(filepath.Join(t.TempDir(), "state"))
	if err := s.MarkHandedOff(claudeID, "task-1", "sha256:ab", "", time.Now()); err != nil {
		t.Fatal(err)
	}
	if err := s.ClearHandedOff(claudeID); err != nil {
		t.Fatal(err)
	}
	if id, found, err := s.SeededSession("sha256:ab"); err != nil || !found || id != claudeID {
		t.Fatalf("%q %v %v", id, found, err)
	}
}
