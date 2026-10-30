package claude_test

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/Autarquico/codefabric/internal/adapters/harness"
	"github.com/Autarquico/codefabric/internal/adapters/harness/claude"
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
