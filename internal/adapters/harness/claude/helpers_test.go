package claude_test

import (
	"crypto/sha256"
	"encoding/hex"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/Autarquico/codefabric/internal/adapters/harness"
	"github.com/Autarquico/codefabric/internal/adapters/harness/localhost"
)

// The values that must never reach a snapshot or a Run configuration.
const (
	loginToken  = "sk-ant-oat01-login-sentinel" //nolint:gosec // a sentinel of the test
	envSecret   = "env-sentinel-AAAA"           //nolint:gosec // a sentinel of the test
	headerValue = "Bearer header-sentinel-BBBB"
	mcpEnvValue = "mcp-env-sentinel-CCCC"
	hookHeader  = "hook-sentinel-DDDD"
)

// homeFiles is a ~/ of a person who has used Claude Code: the adoptable
// configuration, the state of the CLI, and what is never adoptable.
func homeFiles() map[string]string {
	return map[string]string{
		".claude/settings.json": `{
  "model": "opus",
  "env": {"ANTHROPIC_AUTH_TOKEN": "` + envSecret + `", "EMPTY": ""},
  "apiKeyHelper": "~/bin/print-key",
  "hooks": {"PostToolUse": [{"matcher": "Edit", "hooks": [
    {"type": "http", "url": "https://hooks.example/x", "headers": {"Authorization": "` + hookHeader + `"}},
    {"type": "command", "command": "echo done"}]}]},
  "cleanupPeriodDays": 30
}`,
		".claude/CLAUDE.md":                 "Always answer in French.\n",
		".claude/commands/review.md":        "Review $ARGUMENTS\n",
		".claude/commands/team/ship.md":     "Ship it\n",
		".claude/agents/planner.md":         "---\nname: planner\n---\nPlan.\n",
		".claude/skills/tdd/SKILL.md":       "---\nname: tdd\n---\n",
		".claude/skills/tdd/scripts/run.sh": "#!/bin/sh\necho tdd\n",
		".claude/output-styles/terse.md":    "Be terse.\n",
		".claude/.credentials.json":         `{"claudeAiOauth": {"accessToken": "` + loginToken + `", "refreshToken": "r-` + loginToken + `"}}`,
		".claude/projects/-tmp-x/s1.jsonl":  `{"type":"user","message":"mine"}` + "\n",
		".claude/todos/t.json":              `[]`,
		".claude/statsig/cache.json":        `{}`,
		".claude/history.jsonl":             `{"display":"a prompt"}` + "\n",
		".claude/sessions/s.json":           `{}`,
		".claude.json": `{
  "numStartups": 12,
  "userID": "user-sentinel",
  "oauthAccount": {"emailAddress": "me@example.com"},
  "projects": {"/work/x": {"allowedTools": []}},
  "mcpServers": {
    "stdio": {"type": "stdio", "command": "npx", "args": ["-y", "srv"], "env": {"TOKEN": "` + mcpEnvValue + `"}},
    "remote": {"type": "http", "url": "https://mcp.example/x", "headers": {"Authorization": "` + headerValue + `"}}
  }
}`,
	}
}

// seedHome writes files under home, 0600 as Claude Code does.
func seedHome(t *testing.T, home string, files map[string]string) {
	t.Helper()
	for rel, content := range files {
		p := filepath.Join(home, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
}

// newHost is a host with a seeded home and no binary.
func newHost(t *testing.T, files map[string]string) (*localhost.Host, string) {
	t.Helper()
	home := t.TempDir()
	seedHome(t, home, files)
	h, err := localhost.New(localhost.Options{Home: home, Path: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	return h, home
}

// treeHash is a digest of every entry under root: path, mode, and content or
// link target. Two equal hashes are two byte-identical trees.
func treeHash(t *testing.T, root string) string {
	t.Helper()
	h := sha256.New()
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		h.Write([]byte(p + "|" + info.Mode().String() + "|"))
		switch {
		case d.Type()&fs.ModeSymlink != 0:
			target, _ := os.Readlink(p)
			h.Write([]byte(target))
		case d.Type().IsRegular():
			b, err := os.ReadFile(p) //nolint:gosec // a test tree
			if err != nil {
				return err
			}
			h.Write(b)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return hex.EncodeToString(h.Sum(nil))
}

func component(t *testing.T, s harness.NativeSnapshot, name string) harness.Component {
	t.Helper()
	i := slices.IndexFunc(s.Components, func(c harness.Component) bool { return c.Name == name })
	if i < 0 {
		t.Fatalf("the snapshot has no component %q (it has %v)", name, names(s))
	}
	return s.Components[i]
}

func names(s harness.NativeSnapshot) []string {
	var out []string
	for _, c := range s.Components {
		out = append(out, c.Name)
	}
	return out
}

func read(t *testing.T, p string) string {
	t.Helper()
	b, err := os.ReadFile(p) //nolint:gosec // a test tree
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// assertNoSecrets fails if any file under root (or any component) carries one
// of the sentinel values.
func assertNoSecretValues(t *testing.T, what, content string) {
	t.Helper()
	for _, s := range []string{loginToken, envSecret, headerValue, mcpEnvValue, hookHeader, "user-sentinel", "me@example.com"} {
		if strings.Contains(content, s) {
			t.Errorf("%s carries %q", what, s)
		}
	}
}
