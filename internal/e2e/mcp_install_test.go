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

// A fake claude: logs its argv and keeps its servers where the real one does,
// with the command, args and env it was given: ~/.claude.json for the user
// scope, .mcp.json of the working directory for the project scope. `remove` of
// a server that is not there fails the way the real one does.
const mcpFakeClaude = `#!/bin/sh
echo "claude $*" >> "$FAKE_LOG"
op="$2"; scope="$4"; name="$5"
if [ "$scope" = project ]; then out="$(pwd -P)/.mcp.json"; else out="$HOME/.claude.json"; fi
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
// $CODEX_HOME/config.toml with command and args on one line each (and so drops
// any tool_timeout_sec), and --env goes to its [mcp_servers.<name>.env] table;
// `remove` deletes both. Other tables stay. The table goes before the first
// [projects.*] table, in the middle of the file, as in a config that has
// trusted projects.
const mcpFakeCodex = `#!/bin/sh
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

const (
	mcpProjectsHeader = `[projects."/Users/dev/my repo"]`
	mcpOtherTables    = "[model_providers.local]\nname = \"local\"\n\n[mcp_servers.other]\ncommand = \"other\"\nargs = [\"x\"]\n\n" +
		mcpProjectsHeader + "\ntrust_level = \"trusted\"\n"
)

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
		b.t.Fatalf("cflk %s: %v", strings.Join(args, " "), err)
	}
	res.stdout, res.stderr = out.String(), errb.String()
	return res
}

func (b *mcpBox) mustCF(args ...string) mcpRun {
	b.t.Helper()
	res := b.cf(args...)
	if res.code != 0 {
		b.t.Fatalf("cflk %s: exit %d\nstdout: %s\nstderr: %s", strings.Join(args, " "), res.code, res.stdout, res.stderr)
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

// mcpWithoutTable is the config with one table taken out, up to the next header.
func mcpWithoutTable(config, header string) string {
	var kept []string
	skip := false
	for _, line := range strings.Split(config, "\n") {
		if strings.HasPrefix(line, "[") {
			skip = strings.TrimSpace(line) == header
		}
		if !skip {
			kept = append(kept, line)
		}
	}
	return strings.Join(kept, "\n")
}

func TestScenario_MCP_Instalacion_ClaudeUserYProject(t *testing.T) {
	b := newMCPBox(t, "claude")
	b.mustCF("mcp", "install", "--claude", "--scope", "user")
	b.mustCF("mcp", "install", "--claude", "--scope", "project")

	mcpWantCalls(t, b.calls(), []string{
		"claude mcp remove --scope user cflk",
		"claude mcp add --scope user cflk -- " + binary + " mcp serve --profile execute",
		"claude mcp remove --scope project cflk",
		"claude mcp add --scope project cflk -- " + binary + " mcp serve --profile execute",
	})
	skills := map[string]string{
		"user":    filepath.Join(b.home, ".claude", "skills", "codeflock", "SKILL.md"),
		"project": filepath.Join(b.repo, ".claude", "skills", "codeflock", "SKILL.md"),
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
		if !strings.Contains(mcpRead(t, path), "<!-- cflk-version: ") {
			t.Errorf("scope %s: the skill at %s has no cflk-version marker", scope, path)
		}
	}
	registries := map[string]string{"user": filepath.Join(b.home, ".claude.json"), "project": filepath.Join(b.repo, ".mcp.json")}
	for scope, path := range registries {
		want := `{"mcpServers":{"cflk":{"type":"stdio","command":"` + binary + `","args":["mcp","serve","--profile","execute"],"env":{}}}}`
		if got := strings.TrimSpace(mcpRead(t, path)); got != want {
			t.Errorf("scope %s: %s\n got %s\nwant %s", scope, path, got, want)
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
		"codex mcp remove cflk",
		"codex mcp add cflk -- " + binary + " mcp serve --profile execute",
	})
	config := mcpRead(t, b.codexConfig())
	if got := mcpWithoutTable(config, "[mcp_servers.cflk]"); got != mcpOtherTables {
		t.Errorf("the tables the user had changed:\n%s", config)
	}
	if got := mcpTable(config, mcpProjectsHeader); !slices.Equal(got, []string{`trust_level = "trusted"`}) {
		t.Errorf("%s = %q: the timeout landed in the wrong table", mcpProjectsHeader, got)
	}
	want := []string{
		`command = "` + binary + `"`,
		`args = ["mcp", "serve", "--profile", "execute"]`,
		"tool_timeout_sec = 660",
	}
	if got := mcpTable(config, "[mcp_servers.cflk]"); !slices.Equal(got, want) {
		t.Errorf("[mcp_servers.cflk]:\n got %q\nwant %q", got, want)
	}
	if got := mcpTable(config, "[mcp_servers.other]"); len(got) != 2 {
		t.Errorf("[mcp_servers.other] = %q", got)
	}
	skill := filepath.Join(b.home, ".agents", "skills", "codeflock", "SKILL.md")
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
		filepath.Join(b.home, ".claude", "skills", "codeflock", "SKILL.md"),
		filepath.Join(b.home, ".agents", "skills", "codeflock", "SKILL.md"),
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
		"claude mcp add --scope user cflk -- " + binary + " mcp serve --profile read",
		"codex mcp add cflk -- " + binary + " mcp serve --profile read",
	} {
		if !slices.Contains(calls, want) {
			t.Errorf("no call %q in %q", want, calls)
		}
	}
	config := mcpRead(t, b.codexConfig())
	if n := strings.Count(config, "[mcp_servers.cflk]"); n != 1 {
		t.Fatalf("%d [mcp_servers.cflk] tables:\n%s", n, config)
	}
	want := []string{
		`command = "` + binary + `"`,
		`args = ["mcp", "serve", "--profile", "read"]`,
		"tool_timeout_sec = 660",
	}
	if got := mcpTable(config, "[mcp_servers.cflk]"); !slices.Equal(got, want) {
		t.Errorf("[mcp_servers.cflk]:\n got %q\nwant %q", got, want)
	}
	if got := mcpWithoutTable(config, "[mcp_servers.cflk]"); got != mcpOtherTables {
		t.Errorf("the tables the user had changed:\n%s", config)
	}
	if got := mcpTable(config, mcpProjectsHeader); !slices.Equal(got, []string{`trust_level = "trusted"`}) {
		t.Errorf("%s = %q: the timeout landed in the wrong table", mcpProjectsHeader, got)
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

	// claude keeps its own files: what must be left in them is no server.
	if files := mcpFiles(t, b.home); !slices.Equal(files, []string{".claude.json"}) {
		t.Errorf("files left in HOME: %q", files)
	}
	if files := mcpFiles(t, b.repo); !slices.Equal(files, []string{".mcp.json"}) {
		t.Errorf("files left in the repository: %q", files)
	}
	for _, path := range []string{filepath.Join(b.home, ".claude.json"), filepath.Join(b.repo, ".mcp.json")} {
		if got := strings.TrimSpace(mcpRead(t, path)); got != `{"mcpServers":{}}` {
			t.Errorf("%s still has servers: %s", path, got)
		}
	}
	for _, dir := range []string{
		filepath.Join(b.home, ".claude", "skills", "codeflock"), filepath.Join(b.home, ".agents", "skills", "codeflock"),
		filepath.Join(b.repo, ".claude", "skills", "codeflock"),
	} {
		if _, err := os.Lstat(dir); !errors.Is(err, fs.ErrNotExist) {
			t.Errorf("%s was left (%v)", dir, err)
		}
	}
	if got := mcpRead(t, b.codexConfig()); got != mcpOtherTables {
		t.Errorf("config.toml is not as the user had it:\n%s", got)
	}
	// A second uninstall finds nothing to do and still succeeds.
	b.mustCF("mcp", "uninstall", "--claude", "--scope", "user")
	b.mustCF("mcp", "uninstall", "--codex")
}

// The unit suite already covers the rule (TestCfBinary_PrefersThePATHEntryOnlyWhenItIsTheRunningBinary);
// this one shows it in a real process: the cflk in PATH is a symlink to the
// binary, and that link, not its target, is what gets registered.
func TestScenario_MCP_Instalacion_RegistraLaRutaDelPATHYNoElDestinoDelSymlink(t *testing.T) {
	b := newMCPBox(t, "claude")
	link := filepath.Join(b.bin, "cflk")
	if err := os.Symlink(binary, link); err != nil {
		t.Fatal(err)
	}
	if res := b.cfAs(link, "mcp", "install", "--claude"); res.code != 0 {
		t.Fatalf("exit %d\nstdout: %s\nstderr: %s", res.code, res.stdout, res.stderr)
	}
	mcpWantCalls(t, b.calls(), []string{
		"claude mcp remove --scope user cflk",
		"claude mcp add --scope user cflk -- " + link + " mcp serve --profile execute",
	})
}

// The beta.6 registered `cf`: install leaves `cflk` in its place when the old
// one is Code Flock's, and does not touch a `cf` that is something else.
func TestScenario_MCP_Instalacion_ReemplazaElRegistroCfDeCodeFlockYNoTocaElAjeno(t *testing.T) {
	serve := []string{"mcp", "serve", "--profile", "execute"}
	for _, legacy := range []struct {
		name, command string
		args          []string
		wantCfLeft    bool
	}{
		{"claude user scope: cf is Code Flock's", binary, serve, false},
		{"claude user scope: cf is not Code Flock's", "other", []string{"x"}, true},
	} {
		t.Run(legacy.name, func(t *testing.T) {
			b := newMCPBox(t, "claude")
			seed := exec.CommandContext(t.Context(), filepath.Join(b.bin, "claude"), append([]string{"mcp", "add", "--scope", "user", "cf", "--", legacy.command}, legacy.args...)...) //nolint:gosec // the fake
			seed.Env = []string{"HOME=" + b.home, "PATH=" + b.bin, "FAKE_LOG=" + b.log, "FAKE_STATE=" + b.state}
			if out, err := seed.CombinedOutput(); err != nil {
				t.Fatalf("seeding cf: %v: %s", err, out)
			}
			b.mustCF("mcp", "install", "--claude", "--scope", "user")

			registry := mcpRead(t, filepath.Join(b.home, ".claude.json"))
			if !strings.Contains(registry, `"cflk":{`) {
				t.Errorf("cflk is not registered:\n%s", registry)
			}
			if got := strings.Contains(registry, `"cf":{`); got != legacy.wantCfLeft {
				t.Errorf("cf registered = %v, want %v:\n%s", got, legacy.wantCfLeft, registry)
			}
		})
	}
}
