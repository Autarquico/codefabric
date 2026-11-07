package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// A fake claude: logs its arguments and the directory it ran in (to
// $FAKE_LOG.pwd); `remove` of a server that is not there
// fails the way the real one does.
const fakeClaude = `#!/bin/sh
echo "claude $*" >> "$FAKE_LOG"
pwd -P >> "$FAKE_LOG.pwd"
case "$2" in
add) /usr/bin/touch "$FAKE_STATE/claude-registered" ;;
remove)
	if [ -e "$FAKE_STATE/claude-registered" ]; then /bin/rm "$FAKE_STATE/claude-registered"
	else echo 'No MCP server "cf" in user scope'; exit 1; fi ;;
esac
`

// A fake codex 0.160.1: `add` rewrites the [mcp_servers.cf] table of
// $CODEX_HOME/config.toml with command and args, and so drops any
// tool_timeout_sec; `remove` deletes the table. The table goes before the
// first [projects.*] table, in the middle of the file, as in a config that
// has trusted projects. Other tables stay.
const fakeCodex = `#!/bin/sh
echo "codex $*" >> "$FAKE_LOG"
cfg="$CODEX_HOME/config.toml"
drop() {
	[ -e "$cfg" ] || return 0
	/usr/bin/awk '/^\[/ { skip = ($0 == "[mcp_servers.cf]") } !skip' "$cfg" > "$cfg.new" && /bin/mv "$cfg.new" "$cfg"
}
case "$2" in
remove) drop ;;
add)
	drop
	shift 4
	cmd="$1"
	shift
	args=""
	for a in "$@"; do args="$args${args:+, }\"$a\""; done
	[ -e "$cfg" ] || : > "$cfg"
	printf '[mcp_servers.cf]\ncommand = "%s"\nargs = [%s]\n' "$cmd" "$args" > "$cfg.tbl"
	/usr/bin/awk -v f="$cfg.tbl" 'function emit(  l) { while ((getline l < f) > 0) print l; print "" }
		/^\[projects\./ && !done { emit(); done = 1 }
		{ print }
		END { if (!done) emit() }' "$cfg" > "$cfg.new" && /bin/mv "$cfg.new" "$cfg" && /bin/rm "$cfg.tbl"
	;;
esac
`

type mcpMachine struct {
	home, codexHome, log string
}

// newMCPMachine isolates HOME, CODEX_HOME and PATH, with a fake claude and codex in PATH.
func newMCPMachine(t *testing.T) mcpMachine {
	t.Helper()
	m := mcpMachine{home: t.TempDir(), codexHome: filepath.Join(t.TempDir(), "codex"), log: filepath.Join(t.TempDir(), "calls.log")}
	bin, state := t.TempDir(), t.TempDir()
	for name, script := range map[string]string{"claude": fakeClaude, "codex": fakeCodex} {
		if err := os.WriteFile(filepath.Join(bin, name), []byte(script), 0o755); err != nil { //nolint:gosec // a test double that must run
			t.Fatal(err)
		}
	}
	if err := os.MkdirAll(m.codexHome, 0o755); err != nil { //nolint:gosec // a test directory
		t.Fatal(err)
	}
	t.Setenv("HOME", m.home)
	t.Setenv("CODEX_HOME", m.codexHome)
	t.Setenv("PATH", bin)
	t.Setenv("FAKE_LOG", m.log)
	t.Setenv("FAKE_STATE", state)
	return m
}

func (m mcpMachine) calls(t *testing.T) []string {
	t.Helper()
	data, err := os.ReadFile(m.log)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		t.Fatal(err)
	}
	return strings.Split(strings.TrimSpace(string(data)), "\n")
}

func (m mcpMachine) skill(tool string) string {
	return filepath.Join(m.home, map[string]string{"claude": ".claude", "codex": ".agents"}[tool], "skills", "codefabric", "SKILL.md")
}

func (m mcpMachine) codexConfig() string { return filepath.Join(m.codexHome, "config.toml") }

func readFile(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path) //nolint:gosec // a path of the test
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func requireEqual(t *testing.T, got, want any) {
	t.Helper()
	if !reflectEqual(got, want) {
		t.Fatalf("got %v, want %v", got, want)
	}
}

func reflectEqual(a, b any) bool {
	switch x := a.(type) {
	case []string:
		y, ok := b.([]string)
		return ok && strings.Join(x, "\x00") == strings.Join(y, "\x00")
	}
	return a == b
}

func selfBinary(t *testing.T) string {
	t.Helper()
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	return self
}

func TestMCPInstall_ClaudeRunsRemoveThenAddWithScopeAndWritesTheSkill(t *testing.T) {
	m := newMCPMachine(t)
	code, stdout, stderr := run(t, "mcp", "install", "--claude")
	if code != 0 {
		t.Fatalf("exit %d: %s%s", code, stdout, stderr)
	}
	requireEqual(t, m.calls(t), []string{
		"claude mcp remove --scope user cf",
		"claude mcp add --scope user cf -- " + selfBinary(t) + " mcp serve --profile execute",
	})
	requireEqual(t, readFile(t, m.skill("claude")), string(codefabricSkill))
	info, err := os.Stat(m.skill("claude"))
	if err != nil {
		t.Fatal(err)
	}
	requireEqual(t, info.Mode().Perm(), os.FileMode(0o644))
	dir, err := os.Stat(filepath.Dir(m.skill("claude")))
	if err != nil {
		t.Fatal(err)
	}
	requireEqual(t, dir.Mode().Perm(), os.FileMode(0o755))
}

func TestMCPInstall_TwiceLeavesTheSameStateAndDoesNotRewriteTheSkill(t *testing.T) {
	for _, tool := range []string{"claude", "codex"} {
		t.Run(tool, func(t *testing.T) {
			m := newMCPMachine(t)
			if code, out, errOut := run(t, "mcp", "install", "--"+tool); code != 0 {
				t.Fatalf("exit %d: %s%s", code, out, errOut)
			}
			past := time.Now().Add(-time.Hour).Truncate(time.Second)
			if err := os.Chtimes(m.skill(tool), past, past); err != nil {
				t.Fatal(err)
			}
			cfg := ""
			if tool == "codex" {
				cfg = readFile(t, m.codexConfig())
			}
			code, stdout, _ := run(t, "mcp", "install", "--"+tool, "--json")
			if code != 0 {
				t.Fatalf("second install exit %d: %s", code, stdout)
			}
			info, err := os.Stat(m.skill(tool))
			if err != nil {
				t.Fatal(err)
			}
			if !info.ModTime().Equal(past) {
				t.Errorf("the skill was rewritten: mtime %v, want %v", info.ModTime(), past)
			}
			if !strings.Contains(stdout, `"state":"unchanged"`) {
				t.Errorf("the report does not say unchanged: %s", stdout)
			}
			if tool == "codex" {
				requireEqual(t, readFile(t, m.codexConfig()), cfg)
				requireEqual(t, strings.Count(cfg, "[mcp_servers.cf]"), 1)
				requireEqual(t, strings.Count(cfg, "tool_timeout_sec"), 1)
			}
		})
	}
}

func TestMCPInstall_CodexAddsTheToolTimeoutAndLeavesOtherTablesAlone(t *testing.T) {
	m := newMCPMachine(t)
	head := "model = \"gpt-5\"\n\n[mcp_servers.other]\ncommand = \"x\"\ntool_timeout_sec = 5\n\n"
	projects := "[projects.\"/Users/a b/ç\"]\ntrust_level = \"trusted\"\n"
	if err := os.WriteFile(m.codexConfig(), []byte(head+projects), 0o600); err != nil {
		t.Fatal(err)
	}
	if code, out, errOut := run(t, "mcp", "install", "--codex", "--profile", "read"); code != 0 {
		t.Fatalf("exit %d: %s%s", code, out, errOut)
	}
	requireEqual(t, m.calls(t), []string{
		"codex mcp remove cf",
		"codex mcp add cf -- " + selfBinary(t) + " mcp serve --profile read",
	})
	cfg := readFile(t, m.codexConfig())
	if !strings.HasPrefix(cfg, head) || !strings.HasSuffix(cfg, "\n"+projects) {
		t.Errorf("the other tables changed:\n%s", cfg)
	}
	table := cfg[strings.Index(cfg, "[mcp_servers.cf]"):strings.Index(cfg, "[projects.")]
	if !strings.Contains(table, "tool_timeout_sec = 660\n") {
		t.Errorf("no timeout in the cf table:\n%s", cfg)
	}
	requireEqual(t, strings.Count(cfg, "tool_timeout_sec"), 2)
	requireEqual(t, readFile(t, m.skill("codex")), string(codefabricSkill))
}

func TestSetCodexToolTimeout_KeepsTheModeAndRefusesASymlink(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.toml")
	if err := os.WriteFile(path, []byte("[mcp_servers.cf]\ncommand = \"cf\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if changed, err := setCodexToolTimeout(path, 660); err != nil || !changed {
		t.Fatalf("changed=%v err=%v", changed, err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	requireEqual(t, info.Mode().Perm(), os.FileMode(0o600))
	link := filepath.Join(dir, "link.toml")
	if err := os.Symlink(path, link); err != nil {
		t.Fatal(err)
	}
	if _, err := setCodexToolTimeout(link, 120); err == nil {
		t.Error("edited a symlinked config")
	}
	requireEqual(t, strings.Contains(readFile(t, path), "660"), true)
}

func TestMCPInstall_CodexProjectScopePutsTheSkillInTheRepoAndKeepsTheServerGlobal(t *testing.T) {
	m := newMCPMachine(t)
	repo := t.TempDir()
	if err := os.MkdirAll(filepath.Join(repo, ".git"), 0o755); err != nil { //nolint:gosec // a test directory
		t.Fatal(err)
	}
	sub := filepath.Join(repo, "pkg", "deep")
	if err := os.MkdirAll(sub, 0o755); err != nil { //nolint:gosec // a test directory
		t.Fatal(err)
	}
	t.Chdir(sub)
	repoReal, err := os.Getwd() // t.TempDir may sit behind a symlink (macOS /var)
	if err != nil {
		t.Fatal(err)
	}
	repoReal = strings.TrimSuffix(repoReal, "/pkg/deep")
	code, stdout, stderr := run(t, "mcp", "install", "--codex", "--scope", "project")
	if code != 0 {
		t.Fatalf("exit %d: %s%s", code, stdout, stderr)
	}
	if !strings.Contains(stdout, "no project scope") {
		t.Errorf("the output does not say the server stays global: %s", stdout)
	}
	readFile(t, filepath.Join(repoReal, ".agents", "skills", "codefabric", "SKILL.md"))
	if _, err := os.Stat(m.skill("codex")); !os.IsNotExist(err) {
		t.Errorf("the skill also went to the home: %v", err)
	}
}

func TestMCPInstall_ProjectScopeOutsideARepoIsAUsageError(t *testing.T) {
	newMCPMachine(t)
	t.Chdir(t.TempDir())
	code, stdout, _ := run(t, "mcp", "install", "--claude", "--scope", "project", "--json")
	requireEqual(t, code, 2)
	requireEqual(t, errorOf(t, stdout)["code"], "CF_INVALID_REQUEST")
}

func TestMCPInstall_RefusesToWriteThroughASymlink(t *testing.T) {
	t.Run("directory", func(t *testing.T) {
		m := newMCPMachine(t)
		target := t.TempDir()
		link := filepath.Dir(m.skill("claude"))
		if err := os.MkdirAll(filepath.Dir(link), 0o755); err != nil { //nolint:gosec // a test directory
			t.Fatal(err)
		}
		if err := os.Symlink(target, link); err != nil {
			t.Fatal(err)
		}
		if code, _, _ := run(t, "mcp", "install", "--claude"); code == 0 {
			t.Fatal("install through a symlinked directory succeeded")
		}
		entries, _ := os.ReadDir(target)
		requireEqual(t, len(entries), 0)
	})
	t.Run("file", func(t *testing.T) {
		m := newMCPMachine(t)
		dir := filepath.Dir(m.skill("codex"))
		if err := os.MkdirAll(dir, 0o755); err != nil { //nolint:gosec // a test directory
			t.Fatal(err)
		}
		target := filepath.Join(t.TempDir(), "elsewhere.md")
		if err := os.WriteFile(target, []byte("mine"), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(target, m.skill("codex")); err != nil {
			t.Fatal(err)
		}
		if code, _, _ := run(t, "mcp", "install", "--codex"); code == 0 {
			t.Fatal("install through a symlinked file succeeded")
		}
		requireEqual(t, readFile(t, target), "mine")
		if code, _, _ := run(t, "mcp", "uninstall", "--codex"); code == 0 {
			t.Fatal("uninstall through a symlinked file succeeded")
		}
		requireEqual(t, readFile(t, target), "mine")
	})
}

func TestMCPInstall_DryRunPrintsAndDoesNothing(t *testing.T) {
	for _, tool := range []string{"claude", "codex"} {
		t.Run(tool, func(t *testing.T) {
			m := newMCPMachine(t)
			code, stdout, stderr := run(t, "mcp", "install", "--"+tool, "--dry-run")
			if code != 0 {
				t.Fatalf("exit %d: %s%s", code, stdout, stderr)
			}
			for _, want := range []string{tool + " mcp remove", tool + " mcp add", "--profile execute", m.skill(tool)} {
				if !strings.Contains(stdout, want) {
					t.Errorf("dry run output lacks %q:\n%s", want, stdout)
				}
			}
			if tool == "codex" && !strings.Contains(stdout, "tool_timeout_sec = 660") {
				t.Errorf("dry run does not show the TOML edit:\n%s", stdout)
			}
			requireEqual(t, len(m.calls(t)), 0)
			for _, path := range []string{m.skill(tool), m.codexConfig()} {
				if _, err := os.Stat(path); !os.IsNotExist(err) {
					t.Errorf("%s exists after a dry run: %v", path, err)
				}
			}
		})
	}
}

func TestMCPInstall_ACLIMissingFromPATHIsNotFoundWithAHint(t *testing.T) {
	m := newMCPMachine(t)
	t.Setenv("PATH", t.TempDir())
	code, stdout, _ := run(t, "mcp", "install", "--codex", "--json")
	requireEqual(t, code, 3)
	env := errorOf(t, stdout)
	requireEqual(t, env["code"], "CF_NOT_FOUND")
	if msg, _ := env["message"].(string); !strings.Contains(msg, "npm install") {
		t.Errorf("no install hint: %q", msg)
	}
	if _, err := os.Stat(m.skill("codex")); !os.IsNotExist(err) {
		t.Errorf("the skill was written without the CLI: %v", err)
	}
}

func TestMCPInstall_CodexFailsClearlyWhenAddDidNotWriteTheTable(t *testing.T) {
	m := newMCPMachine(t)
	bin := t.TempDir()
	if err := os.WriteFile(filepath.Join(bin, "codex"), []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil { //nolint:gosec // a test double that must run
		t.Fatal(err)
	}
	t.Setenv("PATH", bin)
	if err := os.WriteFile(m.codexConfig(), []byte("model = \"x\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	code, _, stderr := run(t, "mcp", "install", "--codex")
	if code == 0 || !strings.Contains(stderr, "[mcp_servers.cf]") {
		t.Fatalf("exit %d, stderr %q", code, stderr)
	}
}

func TestMCPInstall_RejectsBadFlags(t *testing.T) {
	newMCPMachine(t)
	for _, args := range [][]string{
		{"mcp", "install"},
		{"mcp", "install", "--claude", "--codex"},
		{"mcp", "install", "--claude", "--profile", "root"},
		{"mcp", "install", "--claude", "--scope", "local"},
		{"mcp", "uninstall"},
	} {
		if code, _, _ := run(t, args...); code != 2 {
			t.Errorf("%v: exit %d, want 2", args, code)
		}
	}
}

func TestMCPUninstall_LeavesTheMachineAsItWas(t *testing.T) {
	for _, tool := range []string{"claude", "codex"} {
		t.Run(tool, func(t *testing.T) {
			m := newMCPMachine(t)
			if code, out, errOut := run(t, "mcp", "install", "--"+tool); code != 0 {
				t.Fatalf("install: exit %d: %s%s", code, out, errOut)
			}
			if code, out, errOut := run(t, "mcp", "uninstall", "--"+tool); code != 0 {
				t.Fatalf("uninstall: exit %d: %s%s", code, out, errOut)
			}
			calls := m.calls(t)
			want := tool + " mcp remove"
			if tool == "claude" {
				want += " --scope user"
			}
			requireEqual(t, calls[len(calls)-1], want+" cf")
			if _, err := os.Stat(filepath.Dir(m.skill(tool))); !os.IsNotExist(err) {
				t.Errorf("the skill directory is still there: %v", err)
			}
			if tool == "codex" {
				requireEqual(t, strings.Contains(readFile(t, m.codexConfig()), "mcp_servers.cf"), false)
			}
			if code, _, _ := run(t, "mcp", "uninstall", "--"+tool); code != 0 {
				t.Errorf("a second uninstall failed with exit %d", code)
			}
		})
	}
}

func TestMCPUninstall_KeepsADirectoryThatHoldsOtherFiles(t *testing.T) {
	m := newMCPMachine(t)
	if code, _, _ := run(t, "mcp", "install", "--claude"); code != 0 {
		t.Fatal("install failed")
	}
	mine := filepath.Join(filepath.Dir(m.skill("claude")), "notes.md")
	if err := os.WriteFile(mine, []byte("mine"), 0o600); err != nil {
		t.Fatal(err)
	}
	if code, _, _ := run(t, "mcp", "uninstall", "--claude"); code != 0 {
		t.Fatal("uninstall failed")
	}
	requireEqual(t, readFile(t, mine), "mine")
	if _, err := os.Stat(m.skill("claude")); !os.IsNotExist(err) {
		t.Errorf("SKILL.md is still there: %v", err)
	}
}

func TestWithToolTimeout_EditsOnlyTheCfTable(t *testing.T) {
	cases := []struct{ name, in, want string }{
		{
			"inserts after the last key, before the next table",
			"[mcp_servers.cf]\ncommand = \"cf\"\n\n[mcp_servers.cf.env]\nA = \"1\"\n",
			"[mcp_servers.cf]\ncommand = \"cf\"\ntool_timeout_sec = 660\n\n[mcp_servers.cf.env]\nA = \"1\"\n",
		},
		{
			"replaces another value",
			"[mcp_servers.cf]\ntool_timeout_sec = 60\ncommand = \"cf\"\n[other]\ntool_timeout_sec = 60\n",
			"[mcp_servers.cf]\ntool_timeout_sec = 660\ncommand = \"cf\"\n[other]\ntool_timeout_sec = 60\n",
		},
		{
			"table at the end without a newline",
			"[mcp_servers.cf]\ncommand = \"cf\"",
			"[mcp_servers.cf]\ncommand = \"cf\"\ntool_timeout_sec = 660",
		},
		{"already set", "[mcp_servers.cf]\ntool_timeout_sec = 660\n", "[mcp_servers.cf]\ntool_timeout_sec = 660\n"},
		{
			"before a table whose name has a path with spaces",
			"[mcp_servers.cf]\ncommand = \"cf\"\n\n[projects.\"/Users/a b/c\"]\ntrust_level = \"trusted\"\n",
			"[mcp_servers.cf]\ncommand = \"cf\"\ntool_timeout_sec = 660\n\n[projects.\"/Users/a b/c\"]\ntrust_level = \"trusted\"\n",
		},
		{
			"before tables with ':', '@', '~' and non-ASCII names, quoted keys and an array table",
			"[mcp_servers.cf]\ncommand = \"cf\"\n[a:b]\nx = 1\n[\"é@~\"]\n[[tools.ç]]\n",
			"[mcp_servers.cf]\ncommand = \"cf\"\ntool_timeout_sec = 660\n[a:b]\nx = 1\n[\"é@~\"]\n[[tools.ç]]\n",
		},
		{
			"a header followed by a comment",
			"[mcp_servers.cf]  # mine\ncommand = \"cf\"\n[projects.\"/p\"] # trusted\nk = 1\n",
			"[mcp_servers.cf]  # mine\ncommand = \"cf\"\ntool_timeout_sec = 660\n[projects.\"/p\"] # trusted\nk = 1\n",
		},
		{
			"subtables end the cf table",
			"[mcp_servers.cf]\ncommand = \"cf\"\n[mcp_servers.cf.env]\nA = \"1\"\n[projects.\"/p\"]\n",
			"[mcp_servers.cf]\ncommand = \"cf\"\ntool_timeout_sec = 660\n[mcp_servers.cf.env]\nA = \"1\"\n[projects.\"/p\"]\n",
		},
		{
			"the comment that introduces the next table stays with it",
			"[mcp_servers.cf]\ncommand = \"cf\"\n\n# notes for other\n[other]\n",
			"[mcp_servers.cf]\ncommand = \"cf\"\ntool_timeout_sec = 660\n\n# notes for other\n[other]\n",
		},
		{
			"a multi-line array whose lines look like headers is part of the cf table",
			"[mcp_servers.cf]\nargs = [\n  [\"a\"],\n  [\"b\"], # c\n]\n[projects.\"/p\"]\n",
			"[mcp_servers.cf]\nargs = [\n  [\"a\"],\n  [\"b\"], # c\n]\ntool_timeout_sec = 660\n[projects.\"/p\"]\n",
		},
		{
			"a multi-line string whose lines look like headers is part of the cf table",
			"[mcp_servers.cf]\nnote = \"\"\"\n[not a table]\n\"\"\"\n[projects.\"/p\"]\n",
			"[mcp_servers.cf]\nnote = \"\"\"\n[not a table]\n\"\"\"\ntool_timeout_sec = 660\n[projects.\"/p\"]\n",
		},
		{
			"a [mcp_servers.cf] line inside a multi-line string is not the table",
			"note = \"\"\"\n[mcp_servers.cf]\n\"\"\"\n[mcp_servers.cf]\ncommand = \"cf\"\n",
			"note = \"\"\"\n[mcp_servers.cf]\n\"\"\"\n[mcp_servers.cf]\ncommand = \"cf\"\ntool_timeout_sec = 660\n",
		},
		{
			"CRLF keeps its line ending",
			"[mcp_servers.cf]\r\ncommand = \"cf\"\r\n\r\n[projects.\"/p\"]\r\n",
			"[mcp_servers.cf]\r\ncommand = \"cf\"\r\ntool_timeout_sec = 660\r\n\r\n[projects.\"/p\"]\r\n",
		},
		{
			"CRLF replacing a value",
			"[mcp_servers.cf]\r\ntool_timeout_sec = 60\r\n",
			"[mcp_servers.cf]\r\ntool_timeout_sec = 660\r\n",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := withToolTimeout(c.in, 660)
			if err != nil {
				t.Fatal(err)
			}
			requireEqual(t, got, c.want)
		})
	}
	if _, err := withToolTimeout("[mcp_servers.other]\ncommand = \"x\"\n", 660); err == nil {
		t.Error("a config without the cf table was accepted")
	}
}

func TestCfBinary_PrefersThePATHEntryOnlyWhenItIsTheRunningBinary(t *testing.T) {
	self := selfBinary(t)
	bin := t.TempDir()
	link := filepath.Join(bin, "cf")
	if err := os.Symlink(self, link); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin)
	got, err := cfBinary()
	if err != nil {
		t.Fatal(err)
	}
	requireEqual(t, got, link)

	other := t.TempDir()
	if err := os.WriteFile(filepath.Join(other, "cf"), []byte("#!/bin/sh\n"), 0o755); err != nil { //nolint:gosec // a test double
		t.Fatal(err)
	}
	t.Setenv("PATH", other)
	if got, err = cfBinary(); err != nil {
		t.Fatal(err)
	}
	requireEqual(t, got, self)
}

func TestMCPInstall_ClaudeProjectScopeRunsTheCLIInTheRepoRootFromASubdirectory(t *testing.T) {
	for _, verb := range []string{"install", "uninstall"} {
		t.Run(verb, func(t *testing.T) {
			m := newMCPMachine(t)
			repo := t.TempDir()
			sub := filepath.Join(repo, "pkg", "deep")
			if err := os.MkdirAll(filepath.Join(repo, ".git"), 0o755); err != nil { //nolint:gosec // a test directory
				t.Fatal(err)
			}
			if err := os.MkdirAll(sub, 0o755); err != nil { //nolint:gosec // a test directory
				t.Fatal(err)
			}
			t.Chdir(sub)
			root, err := filepath.EvalSymlinks(repo) // the fake prints the physical path; t.TempDir may sit behind a symlink (macOS /var)
			if err != nil {
				t.Fatal(err)
			}
			if code, out, errOut := run(t, "mcp", verb, "--claude", "--scope", "project"); code != 0 {
				t.Fatalf("exit %d: %s%s", code, out, errOut)
			}
			dirs := strings.Split(strings.TrimSpace(readFile(t, m.log+".pwd")), "\n")
			want := []string{root, root}
			if verb == "uninstall" {
				want = want[:1]
			}
			requireEqual(t, dirs, want)
		})
	}
}

func TestMCPUninstall_CodexProjectScopeLeavesTheUserServerAndSaysSo(t *testing.T) {
	m := newMCPMachine(t)
	repo := t.TempDir()
	if err := os.MkdirAll(filepath.Join(repo, ".git"), 0o755); err != nil { //nolint:gosec // a test directory
		t.Fatal(err)
	}
	t.Chdir(repo)
	if code, out, errOut := run(t, "mcp", "install", "--codex"); code != 0 {
		t.Fatalf("install: exit %d: %s%s", code, out, errOut)
	}
	if code, out, errOut := run(t, "mcp", "install", "--codex", "--scope", "project"); code != 0 {
		t.Fatalf("install project: exit %d: %s%s", code, out, errOut)
	}
	before := len(m.calls(t))
	code, stdout, errOut := run(t, "mcp", "uninstall", "--codex", "--scope", "project")
	if code != 0 {
		t.Fatalf("uninstall: exit %d: %s%s", code, stdout, errOut)
	}
	requireEqual(t, len(m.calls(t)), before)
	requireEqual(t, strings.Contains(readFile(t, m.codexConfig()), "[mcp_servers.cf]"), true)
	requireEqual(t, strings.Contains(stdout, "left alone"), true)
	if _, err := os.Stat(filepath.Join(repo, ".agents", "skills", "codefabric")); !os.IsNotExist(err) {
		t.Errorf("the project skill is still there: %v", err)
	}
}
