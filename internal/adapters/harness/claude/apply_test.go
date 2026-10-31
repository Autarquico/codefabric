package claude_test

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/Autarquico/codefabric/internal/adapters/harness"
	"github.com/Autarquico/codefabric/internal/adapters/harness/claude"
)

func applyRun(t *testing.T, h harness.Host, snap harness.NativeSnapshot, linkAuth bool) (string, harness.ApplyResult) {
	t.Helper()
	dir := t.TempDir()
	res, err := claude.Adapter{}.ApplyConfig(t.Context(), h, snap, harness.ApplyOptions{Target: harness.TargetRun{Dir: dir}, LinkAuth: linkAuth})
	if err != nil {
		t.Fatal(err)
	}
	return dir, res
}

func jsonOf(t *testing.T, s string) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal([]byte(s), &m); err != nil {
		t.Fatalf("not JSON: %v\n%s", err, s)
	}
	return m
}

func TestScenario_ClaudeCodeAdapter_ApplyAislado(t *testing.T) {
	h, home := newHost(t, homeFiles())
	snap := snapshot(t, h)
	claudeBefore, stateBefore := treeHash(t, filepath.Join(home, ".claude")), read(t, filepath.Join(home, ".claude.json"))
	homeBefore := treeHash(t, home)

	dir, res := applyRun(t, h, snap, true)

	if treeHash(t, home) != homeBefore || treeHash(t, filepath.Join(home, ".claude")) != claudeBefore || read(t, filepath.Join(home, ".claude.json")) != stateBefore {
		t.Fatal("the apply changed ~/.claude or ~/.claude.json")
	}
	if !slices.Equal(res.AuthLinks, []string{".credentials.json"}) || len(res.AuthCopies) != 0 {
		t.Errorf("auth = links %v, copies %v", res.AuthLinks, res.AuthCopies)
	}
	if len(res.Changes) != len(snap.Components) {
		t.Errorf("changes = %+v", res.Changes)
	}
	for _, p := range []string{"settings.json", "CLAUDE.md", "commands/team/ship.md", "agents/planner.md", "skills/tdd/scripts/run.sh", "output-styles/terse.md"} {
		if _, err := os.Stat(filepath.Join(dir, p)); err != nil {
			t.Errorf("the Run lacks %s: %v", p, err)
		}
	}
	// The link is to the login of the host and is the only symlink.
	target, err := os.Readlink(filepath.Join(dir, ".credentials.json"))
	if err != nil || target != filepath.Join(home, ".claude", ".credentials.json") {
		t.Errorf("link = %q, %v", target, err)
	}
	// .claude.json: only mcpServers and the onboarding, markers unrendered.
	state := jsonOf(t, read(t, filepath.Join(dir, ".claude.json")))
	if len(state) != 2 || state["hasCompletedOnboarding"] != true {
		t.Errorf(".claude.json = %v", state)
	}
	servers, _ := state["mcpServers"].(map[string]any)
	if len(servers) != 2 {
		t.Errorf("mcpServers = %v", servers)
	}
	if !strings.Contains(read(t, filepath.Join(dir, ".claude.json")), "{{cf-secret:claude/claude.json/mcpServers/stdio/env/TOKEN}}") {
		t.Error(".claude.json has no marker")
	}
	_ = filepath.WalkDir(dir, func(p string, d os.DirEntry, err error) error {
		if err == nil && d.Type().IsRegular() {
			assertNoSecretValues(t, p, read(t, p))
		}
		return nil
	})
	// A Run does not apply onto a directory that has something.
	if _, err := (claude.Adapter{}).ApplyConfig(t.Context(), h, snap, harness.ApplyOptions{Target: harness.TargetRun{Dir: dir}}); err == nil {
		t.Error("applied onto a directory that is not empty")
	}
}

func TestApplyRunWithoutLoginMakesNoLinkAndAnEmptySnapshotStillOnboards(t *testing.T) {
	h, home := newHost(t, nil)
	before := treeHash(t, home)
	dir, res := applyRun(t, h, snapshot(t, h), true)
	if len(res.AuthLinks) != 0 || len(res.Changes) != 0 {
		t.Errorf("result = %+v", res)
	}
	entries, _ := os.ReadDir(dir)
	if len(entries) != 1 || entries[0].Name() != ".claude.json" {
		t.Errorf("the Run has %v", entries)
	}
	if got := jsonOf(t, read(t, filepath.Join(dir, ".claude.json"))); len(got) != 1 || got["hasCompletedOnboarding"] != true {
		t.Errorf(".claude.json = %v", got)
	}
	if treeHash(t, home) != before {
		t.Error("the home changed")
	}
	// Without LinkAuth no link, even with a login.
	h2, _ := newHost(t, homeFiles())
	dir2, res2 := applyRun(t, h2, snapshot(t, h2), false)
	if _, err := os.Lstat(filepath.Join(dir2, ".credentials.json")); err == nil || len(res2.AuthLinks) != 0 {
		t.Error("linked the login without LinkAuth")
	}
}

func TestTwoRunsWithDifferentConfigurationsDoNotShareAnything(t *testing.T) {
	h, home := newHost(t, homeFiles())
	a := snapshot(t, h)
	seedHome(t, home, map[string]string{".claude/CLAUDE.md": "Answer in German.\n", ".claude.json": `{"mcpServers": {"only": {"command": "x"}}}`})
	if err := os.RemoveAll(filepath.Join(home, ".claude", "skills")); err != nil {
		t.Fatal(err)
	}
	b := snapshot(t, h)

	dirA, _ := applyRun(t, h, a, true)
	dirB, _ := applyRun(t, h, b, true)
	if dirA == dirB {
		t.Fatal("one directory")
	}
	if read(t, filepath.Join(dirA, "CLAUDE.md")) != "Always answer in French.\n" || read(t, filepath.Join(dirB, "CLAUDE.md")) != "Answer in German.\n" {
		t.Error("the CLAUDE.md of a Run is not its own")
	}
	if _, err := os.Stat(filepath.Join(dirB, "skills")); err == nil {
		t.Error("Run B has skills that its snapshot lacks")
	}
	if got := jsonOf(t, read(t, filepath.Join(dirB, ".claude.json")))["mcpServers"].(map[string]any); len(got) != 1 || got["only"] == nil {
		t.Errorf("Run B mcpServers = %v", got)
	}
	if err := os.WriteFile(filepath.Join(dirA, "CLAUDE.md"), []byte("edited"), 0o600); err != nil {
		t.Fatal(err)
	}
	if read(t, filepath.Join(dirB, "CLAUDE.md")) != "Answer in German.\n" || read(t, filepath.Join(home, ".claude", "CLAUDE.md")) != "Answer in German.\n" {
		t.Error("a Run changed another or the host")
	}
}

func TestApplyDryRunWritesNothing(t *testing.T) {
	h, home := newHost(t, homeFiles())
	snap := snapshot(t, h)
	dir := t.TempDir()
	for _, target := range []harness.Target{harness.TargetRun{Dir: dir}, harness.TargetGlobal{}} {
		before := treeHash(t, home)
		sn := snap
		if _, ok := target.(harness.TargetGlobal); ok {
			sn = harness.NativeSnapshot{Harness: claude.ID, SchemaVersion: 1, Components: []harness.Component{component(t, snap, "CLAUDE.md")}}
		}
		if _, err := (claude.Adapter{}).ApplyConfig(t.Context(), h, sn, harness.ApplyOptions{Target: target, DryRun: true, LinkAuth: true}); err != nil {
			t.Fatal(err)
		}
		if treeHash(t, home) != before {
			t.Errorf("%T dry run changed the home", target)
		}
	}
	if entries, _ := os.ReadDir(dir); len(entries) != 0 {
		t.Errorf("dry run wrote %v", entries)
	}
}

func TestApplyRefusesAForeignSnapshot(t *testing.T) {
	h, _ := newHost(t, nil)
	for name, snap := range map[string]harness.NativeSnapshot{
		"other harness": {Harness: "codex", SchemaVersion: 1, Components: []harness.Component{{Name: "x"}}},
		"other schema":  {Harness: claude.ID, SchemaVersion: 9, Components: []harness.Component{{Name: "CLAUDE.md"}}},
		"unknown name":  {Harness: claude.ID, SchemaVersion: 1, Components: []harness.Component{{Name: "projects", Kind: harness.KindDir, Ownership: harness.Managed}}},
		"path escapes": {Harness: claude.ID, SchemaVersion: 1, Components: []harness.Component{{
			Name: "CLAUDE.md", Kind: harness.KindFile, Ownership: harness.Managed,
			Files: []harness.File{{Path: "../x", Content: []byte("x")}},
		}}},
	} {
		if _, err := (claude.Adapter{}).ApplyConfig(t.Context(), h, snap, harness.ApplyOptions{Target: harness.TargetRun{Dir: t.TempDir()}}); err == nil {
			t.Errorf("%s: applied", name)
		}
	}
}

// --- TargetGlobal

func global(t *testing.T, h harness.Host, snap harness.NativeSnapshot) harness.ApplyResult {
	t.Helper()
	res, err := claude.Adapter{}.ApplyConfig(t.Context(), h, snap, harness.ApplyOptions{Target: harness.TargetGlobal{}})
	if err != nil {
		t.Fatal(err)
	}
	return res
}

func snapOf(t *testing.T, s harness.NativeSnapshot, names ...string) harness.NativeSnapshot {
	t.Helper()
	out := harness.NativeSnapshot{Harness: s.Harness, SchemaVersion: s.SchemaVersion}
	for _, n := range names {
		out.Components = append(out.Components, component(t, s, n))
	}
	return out
}

func mcp(servers string) harness.NativeSnapshot {
	c := `{"mcpServers": ` + servers + `}`
	return harness.NativeSnapshot{Harness: claude.ID, SchemaVersion: 1, Components: []harness.Component{{
		Name: "mcpServers", Kind: harness.KindJSONSubset, Ownership: harness.Managed,
		Files: []harness.File{{Path: ".claude.json", Mode: 0o600, Content: []byte(c)}},
	}}}
}

func TestGlobalApplyMergesMCPServersAndKeepsEveryOtherField(t *testing.T) {
	h, home := newHost(t, map[string]string{
		".claude.json":              `{"numStartups": 7, "big": 12345678901234567890, "projects": {"/x": {"a": [1, 2]}}, "mcpServers": {"mine": {"command": "local"}}}`,
		".claude/.credentials.json": "{}",
	})
	res := global(t, h, mcp(`{"team": {"command": "npx", "args": ["a"]}}`))
	if len(res.Changes) != 1 || res.Changes[0].Kind != harness.Modified {
		t.Errorf("changes = %+v", res.Changes)
	}
	got := jsonOf(t, read(t, filepath.Join(home, ".claude.json")))
	servers := got["mcpServers"].(map[string]any)
	if len(servers) != 2 || servers["mine"] == nil || servers["team"] == nil {
		t.Errorf("mcpServers = %v", servers)
	}
	raw := read(t, filepath.Join(home, ".claude.json"))
	if got["numStartups"] != float64(7) || got["projects"] == nil || !strings.Contains(raw, "12345678901234567890") {
		t.Errorf("another field changed:\n%s", raw)
	}
	if info, _ := os.Stat(filepath.Join(home, ".claude.json")); info.Mode().Perm() != 0o600 {
		t.Errorf("mode = %v", info.Mode())
	}

	// A second snapshot without "team": it goes, "mine" stays.
	global(t, h, mcp(`{"other": {"command": "y"}}`))
	servers = jsonOf(t, read(t, filepath.Join(home, ".claude.json")))["mcpServers"].(map[string]any)
	if len(servers) != 2 || servers["mine"] == nil || servers["other"] == nil || servers["team"] != nil {
		t.Errorf("mcpServers = %v", servers)
	}
	// An empty snapshot removes what was applied and leaves the rest.
	global(t, h, harness.NativeSnapshot{Harness: claude.ID})
	servers = jsonOf(t, read(t, filepath.Join(home, ".claude.json")))["mcpServers"].(map[string]any)
	if len(servers) != 1 || servers["mine"] == nil {
		t.Errorf("mcpServers = %v", servers)
	}
}

func TestGlobalApplyCreatesStateFileAndRemovesItWhenItEndsEmpty(t *testing.T) {
	h, home := newHost(t, nil)
	global(t, h, mcp(`{"a": {"command": "x"}}`))
	if got := jsonOf(t, read(t, filepath.Join(home, ".claude.json"))); got["mcpServers"] == nil {
		t.Fatalf("state = %v", got)
	}
	global(t, h, harness.NativeSnapshot{Harness: claude.ID})
	if _, err := os.Stat(filepath.Join(home, ".claude.json")); err == nil {
		t.Error("an empty .claude.json was left")
	}
}

func TestGlobalApplyOwnership(t *testing.T) {
	src, _ := newHost(t, homeFiles())
	snap := snapshot(t, src)
	snap = snapOf(t, snap, "CLAUDE.md", "commands", "agents")
	h, home := newHost(t, map[string]string{".claude/commands/mine.md": "mine", ".claude/CLAUDE.md": "person's"})

	// A local component is never written.
	snap.Components[0].Ownership = harness.Local
	res := global(t, h, snap)
	if read(t, filepath.Join(home, ".claude", "CLAUDE.md")) != "person's" {
		t.Error("a local component was overwritten")
	}
	for _, c := range res.Changes {
		if c.Name == "CLAUDE.md" {
			t.Errorf("a local component is in the changes: %+v", res.Changes)
		}
	}
	// A managed directory is replaced whole: the stray file goes.
	if _, err := os.Stat(filepath.Join(home, ".claude", "commands", "mine.md")); err == nil {
		t.Error("the managed directory kept a file the snapshot lacks")
	}
	if read(t, filepath.Join(home, ".claude", "commands", "review.md")) != "Review $ARGUMENTS\n" {
		t.Error("commands not written")
	}
	// Dropped from the snapshot, a managed component it wrote is deleted; a local one stays.
	global(t, h, snapOf(t, snap, "agents"))
	if _, err := os.Stat(filepath.Join(home, ".claude", "commands")); err == nil {
		t.Error("commands survived being dropped")
	}
	if read(t, filepath.Join(home, ".claude", "CLAUDE.md")) != "person's" {
		t.Error("a component the apply never wrote was removed")
	}
}

func TestGlobalApplyRefusesMarkers(t *testing.T) {
	src, _ := newHost(t, homeFiles())
	snap := snapshot(t, src)
	h, home := newHost(t, nil)
	_, err := claude.Adapter{}.ApplyConfig(t.Context(), h, snap, harness.ApplyOptions{Target: harness.TargetGlobal{}})
	if !errors.Is(err, harness.ErrSecretUnavailable) {
		t.Fatalf("err = %v", err)
	}
	if entries, _ := os.ReadDir(home); len(entries) != 0 {
		t.Errorf("wrote %v", entries)
	}
}

func TestDiffConfig(t *testing.T) {
	src, _ := newHost(t, homeFiles())
	snap := snapshot(t, src)

	// The same home: no changes, even though its secrets are plain there.
	d, err := claude.Adapter{}.DiffConfig(t.Context(), src, snap)
	if err != nil || len(d.Changes) != 0 {
		t.Fatalf("same home: %+v, %v", d, err)
	}

	empty, _ := newHost(t, nil)
	d, _ = claude.Adapter{}.DiffConfig(t.Context(), empty, snap)
	if len(d.Changes) != len(snap.Components) {
		t.Fatalf("empty host: %+v", d.Changes)
	}
	for _, c := range d.Changes {
		if c.Kind != harness.Added {
			t.Errorf("%+v", c)
		}
	}

	edited, _ := newHost(t, homeFiles())
	seedHome(t, edited.Home(), map[string]string{
		".claude/CLAUDE.md": "other",
		".claude.json":      `{"mcpServers": {"stdio": {"command": "changed"}}}`,
	})
	d, _ = claude.Adapter{}.DiffConfig(t.Context(), edited, snap)
	got := map[string]harness.ChangeKind{}
	for _, c := range d.Changes {
		got[c.Name] = c.Kind
	}
	if len(got) != 2 || got["CLAUDE.md"] != harness.Modified || got["mcpServers"] != harness.Modified {
		t.Errorf("changes = %v", got)
	}

	// Removed: applied, then dropped from the snapshot while still on the host.
	h, _ := newHost(t, nil)
	global(t, h, snapOf(t, mustPlain(t), "CLAUDE.md"))
	d, _ = claude.Adapter{}.DiffConfig(t.Context(), h, harness.NativeSnapshot{Harness: claude.ID})
	if len(d.Changes) != 1 || d.Changes[0].Kind != harness.Removed || d.Changes[0].Name != "CLAUDE.md" {
		t.Errorf("removed: %+v", d.Changes)
	}
	// Components of the person that no apply wrote are not removed.
	h2, _ := newHost(t, map[string]string{".claude/CLAUDE.md": "x"})
	if d, _ := (claude.Adapter{}).DiffConfig(t.Context(), h2, harness.NativeSnapshot{Harness: claude.ID}); len(d.Changes) != 0 {
		t.Errorf("unapplied: %+v", d.Changes)
	}
}

// mustPlain is a snapshot of a home without secrets.
func mustPlain(t *testing.T) harness.NativeSnapshot {
	t.Helper()
	h, _ := newHost(t, map[string]string{".claude/CLAUDE.md": "plain"})
	return snapshot(t, h)
}

func TestGlobalApplyRefusesAnUnknownRecordedComponentAndTouchesNothing(t *testing.T) {
	h, home := newHost(t, homeFiles())
	rec := filepath.Join(home, ".claude", ".cf-applied.json")
	if err := os.WriteFile(rec, []byte(`{"unknown":[]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	before := treeHash(t, home)
	for _, dry := range []bool{true, false} {
		_, err := claude.Adapter{}.ApplyConfig(t.Context(), h, harness.NativeSnapshot{Harness: claude.ID, SchemaVersion: 1}, harness.ApplyOptions{Target: harness.TargetGlobal{}, DryRun: dry})
		if err == nil {
			t.Errorf("dry=%v: applied with an unknown recorded component", dry)
		}
	}
	if treeHash(t, home) != before {
		t.Error("HOME changed")
	}
}
