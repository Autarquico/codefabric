package claude_test

import (
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"testing"

	"github.com/Autarquico/codefabric/internal/adapters/harness/claude"
	"github.com/Autarquico/codefabric/internal/domain"
)

func TestSessionIDIsAStableUUIDv5OfRunAndEpoch(t *testing.T) {
	id := claude.SessionID("r1", 1)
	if !regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-5[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`).MatchString(id) {
		t.Errorf("id = %q is not a UUID v5", id)
	}
	if id != claude.SessionID("r1", 1) || id == claude.SessionID("r1", 2) || id == claude.SessionID("r2", 1) {
		t.Errorf("id = %q: it must depend on the run and the epoch, and on nothing else", id)
	}
}

// writeSession leaves the jsonl Claude Code would have written for the session
// of run r1 in the epoch, under the directory it names after the resolved cwd.
func writeSession(t *testing.T, configDir, cwd string, epoch uint64) {
	t.Helper()
	resolved, err := filepath.EvalSymlinks(cwd)
	if err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(configDir, "projects", regexp.MustCompile(`[^a-zA-Z0-9]`).ReplaceAllString(resolved, "-"))
	if err := os.MkdirAll(dir, 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, claude.SessionID("r1", epoch)+".jsonl"), []byte("{}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
}

// A Start repeated once the session exists resumes it with the same id: the CLI
// refuses --session-id for a session in use.
func TestStartResumesTheSessionThatAlreadyExists(t *testing.T) {
	for _, mode := range []domain.Mode{domain.ModeHeadless, domain.ModeInteractive} {
		t.Run(string(mode), func(t *testing.T) {
			h := hostWithBinary(t, "#!/bin/sh\n")
			run := spec(mode)
			run.ConfigDir, run.Worktree = t.TempDir(), t.TempDir()
			first, err := claude.Adapter{}.Start(t.Context(), h, run)
			if err != nil {
				t.Fatal(err)
			}
			if !slices.Contains(first.Argv, "--session-id") || slices.Contains(first.Argv, "--resume") {
				t.Fatalf("first argv = %q", first.Argv)
			}
			writeSession(t, run.ConfigDir, run.Worktree, 1)
			again, err := claude.Adapter{}.Start(t.Context(), h, run)
			if err != nil {
				t.Fatal(err)
			}
			i := slices.Index(again.Argv, "--resume")
			if i < 0 || again.Argv[i+1] != claude.SessionID("r1", 1) || slices.Contains(again.Argv, "--session-id") {
				t.Errorf("argv = %q", again.Argv)
			}
			run.LeaseEpoch = 2
			next, err := claude.Adapter{}.Start(t.Context(), h, run)
			if err != nil {
				t.Fatal(err)
			}
			if !slices.Contains(next.Argv, "--session-id") || slices.Contains(next.Argv, "--resume") {
				t.Errorf("another epoch has its own session: argv = %q", next.Argv)
			}
		})
	}
}
