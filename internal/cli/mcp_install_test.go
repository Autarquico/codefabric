package cli

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Autarquico/codeflock/internal/buildinfo"
)

// A fake claude: logs its arguments and the directory it ran in (to
// $FAKE_LOG.pwd), and keeps its servers where the real one does:
// ~/.claude.json (or $CLAUDE_CONFIG_DIR/.claude.json) for the user scope, .mcp.json of the cwd for the project
// scope. `remove` of a server that is not there fails the way the real one
// does. FAKE_FAIL_ADD makes the first `add` fail and FAKE_NOOP_ADD makes it
// succeed without writing anything.
const fakeClaude = `#!/bin/sh
echo "claude $*" >> "$FAKE_LOG"
pwd -P >> "$FAKE_LOG.pwd"
op="$2"; scope="$4"; name="$5"
if [ "$scope" = project ]; then out="$(pwd -P)/.mcp.json"; else out="${CLAUDE_CONFIG_DIR:-$HOME}/.claude.json"; fi
dir="$FAKE_STATE/$scope"
/bin/mkdir -p "$dir"
rebuild() {
	{
		printf '{"mcpServers":{'
		sep=""
		for f in "$dir"/*.json; do
			[ -e "$f" ] || continue
			printf '%s' "$sep"
			/bin/cat "$f"
			sep=","
		done
		printf '}}\n'
	} > "$out"
}
case "$op" in
add)
	if [ -n "$FAKE_FAIL_ADD" ] && [ ! -e "$FAKE_STATE/faulted" ]; then : > "$FAKE_STATE/faulted"; echo "interrupted" >&2; exit 1; fi
	if [ -n "$FAKE_NOOP_ADD" ] && [ ! -e "$FAKE_STATE/faulted" ]; then : > "$FAKE_STATE/faulted"; exit 0; fi
	shift 5
	env=""
	while [ "$1" = --env ]; do env="$env${env:+,}\"${2%%=*}\":\"${2#*=}\""; shift 2; done
	shift
	cmd="$1"
	shift
	args=""
	for a in "$@"; do args="$args${args:+,}\"$a\""; done
	printf '"%s":{"type":"stdio","command":"%s","args":[%s],"env":{%s}}' "$name" "$cmd" "$args" "$env" > "$dir/$name.json"
	rebuild ;;
remove)
	if [ -e "$dir/$name.json" ]; then /bin/rm "$dir/$name.json"; rebuild
	else echo "No MCP server found with name: $name"; exit 1; fi ;;
esac
`

// A fake codex 0.160.1: `add` rewrites the [mcp_servers.<name>] table of
// $CODEX_HOME/config.toml with command and args, and so drops any
// tool_timeout_sec; `remove` deletes the table. The table goes before the
// first [projects.*] table, in the middle of the file, as in a config that
// has trusted projects, and --env goes to its [mcp_servers.<name>.env] table.
// Other tables stay. FAKE_FAIL_ADD and FAKE_NOOP_ADD are as in the fake claude.
const fakeCodex = `#!/bin/sh
echo "codex $*" >> "$FAKE_LOG"
cfg="$CODEX_HOME/config.toml"
name="$3"
drop() {
	[ -e "$cfg" ] || return 0
	/usr/bin/awk -v t="[mcp_servers.$name]" -v e="[mcp_servers.$name.env]" '/^\[/ { skip = ($0 == t || $0 == e) } !skip' "$cfg" > "$cfg.new" && /bin/mv "$cfg.new" "$cfg"
}
case "$2" in
remove) drop ;;
add)
	if [ -n "$FAKE_FAIL_ADD" ] && [ ! -e "$FAKE_STATE/faulted" ]; then : > "$FAKE_STATE/faulted"; echo "interrupted" >&2; exit 1; fi
	if [ -n "$FAKE_NOOP_ADD" ] && [ ! -e "$FAKE_STATE/faulted" ]; then : > "$FAKE_STATE/faulted"; exit 0; fi
	drop
	shift 3
	env=""
	while [ "$1" = --env ]; do env="$env${2%%=*} = \"${2#*=}\"\n"; shift 2; done
	shift
	cmd="$1"
	shift
	args=""
	for a in "$@"; do args="$args${args:+, }\"$a\""; done
	[ -e "$cfg" ] || : > "$cfg"
	printf '[mcp_servers.%s]\ncommand = "%s"\nargs = [%s]\n' "$name" "$cmd" "$args" > "$cfg.tbl"
	[ -n "$env" ] && { printf '\n[mcp_servers.%s.env]\n' "$name"; printf "$env"; } >> "$cfg.tbl"
	/usr/bin/awk -v f="$cfg.tbl" 'function emit(  l) { while ((getline l < f) > 0) print l; print "" }
		/^\[projects\./ && !done { emit(); done = 1 }
		{ print }
		END { if (!done) emit() }' "$cfg" > "$cfg.new" && /bin/mv "$cfg.new" "$cfg" && /bin/rm "$cfg.tbl"
	;;
esac
`

type mcpMachine struct {
	home, codexHome, log, bin string
}

// newMCPMachine isolates HOME, CODEX_HOME and PATH, with a fake claude and codex in PATH.
func newMCPMachine(t *testing.T) mcpMachine {
	t.Helper()
	m := mcpMachine{home: t.TempDir(), codexHome: filepath.Join(t.TempDir(), "codex"), log: filepath.Join(t.TempDir(), "calls.log")}
	bin, state := t.TempDir(), t.TempDir()
	m.bin = bin
	for name, script := range map[string]string{"claude": fakeClaude, "codex": fakeCodex} {
		if err := os.WriteFile(filepath.Join(bin, name), []byte(script), 0o755); err != nil { //nolint:gosec // a test double that must run
			t.Fatal(err)
		}
	}
	if err := os.MkdirAll(m.codexHome, 0o755); err != nil { //nolint:gosec // a test directory
		t.Fatal(err)
	}
	t.Setenv("HOME", m.home)
	t.Setenv("CLAUDE_CONFIG_DIR", "")
	t.Setenv("CODEX_HOME", m.codexHome)
	t.Setenv("PATH", bin)
	t.Setenv("FAKE_LOG", m.log)
	t.Setenv("FAKE_STATE", state)
	return m
}

// seed registers a server the way the tool would, and forgets the call.
func (m mcpMachine) seed(t *testing.T, tool, scope, name, command string, args ...string) {
	t.Helper()
	m.seedEnv(t, tool, scope, name, nil, command, args...)
}

// seedEnv is seed with environment variables, as KEY=VALUE.
func (m mcpMachine) seedEnv(t *testing.T, tool, scope, name string, env []string, command string, args ...string) {
	t.Helper()
	argv := []string{"mcp", "add"}
	if tool == "claude" {
		argv = append(argv, "--scope", scope)
	}
	argv = append(argv, name)
	for _, kv := range env {
		argv = append(argv, "--env", kv)
	}
	argv = append(argv, "--", command)
	out, err := exec.CommandContext(t.Context(), filepath.Join(m.bin, tool), append(argv, args...)...).CombinedOutput() //nolint:gosec // a test double
	if err != nil {
		t.Fatalf("seeding %s: %v: %s", name, err, out)
	}
	_ = os.Remove(m.log)
	_ = os.Remove(m.log + ".pwd")
}

// registered is the servers the tool has under the user scope, by name.
func (m mcpMachine) registered(t *testing.T, tool string) map[string]mcpRegistration {
	t.Helper()
	var (
		regs map[string]mcpRegistration
		err  error
	)
	if tool == "claude" {
		regs, err = readRegistrations(claudeTool, "user", m.home)
	} else {
		regs, err = readRegistrations(codexTool, "user", "")
	}
	if err != nil {
		t.Fatal(err)
	}
	return regs
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
	return filepath.Join(m.home, map[string]string{"claude": ".claude", "codex": ".agents"}[tool], "skills", "codeflock", "SKILL.md")
}

// betaCf is a `cf` of Code Flock, as the beta.6 registered it: a file called cf
// that is this very binary.
func betaCf(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "cf")
	if err := os.Symlink(selfBinary(t), path); err != nil {
		t.Fatal(err)
	}
	return path
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

func renderedSkill(t *testing.T) string {
	t.Helper()
	out, err := renderCodeflockSkill(buildinfo.Get().Version)
	if err != nil {
		t.Fatal(err)
	}
	return string(out)
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
		"claude mcp remove --scope user cflk",
		"claude mcp add --scope user cflk -- " + selfBinary(t) + " mcp serve --profile execute",
	})
	requireEqual(t, readFile(t, m.skill("claude")), renderedSkill(t))
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
				requireEqual(t, strings.Count(cfg, "[mcp_servers.cflk]"), 1)
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
		"codex mcp remove cflk",
		"codex mcp add cflk -- " + selfBinary(t) + " mcp serve --profile read",
	})
	cfg := readFile(t, m.codexConfig())
	if !strings.HasPrefix(cfg, head) || !strings.HasSuffix(cfg, "\n"+projects) {
		t.Errorf("the other tables changed:\n%s", cfg)
	}
	table := cfg[strings.Index(cfg, "[mcp_servers.cflk]"):strings.Index(cfg, "[projects.")]
	if !strings.Contains(table, "tool_timeout_sec = 660\n") {
		t.Errorf("no timeout in the cflk table:\n%s", cfg)
	}
	requireEqual(t, strings.Count(cfg, "tool_timeout_sec"), 2)
	requireEqual(t, readFile(t, m.skill("codex")), renderedSkill(t))
}

func TestSetCodexToolTimeout_KeepsTheModeAndRefusesASymlink(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.toml")
	if err := os.WriteFile(path, []byte("[mcp_servers.cflk]\ncommand = \"cf\"\n"), 0o600); err != nil {
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
	readFile(t, filepath.Join(repoReal, ".agents", "skills", "codeflock", "SKILL.md"))
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
	if code == 0 || !strings.Contains(stderr, "[mcp_servers.cflk]") {
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
			requireEqual(t, calls[len(calls)-1], want+" cflk")
			if _, err := os.Stat(filepath.Dir(m.skill(tool))); !os.IsNotExist(err) {
				t.Errorf("the skill directory is still there: %v", err)
			}
			if tool == "codex" {
				requireEqual(t, strings.Contains(readFile(t, m.codexConfig()), "mcp_servers.cflk"), false)
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

func TestWithToolTimeout_EditsOnlyTheCflkTable(t *testing.T) {
	cases := []struct{ name, in, want string }{
		{
			"inserts after the last key, before the next table",
			"[mcp_servers.cflk]\ncommand = \"cf\"\n\n[mcp_servers.cflk.env]\nA = \"1\"\n",
			"[mcp_servers.cflk]\ncommand = \"cf\"\ntool_timeout_sec = 660\n\n[mcp_servers.cflk.env]\nA = \"1\"\n",
		},
		{
			"replaces another value",
			"[mcp_servers.cflk]\ntool_timeout_sec = 60\ncommand = \"cf\"\n[other]\ntool_timeout_sec = 60\n",
			"[mcp_servers.cflk]\ntool_timeout_sec = 660\ncommand = \"cf\"\n[other]\ntool_timeout_sec = 60\n",
		},
		{
			"table at the end without a newline",
			"[mcp_servers.cflk]\ncommand = \"cf\"",
			"[mcp_servers.cflk]\ncommand = \"cf\"\ntool_timeout_sec = 660",
		},
		{"already set", "[mcp_servers.cflk]\ntool_timeout_sec = 660\n", "[mcp_servers.cflk]\ntool_timeout_sec = 660\n"},
		{
			"before a table whose name has a path with spaces",
			"[mcp_servers.cflk]\ncommand = \"cf\"\n\n[projects.\"/Users/a b/c\"]\ntrust_level = \"trusted\"\n",
			"[mcp_servers.cflk]\ncommand = \"cf\"\ntool_timeout_sec = 660\n\n[projects.\"/Users/a b/c\"]\ntrust_level = \"trusted\"\n",
		},
		{
			"before tables with ':', '@', '~' and non-ASCII names, quoted keys and an array table",
			"[mcp_servers.cflk]\ncommand = \"cf\"\n[a:b]\nx = 1\n[\"é@~\"]\n[[tools.ç]]\n",
			"[mcp_servers.cflk]\ncommand = \"cf\"\ntool_timeout_sec = 660\n[a:b]\nx = 1\n[\"é@~\"]\n[[tools.ç]]\n",
		},
		{
			"a header followed by a comment",
			"[mcp_servers.cflk]  # mine\ncommand = \"cf\"\n[projects.\"/p\"] # trusted\nk = 1\n",
			"[mcp_servers.cflk]  # mine\ncommand = \"cf\"\ntool_timeout_sec = 660\n[projects.\"/p\"] # trusted\nk = 1\n",
		},
		{
			"subtables end the cflk table",
			"[mcp_servers.cflk]\ncommand = \"cf\"\n[mcp_servers.cflk.env]\nA = \"1\"\n[projects.\"/p\"]\n",
			"[mcp_servers.cflk]\ncommand = \"cf\"\ntool_timeout_sec = 660\n[mcp_servers.cflk.env]\nA = \"1\"\n[projects.\"/p\"]\n",
		},
		{
			"the comment that introduces the next table stays with it",
			"[mcp_servers.cflk]\ncommand = \"cf\"\n\n# notes for other\n[other]\n",
			"[mcp_servers.cflk]\ncommand = \"cf\"\ntool_timeout_sec = 660\n\n# notes for other\n[other]\n",
		},
		{
			"a multi-line array whose lines look like headers is part of the cflk table",
			"[mcp_servers.cflk]\nargs = [\n  [\"a\"],\n  [\"b\"], # c\n]\n[projects.\"/p\"]\n",
			"[mcp_servers.cflk]\nargs = [\n  [\"a\"],\n  [\"b\"], # c\n]\ntool_timeout_sec = 660\n[projects.\"/p\"]\n",
		},
		{
			"a multi-line string whose lines look like headers is part of the cflk table",
			"[mcp_servers.cflk]\nnote = \"\"\"\n[not a table]\n\"\"\"\n[projects.\"/p\"]\n",
			"[mcp_servers.cflk]\nnote = \"\"\"\n[not a table]\n\"\"\"\ntool_timeout_sec = 660\n[projects.\"/p\"]\n",
		},
		{
			"a [mcp_servers.cflk] line inside a multi-line string is not the table",
			"note = \"\"\"\n[mcp_servers.cflk]\n\"\"\"\n[mcp_servers.cflk]\ncommand = \"cf\"\n",
			"note = \"\"\"\n[mcp_servers.cflk]\n\"\"\"\n[mcp_servers.cflk]\ncommand = \"cf\"\ntool_timeout_sec = 660\n",
		},
		{
			"CRLF keeps its line ending",
			"[mcp_servers.cflk]\r\ncommand = \"cf\"\r\n\r\n[projects.\"/p\"]\r\n",
			"[mcp_servers.cflk]\r\ncommand = \"cf\"\r\ntool_timeout_sec = 660\r\n\r\n[projects.\"/p\"]\r\n",
		},
		{
			"CRLF replacing a value",
			"[mcp_servers.cflk]\r\ntool_timeout_sec = 60\r\n",
			"[mcp_servers.cflk]\r\ntool_timeout_sec = 660\r\n",
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
		t.Error("a config without the cflk table was accepted")
	}
}

func TestCflkBinary_PrefersThePATHEntryOnlyWhenItIsTheRunningBinary(t *testing.T) {
	self := selfBinary(t)
	bin := t.TempDir()
	link := filepath.Join(bin, "cflk")
	if err := os.Symlink(self, link); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin)
	got, err := cflkBinary()
	if err != nil {
		t.Fatal(err)
	}
	requireEqual(t, got, link)

	other := t.TempDir()
	if err := os.WriteFile(filepath.Join(other, "cflk"), []byte("#!/bin/sh\n"), 0o755); err != nil { //nolint:gosec // a test double
		t.Fatal(err)
	}
	t.Setenv("PATH", other)
	if got, err = cflkBinary(); err != nil {
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
			want := []string{root, root}
			if verb == "uninstall" {
				// the registration has to be there for there to be something to remove
				if code, out, errOut := run(t, "mcp", "install", "--claude", "--scope", "project"); code != 0 {
					t.Fatalf("install: exit %d: %s%s", code, out, errOut)
				}
				want = append(want, root)
			}
			if code, out, errOut := run(t, "mcp", verb, "--claude", "--scope", "project"); code != 0 {
				t.Fatalf("exit %d: %s%s", code, out, errOut)
			}
			dirs := strings.Split(strings.TrimSpace(readFile(t, m.log+".pwd")), "\n")
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
	requireEqual(t, strings.Contains(readFile(t, m.codexConfig()), "[mcp_servers.cflk]"), true)
	requireEqual(t, strings.Contains(stdout, "left alone"), true)
	if _, err := os.Stat(filepath.Join(repo, ".agents", "skills", "codeflock")); !os.IsNotExist(err) {
		t.Errorf("the project skill is still there: %v", err)
	}
}

// --- the rename: cf to cflk ---

func TestMCPInstall_ReplacesTheCfOfTheBetaOnceCflkIsRegistered(t *testing.T) {
	for _, tool := range []string{"claude", "codex"} {
		t.Run(tool, func(t *testing.T) {
			m := newMCPMachine(t)
			m.seed(t, tool, "user", "cf", betaCf(t), "mcp", "serve", "--profile", "read")
			if code, out, errOut := run(t, "mcp", "install", "--"+tool); code != 0 {
				t.Fatalf("exit %d: %s%s", code, out, errOut)
			}
			scope := ""
			if tool == "claude" {
				scope = " --scope user"
			}
			requireEqual(t, m.calls(t), []string{
				tool + " mcp remove" + scope + " cflk",
				tool + " mcp add" + scope + " cflk -- " + selfBinary(t) + " mcp serve --profile execute",
				tool + " mcp remove" + scope + " cf",
			})
			got := m.registered(t, tool)
			if _, ok := got["cf"]; ok {
				t.Error("the cf of Code Flock is still registered")
			}
			requireEqual(t, got["cflk"].Command, selfBinary(t))
		})
	}
}

func TestMCPInstall_ACfThatIsNotCodeFlockIsLeftAlone(t *testing.T) {
	foreign := map[string][]string{
		"another executable": {"/usr/bin/cf", "target", "list"},
		"another arguments":  {"/opt/cf", "mcp", "serve", "--port", "80"},
		"another name":       {"/opt/other", "mcp", "serve", "--profile", "read"},
	}
	for _, tool := range []string{"claude", "codex"} {
		for name, argv := range foreign {
			t.Run(tool+"/"+name, func(t *testing.T) {
				m := newMCPMachine(t)
				m.seed(t, tool, "user", "cf", argv[0], argv[1:]...)
				if code, out, errOut := run(t, "mcp", "install", "--"+tool); code != 0 {
					t.Fatalf("exit %d: %s%s", code, out, errOut)
				}
				got := m.registered(t, tool)
				requireEqual(t, got["cf"].Command, argv[0])
				requireEqual(t, got["cf"].Args, argv[1:])
				requireEqual(t, got["cflk"].Command, selfBinary(t))
				for _, c := range m.calls(t) {
					if strings.HasSuffix(c, " cf") {
						t.Errorf("a call touched the cf that is not Code Flock's: %s", c)
					}
				}
				if code, _, _ := run(t, "mcp", "uninstall", "--"+tool); code != 0 {
					t.Fatalf("uninstall exit %d", code)
				}
				requireEqual(t, m.registered(t, tool)["cf"].Command, argv[0])
			})
		}
	}
}

func TestMCPInstall_ACflkThatIsNotCodeFlockIsAnErrorWithoutChanges(t *testing.T) {
	for _, tool := range []string{"claude", "codex"} {
		t.Run(tool, func(t *testing.T) {
			m := newMCPMachine(t)
			m.seed(t, tool, "user", "cf", "/opt/cf", "mcp", "serve", "--profile", "read")
			m.seed(t, tool, "user", "cflk", "/usr/bin/other", "--flag")
			code, stdout, _ := run(t, "mcp", "install", "--"+tool, "--json")
			if code == 0 {
				t.Fatal("install over a cflk that is not Code Flock succeeded")
			}
			requireEqual(t, errorOf(t, stdout)["code"], "CF_INVALID_REQUEST")
			requireEqual(t, len(m.calls(t)), 0)
			got := m.registered(t, tool)
			requireEqual(t, got["cflk"].Command, "/usr/bin/other")
			requireEqual(t, got["cf"].Command, "/opt/cf")
			if _, err := os.Stat(m.skill(tool)); !os.IsNotExist(err) {
				t.Errorf("the skill was written: %v", err)
			}
			if code, _, _ := run(t, "mcp", "uninstall", "--"+tool); code != 0 {
				t.Fatalf("uninstall exit %d", code)
			}
			requireEqual(t, m.registered(t, tool)["cflk"].Command, "/usr/bin/other")
		})
	}
}

func TestMCPInstall_AnInterruptedAddLeavesTheCfAndTheNextRunFinishesTheMove(t *testing.T) {
	for _, tool := range []string{"claude", "codex"} {
		for _, fault := range []string{"FAKE_FAIL_ADD", "FAKE_NOOP_ADD"} {
			t.Run(tool+"/"+fault, func(t *testing.T) {
				m := newMCPMachine(t)
				beta := betaCf(t)
				m.seed(t, tool, "user", "cf", beta, "mcp", "serve", "--profile", "execute")
				t.Setenv(fault, "1")
				if code, _, _ := run(t, "mcp", "install", "--"+tool); code == 0 {
					t.Fatal("install succeeded although cflk was not registered")
				}
				got := m.registered(t, tool)
				requireEqual(t, got["cf"].Command, beta)
				if _, ok := got["cflk"]; ok {
					t.Error("cflk is registered after a failed add")
				}
				for _, c := range m.calls(t) {
					if strings.HasSuffix(c, " cf") {
						t.Errorf("the cf was removed before cflk was in place: %s", c)
					}
				}
				t.Setenv(fault, "")
				if code, out, errOut := run(t, "mcp", "install", "--"+tool); code != 0 {
					t.Fatalf("second install: exit %d: %s%s", code, out, errOut)
				}
				got = m.registered(t, tool)
				if _, ok := got["cf"]; ok {
					t.Error("the cf is still registered after the second install")
				}
				requireEqual(t, got["cflk"].Command, selfBinary(t))
			})
		}
	}
}

func TestMCPInstall_ProjectScopeReadsTheMCPJSONOfTheRepo(t *testing.T) {
	m := newMCPMachine(t)
	repo := t.TempDir()
	if err := os.MkdirAll(filepath.Join(repo, ".git"), 0o755); err != nil { //nolint:gosec // a test directory
		t.Fatal(err)
	}
	t.Chdir(repo)
	m.seed(t, "claude", "project", "cf", betaCf(t), "mcp", "serve", "--profile", "modify")
	if code, out, errOut := run(t, "mcp", "install", "--claude", "--scope", "project"); code != 0 {
		t.Fatalf("exit %d: %s%s", code, out, errOut)
	}
	got, err := readRegistrations(claudeTool, "project", repo)
	if err != nil {
		t.Fatal(err)
	}
	requireEqual(t, len(got), 1)
	requireEqual(t, got["cflk"].Command, selfBinary(t))
}

func TestMCPRegistration_IsCodeFlockOnlyWhenTheExecutableIsAndTheArgumentsMatch(t *testing.T) {
	dir := t.TempDir()
	bin := selfBinary(t)
	link := filepath.Join(dir, "cf")
	if err := os.Symlink(bin, link); err != nil {
		t.Fatal(err)
	}
	copied := filepath.Join(dir, "cflk-copy")      // not the same file, but built from this module
	if data, err := os.ReadFile(bin); err != nil { //nolint:gosec // the test binary
		t.Fatal(err)
	} else if err := os.WriteFile(copied, data, 0o755); err != nil { //nolint:gosec // a copy of the test binary that must run
		t.Fatal(err)
	}
	script := filepath.Join(dir, "cflk")                                       // called cflk, is not
	if err := os.WriteFile(script, []byte("#!/bin/sh\n"), 0o755); err != nil { //nolint:gosec // a test double
		t.Fatal(err)
	}
	cases := []struct {
		name string
		r    mcpRegistration
		want bool
	}{
		{"the binary being installed", mcpRegistration{Command: bin, Args: mcpServeArgs("execute")}, true},
		{"a link to it", mcpRegistration{Command: link, Args: mcpServeArgs("read")}, true},
		{"a copy built from the module", mcpRegistration{Command: copied, Args: mcpServeArgs("read")}, true},
		{"called cflk, is a script", mcpRegistration{Command: script, Args: mcpServeArgs("read")}, false},
		{"called cf, does not exist", mcpRegistration{Command: "/opt/none/cf", Args: mcpServeArgs("read")}, false},
		{"another binary", mcpRegistration{Command: "/bin/sh", Args: mcpServeArgs("read")}, false},
		{"another arguments", mcpRegistration{Command: link, Args: []string{"target", "list"}}, false},
		{"another profile", mcpRegistration{Command: link, Args: mcpServeArgs("root")}, false},
		{"an extra argument", mcpRegistration{Command: link, Args: append(mcpServeArgs("read"), "--extra")}, false},
		{"empty", mcpRegistration{}, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			requireEqual(t, c.r.isCodeFlock(bin), c.want)
		})
	}
}

func TestMCPInstall_TheSkillDirectoryKeepsTheFilesOfTheUser(t *testing.T) {
	m := newMCPMachine(t)
	if code, _, _ := run(t, "mcp", "install", "--claude"); code != 0 {
		t.Fatal("install failed")
	}
	mine := filepath.Join(filepath.Dir(m.skill("claude")), "extra.md")
	if err := os.WriteFile(mine, []byte("mine"), 0o600); err != nil {
		t.Fatal(err)
	}
	if code, _, _ := run(t, "mcp", "install", "--claude"); code != 0 {
		t.Fatal("second install failed")
	}
	requireEqual(t, readFile(t, mine), "mine")
}

func TestParseCodexRegistrations_ReadsCommandAndArgsOfEachServer(t *testing.T) {
	got, err := parseCodexRegistrations([]byte("model = \"x\"\n\n[mcp_servers.cf]\ncommand = \"/opt/a b/cf\"\nargs = [\n  \"mcp\", \"serve\",\n  '--profile', \"read\"\n]\n\n" +
		"[mcp_servers.\"cflk\"] # mine\ncommand = '/x/cflk'\nargs = [\"mcp\"]\ntool_timeout_sec = 660\n[mcp_servers.cf.env]\nA = \"1\"\n[projects.\"/p\"]\ncommand = \"no\"\n"))
	if err != nil {
		t.Fatal(err)
	}
	requireEqual(t, len(got), 2)
	requireEqual(t, got["cf"].Command, "/opt/a b/cf")
	requireEqual(t, got["cf"].Args, []string{"mcp", "serve", "--profile", "read"})
	requireEqual(t, got["cflk"].Command, "/x/cflk")
	requireEqual(t, got["cflk"].Args, []string{"mcp"})
}

// The three forms TOML has for the same server, and a comment after the args.
var codexForms = map[string]string{
	"table":   "[mcp_servers.cflk]\ncommand = \"/opt/other/tool\"\nargs = [\"x\", \"y\"]\n",
	"inline":  "[mcp_servers]\ncflk = { command = \"/opt/other/tool\", args = [\"x\", \"y\"] }\n",
	"dotted":  "[mcp_servers]\ncflk.command = \"/opt/other/tool\"\ncflk.args = [\"x\", \"y\"]\n",
	"dotted2": "mcp_servers.cflk.command = \"/opt/other/tool\"\nmcp_servers.cflk.args = [\"x\", \"y\"]\n",
}

func TestParseCodexRegistrations_ReadsTheThreeFormsOfAServer(t *testing.T) {
	for name, content := range codexForms {
		t.Run(name, func(t *testing.T) {
			got, err := parseCodexRegistrations([]byte(content))
			if err != nil {
				t.Fatal(err)
			}
			requireEqual(t, got["cflk"].Command, "/opt/other/tool")
			requireEqual(t, got["cflk"].Args, []string{"x", "y"})
		})
	}
}

func TestParseCodexRegistrations_ACommentAfterTheArgsIsNotAnArgument(t *testing.T) {
	got, err := parseCodexRegistrations([]byte("[mcp_servers.cf]\ncommand = \"/opt/cf\" # \"old\"\nargs = [\"mcp\", \"serve\", \"--profile\", \"read\"] # was \"modify\"\n"))
	if err != nil {
		t.Fatal(err)
	}
	requireEqual(t, got["cf"].Command, "/opt/cf")
	requireEqual(t, got["cf"].Args, mcpServeArgs("read"))
	requireEqual(t, got["cf"].isCodeFlock("/opt/cf"), true)
}

func TestParseCodexRegistrations_WhatItCannotReadIsNotCodeFlocks(t *testing.T) {
	got, err := parseCodexRegistrations([]byte("[mcp_servers]\na = 3\nb = { command = 7, args = [1] }\nc = { command = \"cflk\", args = [\"mcp\", 2] }\n"))
	if err != nil {
		t.Fatal(err)
	}
	requireEqual(t, len(got), 3)
	for name, reg := range got {
		if reg.isCodeFlock("/bin/x") {
			t.Errorf("%s reads as Code Flock's: %+v", name, reg)
		}
	}
	if _, err := parseCodexRegistrations([]byte("[mcp_servers.cflk\ncommand = ")); err == nil {
		t.Error("a file that is not TOML parsed")
	}
}

func TestParseClaudeRegistrations_MatchesKeysAsClaudeDoes(t *testing.T) {
	got, err := parseClaudeRegistrations([]byte(`{"MCPSERVERS":{"cflk":{"command":"/a","args":[]}},"mcpServers":{"cflk":{"Command":"/b","command":"/c","args":["mcp"]},"cf":{"command":5,"args":"x"},"odd":3}}`))
	if err != nil {
		t.Fatal(err)
	}
	requireEqual(t, len(got), 3)
	requireEqual(t, got["cflk"].Command, "/c")
	requireEqual(t, got["cflk"].Args, []string{"mcp"})
	requireEqual(t, got["cf"].Command+strings.Join(got["cf"].Args, ""), "")
	requireEqual(t, got["odd"].Command+strings.Join(got["odd"].Args, ""), "")
	if got, _ := parseClaudeRegistrations([]byte(`{"MCPServers":{"cflk":{"command":"/a"}}}`)); len(got) != 0 {
		t.Errorf("a key in another case was read: %v", got)
	}
}

func TestMCPInstall_ACflkThatIsNotCodeFlockIsAnErrorInEachFormOfTheCodexConfig(t *testing.T) {
	for name, content := range codexForms {
		t.Run(name, func(t *testing.T) {
			m := newMCPMachine(t)
			if err := os.WriteFile(m.codexConfig(), []byte(content), 0o600); err != nil {
				t.Fatal(err)
			}
			code, stdout, _ := run(t, "mcp", "install", "--codex", "--json")
			if code == 0 {
				t.Fatal("install over a cflk that is not Code Flock succeeded")
			}
			requireEqual(t, errorOf(t, stdout)["code"], "CF_INVALID_REQUEST")
			requireEqual(t, len(m.calls(t)), 0)
			requireEqual(t, readFile(t, m.codexConfig()), content)
			if code, _, _ := run(t, "mcp", "uninstall", "--codex"); code != 0 {
				t.Fatalf("uninstall exit %d", code)
			}
			requireEqual(t, len(m.calls(t)), 0)
			requireEqual(t, readFile(t, m.codexConfig()), content)
		})
	}
}

func TestMCPInstall_AConfigThatCannotBeParsedIsAnErrorWithoutChanges(t *testing.T) {
	m := newMCPMachine(t)
	broken := "[mcp_servers.cflk\ncommand = \"/opt/other/tool\"\n"
	if err := os.WriteFile(m.codexConfig(), []byte(broken), 0o600); err != nil {
		t.Fatal(err)
	}
	if code, _, _ := run(t, "mcp", "install", "--codex"); code == 0 {
		t.Fatal("install over a config that cannot be read succeeded")
	}
	requireEqual(t, len(m.calls(t)), 0)
	requireEqual(t, readFile(t, m.codexConfig()), broken)
}

func TestMCPInstall_ClaudeConfigDirHoldsTheUserRegistrations(t *testing.T) {
	m := newMCPMachine(t)
	dir := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", dir)
	requireEqual(t, claudeServersFile("user", m.home), filepath.Join(dir, ".claude.json"))
	m.seed(t, "claude", "user", "cf", betaCf(t), "mcp", "serve", "--profile", "read")
	if code, out, errOut := run(t, "mcp", "install", "--claude"); code != 0 {
		t.Fatalf("exit %d: %s%s", code, out, errOut)
	}
	got, err := readRegistrations(claudeTool, "user", m.home)
	if err != nil {
		t.Fatal(err)
	}
	requireEqual(t, len(got), 1)
	requireEqual(t, got["cflk"].Command, selfBinary(t))
	if _, err := os.Stat(filepath.Join(m.home, ".claude.json")); !os.IsNotExist(err) {
		t.Errorf("the home was read or written: %v", err)
	}
}

func TestMCPInstall_AProjectMCPJSONThatIsASymlinkIsAnErrorWithoutChanges(t *testing.T) {
	for _, verb := range []string{"install", "uninstall"} {
		t.Run(verb, func(t *testing.T) {
			m := newMCPMachine(t)
			repo := t.TempDir()
			if err := os.MkdirAll(filepath.Join(repo, ".git"), 0o755); err != nil { //nolint:gosec // a test directory
				t.Fatal(err)
			}
			outside := filepath.Join(t.TempDir(), "outside.json")
			const content = `{"mcpServers":{}}`
			if err := os.WriteFile(outside, []byte(content), 0o600); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(outside, filepath.Join(repo, ".mcp.json")); err != nil {
				t.Fatal(err)
			}
			t.Chdir(repo)
			if code, _, errOut := run(t, "mcp", verb, "--claude", "--scope", "project"); code == 0 || !strings.Contains(errOut, "symlink") {
				t.Fatalf("exit %d: %s", code, errOut)
			}
			requireEqual(t, len(m.calls(t)), 0)
			requireEqual(t, readFile(t, outside), content)
		})
	}
}

func TestMCPInstall_TheCodefabricSkillOfAPublishedVersionIsRemoved(t *testing.T) {
	for _, file := range []string{"legacy-skill-2b610f2.md", "legacy-skill-4028d40.md"} {
		for _, verb := range []string{"install", "uninstall"} {
			t.Run(file+"/"+verb, func(t *testing.T) {
				m := newMCPMachine(t)
				legacy := filepath.Join(m.home, ".claude", "skills", "codefabric")
				if err := os.MkdirAll(legacy, 0o755); err != nil { //nolint:gosec // a test directory
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(legacy, "SKILL.md"), []byte(readFile(t, filepath.Join("testdata", file))), 0o600); err != nil {
					t.Fatal(err)
				}
				if code, out, errOut := run(t, "mcp", verb, "--claude"); code != 0 {
					t.Fatalf("exit %d: %s%s", code, out, errOut)
				}
				if _, err := os.Stat(legacy); !os.IsNotExist(err) {
					t.Errorf("the skill directory of the earlier version is still there: %v", err)
				}
			})
		}
	}
}

func TestMCPInstall_ACodefabricSkillThatIsNotAPublishedVersionStaysWithANote(t *testing.T) {
	published := readFile(t, filepath.Join("testdata", "legacy-skill-4028d40.md"))
	cases := map[string]func(t *testing.T, dir string){
		"edited": func(t *testing.T, dir string) { writeLegacy(t, dir, "SKILL.md", published+"my edit\n") },
		"extra file": func(t *testing.T, dir string) {
			writeLegacy(t, dir, "SKILL.md", published)
			writeLegacy(t, dir, "notes.md", "mine")
		},
		"other skill": func(t *testing.T, dir string) { writeLegacy(t, dir, "SKILL.md", "---\nname: codefabric\n---\nmine\n") },
	}
	for name, setup := range cases {
		t.Run(name, func(t *testing.T) {
			m := newMCPMachine(t)
			legacy := filepath.Join(m.home, ".claude", "skills", "codefabric")
			setup(t, legacy)
			before := map[string]string{}
			entries, _ := os.ReadDir(legacy)
			for _, e := range entries {
				before[e.Name()] = readFile(t, filepath.Join(legacy, e.Name()))
			}
			code, out, errOut := run(t, "mcp", "install", "--claude")
			if code != 0 || strings.Count(out, "an earlier version of cflk and can be deleted") != 1 {
				t.Fatalf("exit %d: %s%s", code, out, errOut)
			}
			for file, content := range before {
				requireEqual(t, readFile(t, filepath.Join(legacy, file)), content)
			}
		})
	}
}

func writeLegacy(t *testing.T, dir, name, content string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil { //nolint:gosec // a test directory
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestMCPInstall_ACodeflockSkillThatCflkDidNotWriteIsLeftAloneWithANote(t *testing.T) {
	for _, tool := range []string{"claude", "codex"} {
		t.Run(tool, func(t *testing.T) {
			m := newMCPMachine(t)
			writeLegacy(t, filepath.Dir(m.skill(tool)), "SKILL.md", "---\nname: codeflock\n---\nmine\n")
			code, out, errOut := run(t, "mcp", "install", "--"+tool)
			if code != 0 || !strings.Contains(out, "did not write") {
				t.Fatalf("install: exit %d: %s%s", code, out, errOut)
			}
			requireEqual(t, readFile(t, m.skill(tool)), "---\nname: codeflock\n---\nmine\n")
			requireEqual(t, m.registered(t, tool)["cflk"].Command, selfBinary(t))
			if code, out, errOut := run(t, "mcp", "uninstall", "--"+tool); code != 0 || !strings.Contains(out, "did not write") {
				t.Fatalf("uninstall: exit %d: %s%s", code, out, errOut)
			}
			requireEqual(t, readFile(t, m.skill(tool)), "---\nname: codeflock\n---\nmine\n")
		})
	}
}

func TestMCPInstall_TheSkillCarriesTheMarkerAndIsReplacedWhenItDoes(t *testing.T) {
	m := newMCPMachine(t)
	writeLegacy(t, filepath.Dir(m.skill("claude")), "SKILL.md", "---\nname: codeflock\n---\n<!-- cflk-version: v0.0.1 -->\nold\n")
	if code, out, errOut := run(t, "mcp", "install", "--claude"); code != 0 {
		t.Fatalf("exit %d: %s%s", code, out, errOut)
	}
	got := readFile(t, m.skill("claude"))
	requireEqual(t, got, renderedSkill(t))
	requireEqual(t, strings.Contains(got, "<!-- cflk-version: "+buildinfo.Get().Version+" -->\n"), true)
	if code, _, _ := run(t, "mcp", "uninstall", "--claude"); code != 0 {
		t.Fatal("uninstall failed")
	}
	if _, err := os.Stat(m.skill("claude")); !os.IsNotExist(err) {
		t.Errorf("the skill with the marker was not removed: %v", err)
	}
}

func TestMCPInstall_AFailedAddPutsTheRegistrationItReplacedBack(t *testing.T) {
	for _, tool := range []string{"claude", "codex"} {
		for _, fault := range []string{"FAKE_FAIL_ADD", "FAKE_NOOP_ADD"} {
			t.Run(tool+"/"+fault, func(t *testing.T) {
				m := newMCPMachine(t)
				m.seedEnv(t, tool, "user", "cflk", []string{"K=v"}, selfBinary(t), "mcp", "serve", "--profile", "read")
				t.Setenv(fault, "1")
				code, _, errOut := run(t, "mcp", "install", "--"+tool)
				if code == 0 || !strings.Contains(errOut, "put back") {
					t.Fatalf("exit %d: %s", code, errOut)
				}
				got := m.registered(t, tool)["cflk"]
				requireEqual(t, got.Command, selfBinary(t))
				requireEqual(t, got.Args, mcpServeArgs("read"))
				requireEqual(t, got.Env["K"], "v")
			})
		}
	}
}

func TestMCPInstall_TheEnvOfTheServerGoesToCflk(t *testing.T) {
	for _, tool := range []string{"claude", "codex"} {
		t.Run(tool, func(t *testing.T) {
			m := newMCPMachine(t)
			m.seedEnv(t, tool, "user", "cf", []string{"CF_CONTROLLER=http://c:8080", "TOKEN=s3cret"}, betaCf(t), "mcp", "serve", "--profile", "read")
			code, out, errOut := run(t, "mcp", "install", "--"+tool, "--dry-run")
			if code != 0 || strings.Contains(out+errOut, "s3cret") || !strings.Contains(out, "TOKEN=***") {
				t.Fatalf("dry run: exit %d: %s%s", code, out, errOut)
			}
			if code, out, errOut := run(t, "mcp", "install", "--"+tool); code != 0 || strings.Contains(out+errOut, "s3cret") {
				t.Fatalf("exit %d: %s%s", code, out, errOut)
			}
			got := m.registered(t, tool)
			if _, ok := got["cf"]; ok {
				t.Error("the cf is still registered")
			}
			requireEqual(t, got["cflk"].Env["CF_CONTROLLER"], "http://c:8080")
			requireEqual(t, got["cflk"].Env["TOKEN"], "s3cret")
			if code, out, errOut := run(t, "mcp", "install", "--"+tool); code != 0 {
				t.Fatalf("second install: exit %d: %s%s", code, out, errOut)
			}
			requireEqual(t, m.registered(t, tool)["cflk"].Env["TOKEN"], "s3cret")
		})
	}
}

func TestMCPInstall_ACfWithSettingsThatAddCannotWriteIsNotMoved(t *testing.T) {
	m := newMCPMachine(t)
	beta := betaCf(t)
	cfg := "[mcp_servers.cf]\ncommand = \"" + beta + "\"\nargs = [\"mcp\", \"serve\", \"--profile\", \"read\"]\ncwd = \"/work\"\n"
	if err := os.WriteFile(m.codexConfig(), []byte(cfg), 0o600); err != nil {
		t.Fatal(err)
	}
	code, out, errOut := run(t, "mcp", "install", "--codex")
	if code != 0 || !strings.Contains(out, "cwd") {
		t.Fatalf("exit %d: %s%s", code, out, errOut)
	}
	got := m.registered(t, "codex")
	requireEqual(t, got["cf"].Command, beta)
	requireEqual(t, got["cflk"].Command, selfBinary(t))
	for _, c := range m.calls(t) {
		if strings.HasSuffix(c, " cf") {
			t.Errorf("the cf was removed: %s", c)
		}
	}
}

func TestMCPInstall_ACfCalledCfWithTheServeArgumentsThatIsNotCodeFlockIsLeftAlone(t *testing.T) {
	for _, tool := range []string{"claude", "codex"} {
		t.Run(tool, func(t *testing.T) {
			m := newMCPMachine(t)
			script := filepath.Join(t.TempDir(), "cf")
			if err := os.WriteFile(script, []byte("#!/bin/sh\n"), 0o755); err != nil { //nolint:gosec // a test double
				t.Fatal(err)
			}
			m.seed(t, tool, "user", "cf", script, "mcp", "serve", "--profile", "read")
			for _, verb := range []string{"install", "uninstall"} {
				if code, out, errOut := run(t, "mcp", verb, "--"+tool); code != 0 {
					t.Fatalf("%s: exit %d: %s%s", verb, code, out, errOut)
				}
				requireEqual(t, m.registered(t, tool)["cf"].Command, script)
			}
		})
	}
}

func TestMCPCodex_ASymlinkedConfigIsRefusedBeforeAnythingChanges(t *testing.T) {
	for _, verb := range []string{"install", "uninstall"} {
		t.Run(verb, func(t *testing.T) {
			m := newMCPMachine(t)
			target := filepath.Join(t.TempDir(), "real.toml")
			if err := os.WriteFile(target, []byte("[mcp_servers.cflk]\ncommand = \""+selfBinary(t)+"\"\nargs = [\"mcp\", \"serve\", \"--profile\", \"read\"]\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(target, m.codexConfig()); err != nil {
				t.Fatal(err)
			}
			before := readFile(t, target)
			code, _, errOut := run(t, "mcp", verb, "--codex")
			if code == 0 || !strings.Contains(errOut, "symlink") {
				t.Fatalf("exit %d: %s", code, errOut)
			}
			requireEqual(t, len(m.calls(t)), 0)
			requireEqual(t, readFile(t, target), before)
			if _, err := os.Stat(m.skill("codex")); !os.IsNotExist(err) {
				t.Errorf("the skill was touched: %v", err)
			}
		})
	}
}
