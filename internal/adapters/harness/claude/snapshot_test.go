package claude_test

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/Autarquico/codeflock/internal/adapters/harness"
	"github.com/Autarquico/codeflock/internal/adapters/harness/claude"
)

func snapshot(t *testing.T, h harness.Host) harness.NativeSnapshot {
	t.Helper()
	s, err := claude.Adapter{}.SnapshotConfig(t.Context(), h, harness.SnapshotOptions{})
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func TestScenario_ClaudeCodeAdapter_Snapshot(t *testing.T) {
	h, home := newHost(t, homeFiles())
	before := treeHash(t, home)
	snap := snapshot(t, h)
	if treeHash(t, home) != before {
		t.Error("SnapshotConfig changed the home")
	}

	want := []string{"settings.json", "CLAUDE.md", "commands", "agents", "skills", "output-styles", "mcpServers"}
	if got := names(snap); !slices.Equal(got, want) {
		t.Fatalf("components = %v, want %v", got, want)
	}
	if snap.Harness != claude.ID || snap.SchemaVersion != claude.SchemaVersion {
		t.Errorf("snapshot = %s schema %d", snap.Harness, snap.SchemaVersion)
	}

	// Stable: the same home gives the same components, files and digests.
	again := snapshot(t, h)
	for i, c := range snap.Components {
		if c.Digest == "" || c.Digest != again.Components[i].Digest {
			t.Errorf("%s: digest %q then %q", c.Name, c.Digest, again.Components[i].Digest)
		}
		if d, err := harness.FilesDigest(c.Files); err != nil || d != c.Digest {
			t.Errorf("%s: digest %q is not FilesDigest of its files (%q, %v)", c.Name, c.Digest, d, err)
		}
		if c.Ownership != harness.Managed {
			t.Errorf("%s is %s", c.Name, c.Ownership)
		}
	}

	// No credential, no state of the CLI, no value of a secret.
	var paths []string
	for _, c := range snap.Components {
		for _, f := range c.Files {
			paths = append(paths, f.Path)
			assertNoSecretValues(t, f.Path, string(f.Content))
			if err := f.CheckSecrets(); err != nil {
				t.Errorf("%s: %v", f.Path, err)
			}
		}
	}
	for _, p := range paths {
		for _, banned := range []string{".credentials.json", "projects/", "todos/", "statsig/", "history", "sessions/"} {
			if strings.Contains(p, banned) {
				t.Errorf("the snapshot has %s", p)
			}
		}
	}
	wantPaths := []string{
		"settings.json", "CLAUDE.md", "commands/review.md", "commands/team/ship.md", "agents/planner.md",
		"skills/tdd/SKILL.md", "skills/tdd/scripts/run.sh", "output-styles/terse.md", ".claude.json",
	}
	if !slices.Equal(paths, wantPaths) {
		t.Errorf("files = %v, want %v", paths, wantPaths)
	}

	settings := string(component(t, snap, "settings.json").Files[0].Content)
	wantSettings := `{
  "apiKeyHelper": "~/bin/print-key",
  "cleanupPeriodDays": 30,
  "env": {
    "ANTHROPIC_AUTH_TOKEN": "{{cf-secret:claude/settings/env/ANTHROPIC_AUTH_TOKEN}}",
    "EMPTY": ""
  },
  "hooks": {
    "PostToolUse": [
      {
        "hooks": [
          {
            "headers": {
              "Authorization": "{{cf-secret:claude/settings/hooks/PostToolUse/0/hooks/0/headers/Authorization}}"
            },
            "type": "http",
            "url": "https://hooks.example/x"
          },
          {
            "command": "echo done",
            "type": "command"
          }
        ],
        "matcher": "Edit"
      }
    ]
  },
  "model": "opus"
}
`
	if settings != wantSettings {
		t.Errorf("settings.json =\n%s\nwant\n%s", settings, wantSettings)
	}
	mcp := component(t, snap, "mcpServers")
	wantMCP := `{
  "mcpServers": {
    "remote": {
      "headers": {
        "Authorization": "{{cf-secret:claude/claude.json/mcpServers/remote/headers/Authorization}}"
      },
      "type": "http",
      "url": "https://mcp.example/x"
    },
    "stdio": {
      "args": [
        "-y",
        "srv"
      ],
      "command": "npx",
      "env": {
        "TOKEN": "{{cf-secret:claude/claude.json/mcpServers/stdio/env/TOKEN}}"
      },
      "type": "stdio"
    }
  }
}
`
	if got := string(mcp.Files[0].Content); got != wantMCP || mcp.Kind != harness.KindJSONSubset || mcp.Files[0].Path != ".claude.json" {
		t.Errorf("mcpServers = %s (%s, %s), want\n%s", got, mcp.Kind, mcp.Files[0].Path, wantMCP)
	}
	if got := component(t, snap, "settings.json").Files[0].SecretRefs; len(got) != 2 {
		t.Errorf("settings.json SecretRefs = %v", got)
	}
}

func TestSnapshotOfAnEmptyHomeHasNoComponents(t *testing.T) {
	h, _ := newHost(t, nil)
	if snap := snapshot(t, h); len(snap.Components) != 0 || snap.Harness != claude.ID {
		t.Errorf("snapshot = %+v", snap)
	}
}

func TestSnapshotWithoutMCPServersHasNoMCPComponent(t *testing.T) {
	h, _ := newHost(t, map[string]string{".claude.json": `{"numStartups": 1}`, ".claude/CLAUDE.md": "x"})
	if got := names(snapshot(t, h)); !slices.Equal(got, []string{"CLAUDE.md"}) {
		t.Errorf("components = %v", got)
	}
}

func TestSnapshotOfSomeComponents(t *testing.T) {
	h, _ := newHost(t, homeFiles())
	s, err := claude.Adapter{}.SnapshotConfig(t.Context(), h, harness.SnapshotOptions{Components: []string{"skills", "mcpServers"}})
	if err != nil {
		t.Fatal(err)
	}
	if got := names(s); !slices.Equal(got, []string{"skills", "mcpServers"}) {
		t.Errorf("components = %v", got)
	}
	if _, err := (claude.Adapter{}).SnapshotConfig(t.Context(), h, harness.SnapshotOptions{Components: []string{"projects"}}); err == nil {
		t.Error("a component that is not adoptable was snapshotted")
	}
}

func TestSnapshotKeepsAMarkerThePersonWroteAndNumbersAsTheyAre(t *testing.T) {
	h, _ := newHost(t, map[string]string{".claude/settings.json": `{"env": {"A": "{{cf-secret:team/a}}"}, "big": 12345678901234567890, "f": 1.50}`})
	got := string(component(t, snapshot(t, h), "settings.json").Files[0].Content)
	for _, want := range []string{`"A": "{{cf-secret:team/a}}"`, `12345678901234567890`, `1.50`} {
		if !strings.Contains(got, want) {
			t.Errorf("settings.json lacks %s:\n%s", want, got)
		}
	}
}

func TestSnapshotRefsDoNotCollideBetweenFilesAndSanitiseSegments(t *testing.T) {
	h, _ := newHost(t, map[string]string{
		".claude/settings.json": `{"env": {"A": "x"}}`,
		".claude.json":          `{"mcpServers": {"my server": {"env": {"A": "y"}}}}`,
	})
	snap := snapshot(t, h)
	a := component(t, snap, "settings.json").Files[0].SecretRefs
	b := component(t, snap, "mcpServers").Files[0].SecretRefs
	if len(a) != 1 || a[0] != "claude/settings/env/A" {
		t.Errorf("settings refs = %v", a)
	}
	if len(b) != 1 || !strings.HasPrefix(b[0], "claude/claude.json/mcpServers/my_server-") || !strings.HasSuffix(b[0], "/env/A") {
		t.Errorf("mcp refs = %v", b)
	}
}

func TestSnapshotRefusesWhatItCannotRead(t *testing.T) {
	for name, files := range map[string]map[string]string{
		"settings not JSON": {".claude/settings.json": `model = "x"`},
		"settings array":    {".claude/settings.json": `[]`},
		"state not JSON":    {".claude.json": `{`},
		"trailing data":     {".claude/settings.json": `{} {}`},
	} {
		h, _ := newHost(t, files)
		if _, err := (claude.Adapter{}).SnapshotConfig(t.Context(), h, harness.SnapshotOptions{}); err == nil {
			t.Errorf("%s: no error", name)
		}
	}
}

func TestSnapshotRefusesASymlinkInADirectory(t *testing.T) {
	h, home := newHost(t, map[string]string{".claude/skills/a.md": "x"})
	if err := os.Symlink(filepath.Join(home, ".claude", "skills", "a.md"), filepath.Join(home, ".claude", "skills", "b.md")); err != nil {
		t.Fatal(err)
	}
	if _, err := (claude.Adapter{}).SnapshotConfig(t.Context(), h, harness.SnapshotOptions{}); err == nil {
		t.Error("a symlink was snapshotted")
	}
}

func TestMarkersAreOnlyAllowedInJSONStringValues(t *testing.T) {
	m := harness.SecretMarker("team/a")
	file := func(path, content string) harness.NativeSnapshot {
		kind, name := harness.KindFile, path
		if path == ".claude.json" {
			kind, name = harness.KindJSONSubset, "mcpServers"
		}
		return harness.NativeSnapshot{Harness: claude.ID, SchemaVersion: 1, Components: []harness.Component{{
			Name: name, Kind: kind, Ownership: harness.Managed,
			Files: []harness.File{{Path: path, Mode: 0o600, Content: []byte(content), SecretRefs: harness.SecretRefs([]byte(content))}},
		}}}
	}
	h, _ := newHost(t, nil)
	run := harness.ApplyOptions{Target: harness.TargetRun{Dir: t.TempDir()}, DryRun: true}
	for name, snap := range map[string]harness.NativeSnapshot{
		"in a key":          file("settings.json", `{"`+m+`": "x"}`),
		"in a nested key":   file("settings.json", `{"env": {"`+m+`": "x"}}`),
		"as a bare value":   file("settings.json", `{"a": `+m+`}`),
		"in markdown":       file("CLAUDE.md", "token "+m),
		"in the mcp key":    file(".claude.json", `{"mcpServers": {"`+m+`": {}}}`),
		"dangling":          file("settings.json", `{"a": "{{cf-secret:"}`),
		"mcp not an object": file(".claude.json", `{"mcpServers": []}`),
		"mcp extra field":   file(".claude.json", `{"mcpServers": {}, "x": 1}`),
		"settings not JSON": file("settings.json", `{`),
	} {
		_, err := claude.Adapter{}.ApplyConfig(t.Context(), h, snap, run)
		if err == nil {
			t.Errorf("%s: applied", name)
		}
	}
	ok := file("settings.json", `{"a": "x-`+m+`-y", "l": ["`+m+`"]}`)
	if _, err := (claude.Adapter{}).ApplyConfig(t.Context(), h, ok, run); err != nil {
		t.Errorf("markers in string values: %v", err)
	}
}

func TestSecretEscapeIsJSONForTheTwoJSONFilesOnly(t *testing.T) {
	a := claude.Adapter{}
	for _, p := range []string{"settings.json", ".claude.json"} {
		if e, err := a.SecretEscape(p); err != nil || e != harness.EscapeJSONString {
			t.Errorf("%s: %q, %v", p, e, err)
		}
	}
	if _, err := a.SecretEscape("CLAUDE.md"); err == nil {
		t.Error("CLAUDE.md has an escape")
	}
}

func TestValidateConfigNamesTheComponentsThatAreBroken(t *testing.T) {
	h, _ := newHost(t, map[string]string{".claude/settings.json": `{`, ".claude/CLAUDE.md": "ok", ".claude.json": `{"mcpServers": {}}`})
	v, err := claude.Adapter{}.ValidateConfig(context.Background(), h)
	if err != nil {
		t.Fatal(err)
	}
	if len(v.Issues) != 1 || v.Issues[0].Component != "settings.json" {
		t.Errorf("issues = %+v", v.Issues)
	}
	h, _ = newHost(t, homeFiles())
	if v, err := (claude.Adapter{}).ValidateConfig(context.Background(), h); err != nil || len(v.Issues) != 0 {
		t.Errorf("a good home: %+v, %v", v, err)
	}
}

// linkedSkill is a ~/.claude/skills/<name> link to dir.
func linkedSkill(t *testing.T, home, name, dir string) {
	t.Helper()
	skills := filepath.Join(home, ".claude", "skills")
	if err := os.MkdirAll(skills, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(dir, filepath.Join(skills, name)); err != nil {
		t.Fatal(err)
	}
}

// skillDir is a directory outside ~/.claude with one skill in it.
func skillDir(t *testing.T) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "pkg")
	if err := os.MkdirAll(filepath.Join(dir, "scripts"), 0o750); err != nil {
		t.Fatal(err)
	}
	for p, c := range map[string]string{"SKILL.md": "# tdd", "scripts/run.sh": "echo hi"} {
		if err := os.WriteFile(filepath.Join(dir, p), []byte(c), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

func skillsOf(t *testing.T, h harness.Host) []harness.File {
	t.Helper()
	snap, err := claude.Adapter{}.SnapshotConfig(t.Context(), h, harness.SnapshotOptions{Components: []string{"skills"}})
	if err != nil {
		t.Fatal(err)
	}
	if len(snap.Components) != 1 {
		t.Fatalf("components = %+v", snap.Components)
	}
	return snap.Components[0].Files
}

func TestSnapshotFollowsAFirstLevelLinkToADirectoryOfTheUser(t *testing.T) {
	h, home := newHost(t, nil)
	linkedSkill(t, home, "tdd", skillDir(t))
	var got []string
	for _, f := range skillsOf(t, h) {
		got = append(got, f.Path)
	}
	if !slices.Equal(got, []string{"skills/tdd/SKILL.md", "skills/tdd/scripts/run.sh"}) {
		t.Errorf("files = %v", got)
	}
}

func TestSnapshotFollowsALinkToADirectoryOfRoot(t *testing.T) {
	if os.Getuid() == 0 {
		t.Skip("running as root: a directory of root is a directory of the user")
	}
	entries, _ := os.ReadDir("/usr/share")
	tried := 0
	for _, e := range entries {
		p := filepath.Join("/usr/share", e.Name())
		fi, err := os.Lstat(p)
		if err != nil || !fi.IsDir() || fi.Sys().(*syscall.Stat_t).Uid != 0 || fi.Mode().Perm()&0o022 != 0 {
			continue
		}
		if tried++; tried > 20 { // a few small ones are enough; the rest may be big
			break
		}
		h, home := newHost(t, nil)
		linkedSkill(t, home, "sys", p)
		snap, err := claude.Adapter{}.SnapshotConfig(t.Context(), h, harness.SnapshotOptions{Components: []string{"skills"}})
		if err != nil || len(snap.Components) == 0 || len(snap.Components[0].Files) == 0 {
			continue
		}
		for _, f := range snap.Components[0].Files {
			if !strings.HasPrefix(f.Path, "skills/sys/") {
				t.Errorf("file %q is not recorded under the link", f.Path)
			}
		}
		return
	}
	t.Skip("no root-owned directory of plain files among the first under /usr/share to link to")
}

func TestSnapshotRefusesALinkItDoesNotTrust(t *testing.T) {
	for name, build := range map[string]func(t *testing.T, home string) string{
		"to a file": func(t *testing.T, _ string) string {
			f := filepath.Join(t.TempDir(), "a.md")
			_ = os.WriteFile(f, []byte("x"), 0o600)
			return f
		},
		"inside the target": func(t *testing.T, _ string) string {
			dir := skillDir(t)
			_ = os.Symlink(filepath.Join(dir, "SKILL.md"), filepath.Join(dir, "scripts", "other"))
			return dir
		},
		"to a group-writable dir": func(t *testing.T, _ string) string {
			dir := skillDir(t)
			_ = os.Chmod(dir, 0o775) //nolint:gosec // the test needs a loose mode
			return dir
		},
		"a subdirectory others can write": func(t *testing.T, _ string) string {
			dir := skillDir(t)
			_ = os.Chmod(filepath.Join(dir, "scripts"), 0o777) //nolint:gosec // the test needs a loose mode
			return dir
		},
		"a file others can write": func(t *testing.T, _ string) string {
			dir := skillDir(t)
			_ = os.Chmod(filepath.Join(dir, "SKILL.md"), 0o666) //nolint:gosec // the test needs a loose mode
			return dir
		},
		"below a directory others can write": func(t *testing.T, _ string) string {
			dir := skillDir(t)
			_ = os.Chmod(filepath.Dir(dir), 0o775) //nolint:gosec // the test needs a loose mode
			return dir
		},
		"a FIFO inside": func(t *testing.T, _ string) string {
			dir := skillDir(t)
			_ = syscall.Mkfifo(filepath.Join(dir, "pipe"), 0o600)
			return dir
		},
		"to a world-writable dir": func(t *testing.T, _ string) string {
			dir := skillDir(t)
			_ = os.Chmod(dir, 0o757) //nolint:gosec // the test needs a loose mode
			return dir
		},
		"to a link": func(t *testing.T, _ string) string {
			second := filepath.Join(t.TempDir(), "second")
			_ = os.Symlink(skillDir(t), second)
			return second
		},
	} {
		t.Run(name, func(t *testing.T) {
			h, home := newHost(t, nil)
			linkedSkill(t, home, "x", build(t, home))
			_, err := claude.Adapter{}.SnapshotConfig(t.Context(), h, harness.SnapshotOptions{})
			if err == nil {
				t.Fatal("the link was followed")
			}
			if strings.Contains(name, "inside") || strings.Contains(name, "file others") || strings.Contains(name, "subdirectory") {
				if !strings.Contains(err.Error(), "skills/x/") {
					t.Errorf("the error does not name the path: %v", err)
				}
			}
		})
	}
}

func TestSnapshotRefusesALinkBelowTheFirstLevel(t *testing.T) {
	h, home := newHost(t, map[string]string{".claude/skills/a/SKILL.md": "x"})
	if err := os.Symlink(skillDir(t), filepath.Join(home, ".claude", "skills", "a", "deep")); err != nil {
		t.Fatal(err)
	}
	if _, err := (claude.Adapter{}).SnapshotConfig(t.Context(), h, harness.SnapshotOptions{}); err == nil {
		t.Error("a link at depth 2 was followed")
	}
}

func TestSnapshotHasASizeLimitThatCountsLinkedFiles(t *testing.T) {
	h, home := newHost(t, nil)
	dir := skillDir(t)
	if err := os.WriteFile(filepath.Join(dir, "big.bin"), make([]byte, 17<<20), 0o600); err != nil {
		t.Fatal(err)
	}
	linkedSkill(t, home, "big", dir)
	if _, err := (claude.Adapter{}).SnapshotConfig(t.Context(), h, harness.SnapshotOptions{}); err == nil || !strings.Contains(err.Error(), "too big") {
		t.Errorf("err = %v", err)
	}
	// The same bytes in a real file are counted too.
	h2, _ := newHost(t, map[string]string{".claude/commands/big.md": strings.Repeat("x", 17<<20)})
	if _, err := (claude.Adapter{}).SnapshotConfig(t.Context(), h2, harness.SnapshotOptions{}); err == nil {
		t.Error("a real file over the limit was taken")
	}
	// And so are too many files.
	files := map[string]string{}
	for i := range 2001 {
		files[fmt.Sprintf(".claude/agents/a%d.md", i)] = "x"
	}
	h3, _ := newHost(t, files)
	if _, err := (claude.Adapter{}).SnapshotConfig(t.Context(), h3, harness.SnapshotOptions{}); err == nil {
		t.Error("too many files were taken")
	}
}

func TestSnapshotLimitCoversTheWholeSnapshot(t *testing.T) {
	half := strings.Repeat("x", 9<<20)
	h, _ := newHost(t, map[string]string{".claude/commands/a.md": half, ".claude/agents/b.md": half})
	if _, err := (claude.Adapter{}).SnapshotConfig(t.Context(), h, harness.SnapshotOptions{}); err == nil || !strings.Contains(err.Error(), "too big") {
		t.Errorf("two components of 9 MiB: %v", err)
	}
}

// A link target swapped for a link to a private directory while the snapshot
// is taken is never followed: the directory opened is the one that was checked.
func TestSnapshotDoesNotFollowATargetSwappedForALink(t *testing.T) {
	h, home := newHost(t, nil)
	parent := t.TempDir()
	target := filepath.Join(parent, "pkg")
	good := filepath.Join(parent, "good")
	if err := os.MkdirAll(good, 0o755); err != nil { //nolint:gosec // a directory of plain files
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(good, "SKILL.md"), []byte("# ok"), 0o600); err != nil {
		t.Fatal(err)
	}
	private := filepath.Join(t.TempDir(), "dotssh")
	if err := os.MkdirAll(private, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(private, "id_ed25519"), []byte("PRIVATE KEY"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(good, target); err != nil {
		t.Fatal(err)
	}
	linkedSkill(t, home, "tdd", target)
	var stop atomic.Bool
	done := make(chan struct{})
	go func() {
		defer close(done)
		link := filepath.Join(parent, "evil")
		for !stop.Load() {
			_ = os.Symlink(private, link)
			_ = os.Rename(target, good)
			_ = os.Rename(link, target) // the target is a link to private
			_ = os.Rename(target, link)
			_ = os.Rename(good, target) // and a directory again
			_ = os.Remove(link)
		}
	}()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		snap, err := claude.Adapter{}.SnapshotConfig(t.Context(), h, harness.SnapshotOptions{Components: []string{"skills"}})
		if err != nil || len(snap.Components) == 0 {
			continue
		}
		for _, f := range snap.Components[0].Files {
			if strings.Contains(string(f.Content), "PRIVATE KEY") {
				stop.Store(true)
				<-done
				t.Fatalf("followed a swapped link: %s", f.Path)
			}
		}
	}
	stop.Store(true)
	<-done
}

func TestApplyConfigWritesRealFilesForALinkedSkill(t *testing.T) {
	h, home := newHost(t, nil)
	linkedSkill(t, home, "tdd", skillDir(t))
	snap, err := claude.Adapter{}.SnapshotConfig(t.Context(), h, harness.SnapshotOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if v, err := (claude.Adapter{}).ValidateConfig(t.Context(), h); err != nil || len(v.Issues) > 0 {
		t.Fatalf("validate: %+v %v", v, err)
	}
	dir, _ := applyRun(t, h, snap, false)
	for _, p := range []string{"skills/tdd/SKILL.md", "skills/tdd/scripts/run.sh"} {
		fi, err := os.Lstat(filepath.Join(dir, p))
		if err != nil || !fi.Mode().IsRegular() {
			t.Errorf("%s: %v %v", p, fi, err)
		}
	}
}

// A real intermediate link of the user, in a directory the user and root own,
// leads to a directory of the user: followed, as the kernel would.
func TestSnapshotFollowsAnIntermediateLinkOfTheUser(t *testing.T) {
	h, home := newHost(t, nil)
	base := t.TempDir()
	dir := filepath.Join(base, "releases", "v1", "tdd")
	if err := os.MkdirAll(dir, 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "SKILL.md"), []byte("# tdd"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(base, "releases", "v1"), filepath.Join(base, "current")); err != nil {
		t.Fatal(err)
	}
	linkedSkill(t, home, "tdd", filepath.Join(base, "current", "tdd"))
	files := skillsOf(t, h)
	if len(files) != 1 || files[0].Path != "skills/tdd/SKILL.md" {
		t.Errorf("files = %+v", files)
	}
}

// Links that loop are refused, not followed forever.
func TestSnapshotRefusesALoopOfLinks(t *testing.T) {
	h, home := newHost(t, nil)
	base := t.TempDir()
	_ = os.Symlink(filepath.Join(base, "b"), filepath.Join(base, "a"))
	_ = os.Symlink(filepath.Join(base, "a"), filepath.Join(base, "b"))
	linkedSkill(t, home, "x", filepath.Join(base, "a", "x"))
	if _, err := (claude.Adapter{}).SnapshotConfig(t.Context(), h, harness.SnapshotOptions{}); err == nil {
		t.Error("a loop of links was followed")
	}
}
