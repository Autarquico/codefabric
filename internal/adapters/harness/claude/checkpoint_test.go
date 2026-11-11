package claude_test

import (
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/Autarquico/codefabric/internal/adapters/harness"
	"github.com/Autarquico/codefabric/internal/adapters/harness/claude"
	"github.com/Autarquico/codefabric/internal/domain"
)

const otherSessionID = "aaaaaaaa-bbbb-4ccc-8ddd-eeeeeeeeeeee"

// sessionDir is where Claude Code keeps the sessions of a cwd under configDir.
func sessionDir(t *testing.T, configDir, cwd string) string {
	t.Helper()
	resolved, err := filepath.EvalSymlinks(cwd)
	if err != nil {
		t.Fatal(err)
	}
	return filepath.Join(configDir, "projects", regexp.MustCompile(`[^a-zA-Z0-9]`).ReplaceAllString(resolved, "-"))
}

// writeSessionFile leaves a session of two messages: the prompt and its answer.
func writeSessionFile(t *testing.T, configDir, cwd, id, prompt string) {
	t.Helper()
	if err := os.MkdirAll(cwd, 0o750); err != nil {
		t.Fatal(err)
	}
	dir := sessionDir(t, configDir, cwd)
	if err := os.MkdirAll(dir, 0o750); err != nil {
		t.Fatal(err)
	}
	line := func(typ, role, text string) string {
		return `{"type":"` + typ + `","cwd":"` + cwd + `","sessionId":"` + id + `","version":"2.1.319","message":{"role":"` + role + `","content":"` + text + `"}}` + "\n"
	}
	body := line("user", "user", prompt) + line("assistant", "assistant", "done")
	if err := os.WriteFile(filepath.Join(dir, id+".jsonl"), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

// checkpointOfAnotherSession is a checkpoint of a session of another id, made
// in a directory of its own.
func checkpointOfAnotherSession(t *testing.T) harness.Checkpoint {
	t.Helper()
	root := t.TempDir()
	h := harness.Handle{RunID: "other-run", LeaseEpoch: 1, ConfigDir: filepath.Join(root, "config"), Worktree: filepath.Join(root, "wt")}
	writeSessionFile(t, h.ConfigDir, h.Worktree, claude.SessionID(h.RunID, h.LeaseEpoch), "another prompt")
	cp, err := claude.Adapter{}.Checkpoint(t.Context(), nil, h)
	if err != nil {
		t.Fatal(err)
	}
	return cp
}

// checkpointedRun is a Run with a session on disk and the checkpoint of it.
func checkpointedRun(t *testing.T, mode domain.Mode) (harness.Handle, harness.Checkpoint) {
	t.Helper()
	root := t.TempDir()
	run := spec(mode)
	run.ConfigDir, run.Worktree = filepath.Join(root, "config"), filepath.Join(root, "wt")
	writeSessionFile(t, run.ConfigDir, run.Worktree, claude.SessionID(run.RunID, run.LeaseEpoch), "fix it")
	h := harness.Handle{RunID: run.RunID, LeaseEpoch: run.LeaseEpoch, Mode: mode, ConfigDir: run.ConfigDir, Worktree: run.Worktree}
	cp, err := claude.Adapter{}.Checkpoint(t.Context(), nil, h)
	if err != nil {
		t.Fatal(err)
	}
	if cp.Harness != claude.ID || cp.Format != claude.CheckpointFormat || len(cp.Data) == 0 {
		t.Fatalf("checkpoint = %+v", cp)
	}
	return h, cp
}

// nextRun is the Run that resumes: another run, with directories of its own.
func nextRun(t *testing.T, mode domain.Mode, prompt string) harness.RunSpec {
	t.Helper()
	root := t.TempDir()
	run := spec(mode)
	run.RunID, run.Prompt = "r2", prompt
	run.ConfigDir, run.Worktree = filepath.Join(root, "config"), filepath.Join(root, "wt")
	if err := os.MkdirAll(run.Worktree, 0o750); err != nil {
		t.Fatal(err)
	}
	return run
}

func TestResumeHeadlessContinuesTheCheckpointedSession(t *testing.T) {
	h := hostWithBinary(t, "#!/bin/sh\n")
	bin, _ := h.LookPath(claude.Binary)
	_, cp := checkpointedRun(t, domain.ModeHeadless)
	run := nextRun(t, domain.ModeHeadless, "keep going")
	l, err := claude.Adapter{}.Resume(t.Context(), h, run, cp)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{
		bin, "-p", "--output-format", "stream-json", "--verbose", "--permission-mode", "bypassPermissions",
		"--disallowedTools", "AskUserQuestion", "--resume=" + claude.SessionID("r1", 1), "--", "keep going",
	}
	if !slices.Equal(l.Argv, want) || !l.StructuredStdout || !slices.Equal(l.Env, []string{"CLAUDE_CONFIG_DIR=" + run.ConfigDir}) {
		t.Errorf("launch = %+v", l)
	}
	raw, err := os.ReadFile(filepath.Join(sessionDir(t, run.ConfigDir, run.Worktree), claude.SessionID("r1", 1)+".jsonl"))
	if err != nil {
		t.Fatalf("the session is not in the ConfigDir of the new Run: %v", err)
	}
	if !strings.Contains(string(raw), `"cwd":"`+run.Worktree+`"`) || strings.Count(string(raw), `"cwd":"`) != strings.Count(string(raw), `"cwd":"`+run.Worktree+`"`) {
		t.Errorf("cwd was not rewritten to %s:\n%s", run.Worktree, raw)
	}
}

func TestResumeInteractiveNeedsNoPrompt(t *testing.T) {
	h := hostWithBinary(t, "#!/bin/sh\n")
	bin, _ := h.LookPath(claude.Binary)
	_, cp := checkpointedRun(t, domain.ModeInteractive)
	id := claude.SessionID("r1", 1)

	l, err := claude.Adapter{}.Resume(t.Context(), h, nextRun(t, domain.ModeInteractive, ""), cp)
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{bin, "--permission-mode", "bypassPermissions", "--resume=" + id}; !slices.Equal(l.Argv, want) {
		t.Errorf("argv = %q", l.Argv)
	}
	l, err = claude.Adapter{}.Resume(t.Context(), h, nextRun(t, domain.ModeInteractive, "hello"), cp)
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{bin, "--permission-mode", "bypassPermissions", "--resume=" + id, "--", "hello"}; !slices.Equal(l.Argv, want) {
		t.Errorf("argv = %q", l.Argv)
	}
}

func TestResumeRefuses(t *testing.T) {
	h := hostWithBinary(t, "#!/bin/sh\n")
	_, cp := checkpointedRun(t, domain.ModeHeadless)
	for name, tc := range map[string]struct {
		run func() harness.RunSpec
		cp  harness.Checkpoint
	}{
		"headless without prompt": {func() harness.RunSpec { return nextRun(t, domain.ModeHeadless, "") }, cp},
		"another harness":         {func() harness.RunSpec { return nextRun(t, domain.ModeHeadless, "go") }, harness.Checkpoint{Harness: "codex", Format: cp.Format, Data: cp.Data}},
		"another format":          {func() harness.RunSpec { return nextRun(t, domain.ModeHeadless, "go") }, harness.Checkpoint{Harness: cp.Harness, Format: "v0", Data: cp.Data}},
		"not an archive":          {func() harness.RunSpec { return nextRun(t, domain.ModeHeadless, "go") }, harness.Checkpoint{Harness: cp.Harness, Format: cp.Format, Data: []byte("nope")}},
	} {
		t.Run(name, func(t *testing.T) {
			run := tc.run()
			if _, err := (claude.Adapter{}).Resume(t.Context(), h, run, tc.cp); err == nil {
				t.Fatal("no error")
			}
			if entries, _ := os.ReadDir(run.ConfigDir); len(entries) != 0 {
				t.Errorf("a refused Resume wrote %v", entries)
			}
		})
	}
}

// A Resume repeated in the same epoch continues the session it installed; the
// prompt is sent again only while the session does not hold it.
func TestResumeIsIdempotentAndDoesNotRepeatAPromptTheSessionHolds(t *testing.T) {
	for _, mode := range []domain.Mode{domain.ModeHeadless, domain.ModeInteractive} {
		t.Run(string(mode), func(t *testing.T) {
			h := hostWithBinary(t, "#!/bin/sh\n")
			_, cp := checkpointedRun(t, mode)
			run := nextRun(t, mode, "keep going")
			first, err := claude.Adapter{}.Resume(t.Context(), h, run, cp)
			if err != nil {
				t.Fatal(err)
			}
			again, err := claude.Adapter{}.Resume(t.Context(), h, run, cp)
			if err != nil || !slices.Equal(first.Argv, again.Argv) {
				t.Fatalf("argv %q, then %q, %v", first.Argv, again.Argv, err)
			}
			// The process got the prompt in.
			path := filepath.Join(sessionDir(t, run.ConfigDir, run.Worktree), claude.SessionID("r1", 1)+".jsonl")
			f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0) //nolint:gosec // a file of the test
			if err != nil {
				t.Fatal(err)
			}
			if _, err := f.WriteString(`{"type":"user","message":{"role":"user","content":"keep going"}}` + "\n"); err != nil {
				t.Fatal(err)
			}
			if err := f.Close(); err != nil {
				t.Fatal(err)
			}
			restarted, err := claude.Adapter{}.Resume(t.Context(), h, run, cp)
			if err != nil {
				t.Fatal(err)
			}
			if slices.Contains(restarted.Argv, "keep going") {
				t.Errorf("the prompt was sent again: %q", restarted.Argv)
			}
			if headless := mode == domain.ModeHeadless; headless != slices.Contains(restarted.Argv, "--") {
				t.Errorf("argv = %q: headless needs a prompt, interactive has none", restarted.Argv)
			}
		})
	}
}

func TestStartDoesNotRepeatThePromptTheSessionHolds(t *testing.T) {
	h := hostWithBinary(t, "#!/bin/sh\n")
	run := spec(domain.ModeHeadless)
	run.ConfigDir, run.Worktree = t.TempDir(), t.TempDir()
	writeSessionFile(t, run.ConfigDir, run.Worktree, claude.SessionID("r1", 1), "fix it")
	l, err := claude.Adapter{}.Start(t.Context(), h, run)
	if err != nil {
		t.Fatal(err)
	}
	if slices.Contains(l.Argv, "fix it") || !slices.Contains(l.Argv, "Continue where you left off.") || !slices.Contains(l.Argv, "--resume="+claude.SessionID("r1", 1)) {
		t.Errorf("argv = %q", l.Argv)
	}
	run.Mode = domain.ModeInteractive
	if l, err = (claude.Adapter{}).Start(t.Context(), h, run); err != nil {
		t.Fatal(err)
	}
	if slices.Contains(l.Argv, "--") {
		t.Errorf("interactive argv = %q: it has no prompt to send", l.Argv)
	}
}

// The round trip: the checkpoint of a Run, resumed, is a Run that can be
// checkpointed again, and its session is the same one.
func TestCheckpointOfAResumedRunKeepsTheSession(t *testing.T) {
	h := hostWithBinary(t, "#!/bin/sh\n")
	_, cp := checkpointedRun(t, domain.ModeHeadless)
	run := nextRun(t, domain.ModeHeadless, "keep going")
	if _, err := (claude.Adapter{}).Resume(t.Context(), h, run, cp); err != nil {
		t.Fatal(err)
	}
	handle := harness.Handle{RunID: run.RunID, LeaseEpoch: run.LeaseEpoch, ConfigDir: run.ConfigDir, Worktree: run.Worktree}
	again, err := claude.Adapter{}.Checkpoint(t.Context(), h, handle)
	if err != nil {
		t.Fatal(err)
	}
	third := nextRun(t, domain.ModeHeadless, "and on")
	l, err := claude.Adapter{}.Resume(t.Context(), h, third, again)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(l.Argv, "--resume="+claude.SessionID("r1", 1)) {
		t.Errorf("argv = %q", l.Argv)
	}
}

func TestCheckpointNeedsOneSessionOfTheRun(t *testing.T) {
	root := t.TempDir()
	handle := harness.Handle{RunID: "r9", LeaseEpoch: 1, ConfigDir: filepath.Join(root, "config"), Worktree: filepath.Join(root, "wt")}
	if err := os.MkdirAll(handle.Worktree, 0o750); err != nil {
		t.Fatal(err)
	}
	if _, err := (claude.Adapter{}).Checkpoint(t.Context(), nil, handle); err == nil {
		t.Error("a Run with no session was checkpointed")
	}
	writeSessionFile(t, handle.ConfigDir, handle.Worktree, otherSessionID, "a")
	if _, err := (claude.Adapter{}).Checkpoint(t.Context(), nil, handle); err != nil {
		t.Errorf("the only session is the Run's: %v", err)
	}
	writeSessionFile(t, handle.ConfigDir, handle.Worktree, "bbbbbbbb-bbbb-4ccc-8ddd-eeeeeeeeeeee", "b")
	if _, err := (claude.Adapter{}).Checkpoint(t.Context(), nil, handle); err == nil {
		t.Error("two sessions and none the Run's: it must not guess")
	}
}
