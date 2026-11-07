//go:build e2e && (linux || darwin)

package e2e

import (
	"bytes"
	"context"
	"errors"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

// A fake claude: logs its argv; `remove` of a server that is not there fails
// the way the real one does.
const mcpFakeClaude = `#!/bin/sh
echo "claude $*" >> "$FAKE_LOG"
case "$2" in
add) /usr/bin/touch "$FAKE_STATE/claude-registered" ;;
remove)
	if [ -e "$FAKE_STATE/claude-registered" ]; then /bin/rm "$FAKE_STATE/claude-registered"
	else echo 'No MCP server "cf" in user scope'; exit 1; fi ;;
esac
`

// A fake codex 0.160.1: `add` rewrites the [mcp_servers.cf] table of
// $CODEX_HOME/config.toml with command and args on one line each (and so drops
// any tool_timeout_sec); `remove` deletes the table. Other tables stay.
const mcpFakeCodex = `#!/bin/sh
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
	printf '[mcp_servers.cf]\ncommand = "%s"\nargs = [%s]\n' "$cmd" "$args" >> "$cfg"
	;;
esac
`

const mcpOtherTables = "[model_providers.local]\nname = \"local\"\n\n[mcp_servers.other]\ncommand = \"other\"\nargs = [\"x\"]\n"

// mcpBox is a machine of its own: temporary HOME and CODEX_HOME, a PATH with
// only the fakes, and a git repository for the project scope.
type mcpBox struct {
	t                     *testing.T
	home, codexHome, repo string
	bin, log, state       string
}

func newMCPBox(t *testing.T, fakes ...string) *mcpBox {
	t.Helper()
	root := t.TempDir()
	b := &mcpBox{
		t: t, home: filepath.Join(root, "home"), codexHome: filepath.Join(root, "codex"),
		repo: filepath.Join(root, "repo"), bin: filepath.Join(root, "bin"),
		log: filepath.Join(root, "calls.log"), state: filepath.Join(root, "state"),
	}
	for _, dir := range []string{b.home, b.codexHome, filepath.Join(b.repo, ".git"), b.bin, b.state} {
		if err := os.MkdirAll(dir, 0o755); err != nil { //nolint:gosec // a test directory
			t.Fatal(err)
		}
	}
	scripts := map[string]string{"claude": mcpFakeClaude, "codex": mcpFakeCodex}
	for _, name := range fakes {
		if err := os.WriteFile(filepath.Join(b.bin, name), []byte(scripts[name]), 0o755); err != nil { //nolint:gosec // a test double that must run
			t.Fatal(err)
		}
	}
	return b
}

type mcpRun struct {
	code           int
	stdout, stderr string
}

// cf runs the real binary as a process, from the repository, with this box as its machine.
func (b *mcpBox) cf(args ...string) mcpRun {
	b.t.Helper()
	return b.cfAs(binary, args...)
}

func (b *mcpBox) cfAs(exe string, args ...string) mcpRun {
	b.t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), Scaled(cfLimit))
	defer cancel()
	cmd := exec.CommandContext(ctx, exe, args...) //nolint:gosec // the cf under test
	cmd.Dir = b.repo
	cmd.Env = []string{
		"HOME=" + b.home, "CODEX_HOME=" + b.codexHome, "PATH=" + b.bin, "LANG=C",
		"FAKE_LOG=" + b.log, "FAKE_STATE=" + b.state, "TMPDIR=" + b.state,
	}
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	res := mcpRun{}
	var exit *exec.ExitError
	switch err := cmd.Run(); {
	case errors.As(err, &exit):
		res.code = exit.ExitCode()
	case err != nil:
		b.t.Fatalf("cf %s: %v", strings.Join(args, " "), err)
	}
	res.stdout, res.stderr = out.String(), errb.String()
	return res
}

func (b *mcpBox) mustCF(args ...string) mcpRun {
	b.t.Helper()
	res := b.cf(args...)
	if res.code != 0 {
		b.t.Fatalf("cf %s: exit %d\nstdout: %s\nstderr: %s", strings.Join(args, " "), res.code, res.stdout, res.stderr)
	}
	return res
}

func (b *mcpBox) calls() []string {
	b.t.Helper()
	data, err := os.ReadFile(b.log)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		b.t.Fatal(err)
	}
	return strings.Split(strings.TrimSpace(string(data)), "\n")
}

func (b *mcpBox) codexConfig() string { return filepath.Join(b.codexHome, "config.toml") }

func (b *mcpBox) write(path, content string) {
	b.t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil { //nolint:gosec // a test file
		b.t.Fatal(err)
	}
}

func mcpRead(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path) //nolint:gosec // a path of the test
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

// mcpFiles lists the regular files under dir, relative to it.
func mcpFiles(t *testing.T, dir string) []string {
	t.Helper()
	var files []string
	err := filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err == nil && !d.IsDir() {
			rel, _ := filepath.Rel(dir, path)
			files = append(files, rel)
		}
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	return files
}

func mcpMode(t *testing.T, path string) fs.FileMode {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	return info.Mode().Perm()
}

func mcpWantCalls(t *testing.T, got, want []string) {
	t.Helper()
	if !slices.Equal(got, want) {
		t.Fatalf("calls:\n got %q\nwant %q", got, want)
	}
}

// mcpTable is the lines of a TOML table, header excluded.
func mcpTable(config, header string) []string {
	var table []string
	in := false
	for _, line := range strings.Split(config, "\n") {
		if strings.HasPrefix(line, "[") {
			in = strings.TrimSpace(line) == header
			continue
		}
		if in && strings.TrimSpace(line) != "" {
			table = append(table, line)
		}
	}
	return table
}

func TestScenario_MCP_Instalacion_ClaudeUserYProject(t *testing.T) {
	b := newMCPBox(t, "claude")
	b.mustCF("mcp", "install", "--claude", "--scope", "user")
	b.mustCF("mcp", "install", "--claude", "--scope", "project")

	mcpWantCalls(t, b.calls(), []string{
		"claude mcp remove --scope user cf",
		"claude mcp add --scope user cf -- " + binary + " mcp serve --profile execute",
		"claude mcp remove --scope project cf",
		"claude mcp add --scope project cf -- " + binary + " mcp serve --profile execute",
	})
	skills := map[string]string{
		"user":    filepath.Join(b.home, ".claude", "skills", "codefabric", "SKILL.md"),
		"project": filepath.Join(b.repo, ".claude", "skills", "codefabric", "SKILL.md"),
	}
	for scope, path := range skills {
		if strings.TrimSpace(mcpRead(t, path)) == "" {
			t.Errorf("scope %s: the skill at %s is empty", scope, path)
		}
		if got := mcpMode(t, path); got != 0o644 {
			t.Errorf("scope %s: skill mode %o, want 644", scope, got)
		}
		if got := mcpMode(t, filepath.Dir(path)); got != 0o755 {
			t.Errorf("scope %s: skill directory mode %o, want 755", scope, got)
		}
	}
	if mcpRead(t, skills["user"]) != mcpRead(t, skills["project"]) {
		t.Error("the skill differs between scopes")
	}
}

func TestScenario_MCP_Instalacion_CodexFijaElTimeoutSinTocarOtrasTablas(t *testing.T) {
	b := newMCPBox(t, "codex")
	b.write(b.codexConfig(), mcpOtherTables)
	b.mustCF("mcp", "install", "--codex")

	mcpWantCalls(t, b.calls(), []string{
		"codex mcp remove cf",
		"codex mcp add cf -- " + binary + " mcp serve --profile execute",
	})
	config := mcpRead(t, b.codexConfig())
	if !strings.HasPrefix(config, mcpOtherTables) {
		t.Errorf("the tables the user had changed:\n%s", config)
	}
	want := []string{
		`command = "` + binary + `"`,
		`args = ["mcp", "serve", "--profile", "execute"]`,
		"tool_timeout_sec = 660",
	}
	if got := mcpTable(config, "[mcp_servers.cf]"); !slices.Equal(got, want) {
		t.Errorf("[mcp_servers.cf]:\n got %q\nwant %q", got, want)
	}
	if got := mcpTable(config, "[mcp_servers.other]"); len(got) != 2 {
		t.Errorf("[mcp_servers.other] = %q", got)
	}
	skill := filepath.Join(b.home, ".agents", "skills", "codefabric", "SKILL.md")
	if got := mcpMode(t, skill); got != 0o644 {
		t.Errorf("skill mode %o, want 644", got)
	}
	if got := mcpMode(t, filepath.Dir(skill)); got != 0o755 {
		t.Errorf("skill directory mode %o, want 755", got)
	}
}

func TestScenario_MCP_Instalacion_ReinstalarConOtroPerfilNoDuplicaNiReescribeLaSkill(t *testing.T) {
	b := newMCPBox(t, "claude", "codex")
	b.write(b.codexConfig(), mcpOtherTables)
	skills := []string{
		filepath.Join(b.home, ".claude", "skills", "codefabric", "SKILL.md"),
		filepath.Join(b.home, ".agents", "skills", "codefabric", "SKILL.md"),
	}
	b.mustCF("mcp", "install", "--claude", "--scope", "user")
	b.mustCF("mcp", "install", "--codex")
	mtimes := make([]time.Time, len(skills))
	for i, path := range skills {
		info, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		mtimes[i] = info.ModTime()
	}
	time.Sleep(1100 * time.Millisecond) // beyond the resolution of any filesystem's mtime

	b.mustCF("mcp", "install", "--claude", "--scope", "user", "--profile", "read")
	b.mustCF("mcp", "install", "--codex", "--profile", "read")

	calls := b.calls()
	for _, want := range []string{
		"claude mcp add --scope user cf -- " + binary + " mcp serve --profile read",
		"codex mcp add cf -- " + binary + " mcp serve --profile read",
	} {
		if !slices.Contains(calls, want) {
			t.Errorf("no call %q in %q", want, calls)
		}
	}
	config := mcpRead(t, b.codexConfig())
	if n := strings.Count(config, "[mcp_servers.cf]"); n != 1 {
		t.Fatalf("%d [mcp_servers.cf] tables:\n%s", n, config)
	}
	want := []string{
		`command = "` + binary + `"`,
		`args = ["mcp", "serve", "--profile", "read"]`,
		"tool_timeout_sec = 660",
	}
	if got := mcpTable(config, "[mcp_servers.cf]"); !slices.Equal(got, want) {
		t.Errorf("[mcp_servers.cf]:\n got %q\nwant %q", got, want)
	}
	if !strings.HasPrefix(config, mcpOtherTables) {
		t.Errorf("the tables the user had changed:\n%s", config)
	}
	for i, path := range skills {
		info, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		if !info.ModTime().Equal(mtimes[i]) {
			t.Errorf("%s was rewritten: mtime %v, was %v", path, info.ModTime(), mtimes[i])
		}
	}
}

func TestScenario_MCP_Instalacion_DryRunNoHaceNada(t *testing.T) {
	b := newMCPBox(t, "claude", "codex")
	b.write(b.codexConfig(), mcpOtherTables)
	for _, tool := range []string{"--claude", "--codex"} {
		res := b.mustCF("mcp", "install", tool, "--dry-run")
		if !strings.Contains(res.stdout, "[dry-run]") {
			t.Errorf("%s: stdout does not say it is a dry run:\n%s", tool, res.stdout)
		}
	}
	if calls := b.calls(); calls != nil {
		t.Errorf("a dry run called the CLIs: %q", calls)
	}
	if files := mcpFiles(t, b.home); len(files) != 0 {
		t.Errorf("a dry run wrote in HOME: %q", files)
	}
	if got := mcpRead(t, b.codexConfig()); got != mcpOtherTables {
		t.Errorf("a dry run changed config.toml:\n%s", got)
	}
	if files := mcpFiles(t, b.repo); len(files) != 0 {
		t.Errorf("a dry run wrote in the repository: %q", files)
	}
}

func TestScenario_MCP_Instalacion_SinElCLISaleConCodigo3(t *testing.T) {
	for _, tool := range []string{"claude", "codex"} {
		t.Run(tool, func(t *testing.T) {
			b := newMCPBox(t) // no fake in PATH
			res := b.cf("mcp", "install", "--"+tool)
			if res.code != 3 {
				t.Fatalf("exit %d, want 3\nstdout: %s\nstderr: %s", res.code, res.stdout, res.stderr)
			}
			if !strings.Contains(res.stderr, tool+" is not in PATH") {
				t.Errorf("stderr does not name the missing CLI:\n%s", res.stderr)
			}
			if files := mcpFiles(t, b.home); len(files) != 0 {
				t.Errorf("a failed install wrote in HOME: %q", files)
			}
		})
	}
}

func TestScenario_MCP_Instalacion_UninstallDejaElHomeLimpio(t *testing.T) {
	b := newMCPBox(t, "claude", "codex")
	b.write(b.codexConfig(), mcpOtherTables)
	b.mustCF("mcp", "install", "--claude", "--scope", "user")
	b.mustCF("mcp", "install", "--claude", "--scope", "project")
	b.mustCF("mcp", "install", "--codex")

	b.mustCF("mcp", "uninstall", "--claude", "--scope", "user")
	b.mustCF("mcp", "uninstall", "--claude", "--scope", "project")
	b.mustCF("mcp", "uninstall", "--codex")

	if files := mcpFiles(t, b.home); len(files) != 0 {
		t.Errorf("files left in HOME: %q", files)
	}
	if files := mcpFiles(t, b.repo); len(files) != 0 {
		t.Errorf("files left in the repository: %q", files)
	}
	for _, dir := range []string{
		filepath.Join(b.home, ".claude", "skills", "codefabric"), filepath.Join(b.home, ".agents", "skills", "codefabric"),
		filepath.Join(b.repo, ".claude", "skills", "codefabric"),
	} {
		if _, err := os.Lstat(dir); !errors.Is(err, fs.ErrNotExist) {
			t.Errorf("%s was left (%v)", dir, err)
		}
	}
	if got := mcpRead(t, b.codexConfig()); got != mcpOtherTables {
		t.Errorf("config.toml is not as the user had it:\n%s", got)
	}
	if _, err := os.Stat(filepath.Join(b.state, "claude-registered")); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("claude still has the server registered (%v)", err)
	}
	// A second uninstall finds nothing to do and still succeeds.
	b.mustCF("mcp", "uninstall", "--claude", "--scope", "user")
	b.mustCF("mcp", "uninstall", "--codex")
}

// The unit suite already covers the rule (TestCfBinary_PrefersThePATHEntryOnlyWhenItIsTheRunningBinary);
// this one shows it in a real process: the cf in PATH is a symlink to the
// binary, and that link, not its target, is what gets registered.
func TestScenario_MCP_Instalacion_RegistraLaRutaDelPATHYNoElDestinoDelSymlink(t *testing.T) {
	b := newMCPBox(t, "claude")
	link := filepath.Join(b.bin, "cf")
	if err := os.Symlink(binary, link); err != nil {
		t.Fatal(err)
	}
	if res := b.cfAs(link, "mcp", "install", "--claude"); res.code != 0 {
		t.Fatalf("exit %d\nstdout: %s\nstderr: %s", res.code, res.stdout, res.stderr)
	}
	mcpWantCalls(t, b.calls(), []string{
		"claude mcp remove --scope user cf",
		"claude mcp add --scope user cf -- " + link + " mcp serve --profile execute",
	})
}
