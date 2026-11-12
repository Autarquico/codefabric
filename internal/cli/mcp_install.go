package cli

import (
	"bytes"
	"context"
	_ "embed"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strings"

	"github.com/spf13/cobra"

	"github.com/Autarquico/codefabric/internal/domain"
)

//go:embed skills/codefabric/SKILL.md
var codefabricSkill []byte

const (
	mcpServerName = "cf"
	skillName     = "codefabric"
	// codexToolTimeoutSec is above the 10 min maximum of events_wait; Codex
	// cuts a tool call at 60 s unless its table says otherwise.
	codexToolTimeoutSec = 660
)

var mcpProfiles = []string{"read", "execute", "modify"}

// mcpTool is what differs between the two clients.
type mcpTool struct {
	name        string // the CLI that registers the server
	installHint string
	skillsDir   string // relative to the home or to the repo root
	// scoped says whether the CLI takes --scope for the registration.
	scoped bool
	// userServer says the registration is always for the user: a project
	// scope only moves the skill.
	userServer bool
	// configTimeout says the tool cuts a call at 60 s unless its config
	// file sets tool_timeout_sec.
	configTimeout bool
}

var (
	claudeTool = mcpTool{
		name: "claude", skillsDir: filepath.Join(".claude", "skills"), scoped: true,
		installHint: "install Claude Code (https://docs.claude.com/en/docs/claude-code) and run this again",
	}
	codexTool = mcpTool{
		name: "codex", skillsDir: filepath.Join(".agents", "skills"), userServer: true, configTimeout: true,
		installHint: "install Codex (npm install -g @openai/codex) and run this again",
	}
)

// mcpInstallResult is what install and uninstall report.
type mcpInstallResult struct {
	Tool     string      `json:"tool"`
	Scope    string      `json:"scope"`
	Profile  string      `json:"profile,omitempty"`
	Binary   string      `json:"binary,omitempty"`
	DryRun   bool        `json:"dry_run"`
	Commands [][]string  `json:"commands"`
	Timeout  *mcpTimeout `json:"tool_timeout,omitempty"`
	Skill    mcpSkill    `json:"skill"`
	Notes    []string    `json:"notes,omitempty"`
}

type mcpTimeout struct {
	Path    string `json:"path"`
	Seconds int    `json:"seconds"`
	State   string `json:"state"`
}

type mcpSkill struct {
	Path  string `json:"path"`
	State string `json:"state"`
}

func newMCPCmd() *cobra.Command {
	cmd := &cobra.Command{Use: "mcp", Short: "Use CodeFabric from Claude Code or Codex through MCP"}
	cmd.AddCommand(newMCPInstallCmd(), newMCPUninstallCmd())
	return cmd
}

type mcpFlags struct {
	claude, codex  bool
	scope, profile string
	dryRun         bool
}

func (f *mcpFlags) bind(cmd *cobra.Command, withProfile bool) {
	fl := cmd.Flags()
	fl.BoolVar(&f.claude, "claude", false, "Claude Code")
	fl.BoolVar(&f.codex, "codex", false, "Codex")
	fl.StringVar(&f.scope, "scope", "user", "user (this machine) or project (this repository). Codex registers MCP servers for the user only: with project, the skill goes to the repository and the server stays global")
	if withProfile {
		fl.StringVar(&f.profile, "profile", "execute", "tools the model gets: read, execute or modify")
		fl.BoolVar(&f.dryRun, "dry-run", false, "print what would be done, and do nothing")
	}
}

func (f *mcpFlags) tool() (mcpTool, error) {
	switch {
	case f.claude == f.codex:
		return mcpTool{}, usageError(errors.New("choose exactly one of --claude and --codex"))
	case f.claude:
		return claudeTool, nil
	}
	return codexTool, nil
}

func (f *mcpFlags) validate(withProfile bool) error {
	if err := validScope(f.scope); err != nil {
		return err
	}
	if withProfile && !slices.Contains(mcpProfiles, f.profile) {
		return usageError(fmt.Errorf("--profile must be one of %s, not %q", strings.Join(mcpProfiles, ", "), f.profile))
	}
	return nil
}

func newMCPInstallCmd() *cobra.Command {
	var f mcpFlags
	cmd := &cobra.Command{
		Use:   "install --claude|--codex [--scope user|project] [--profile read|execute|modify] [--dry-run]",
		Short: "Register `cf mcp serve` in Claude Code or Codex and install the codefabric skill",
		Long: "Registers `cf mcp serve` with the CLI of the tool (claude mcp add, codex mcp add) and writes the\n" +
			"codefabric skill where the tool reads skills. It is idempotent: it replaces the registration and\n" +
			"leaves the skill alone when it is already current. For Codex it also sets tool_timeout_sec so that\n" +
			"a long events_wait is not cut at 60 s.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			tool, err := f.tool()
			if err != nil {
				return err
			}
			if err := f.validate(true); err != nil {
				return err
			}
			res, err := installMCP(cmd.Context(), tool, f)
			if err != nil {
				return err
			}
			return emit(cmd, res, func(w io.Writer) error { return printMCPResult(w, res, "install") })
		},
	}
	f.bind(cmd, true)
	return cmd
}

func newMCPUninstallCmd() *cobra.Command {
	var f mcpFlags
	cmd := &cobra.Command{
		Use:   "uninstall --claude|--codex [--scope user|project]",
		Short: "Remove the registration of `cf mcp serve` and the codefabric skill",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			tool, err := f.tool()
			if err != nil {
				return err
			}
			if err := f.validate(false); err != nil {
				return err
			}
			res, err := uninstallMCP(cmd.Context(), tool, f)
			if err != nil {
				return err
			}
			return emit(cmd, res, func(w io.Writer) error { return printMCPResult(w, res, "uninstall") })
		},
	}
	f.bind(cmd, false)
	return cmd
}

func installMCP(ctx context.Context, tool mcpTool, f mcpFlags) (mcpInstallResult, error) {
	res := mcpInstallResult{Tool: tool.name, Scope: f.scope, Profile: f.profile, DryRun: f.dryRun}
	bin, err := cfBinary()
	if err != nil {
		return res, err
	}
	res.Binary = bin
	base, err := skillsBase(f.scope, tool.skillsDir)
	if err != nil {
		return res, err
	}
	skillPath := skillFilePath(tool, base)
	res.Skill.Path = skillPath
	cliPath, err := lookTool(tool, f.dryRun)
	if err != nil {
		return res, err
	}
	res.Commands = [][]string{
		removeArgs(tool, f.scope),
		addArgs(tool, f.scope, bin, f.profile),
	}
	if tool.userServer && f.scope == "project" {
		res.Notes = append(res.Notes, "Codex has no project scope for MCP servers: the server is registered for the user, only the skill goes to the repository")
	}
	var cfgPath string
	if tool.configTimeout {
		if cfgPath, err = codexConfigPath(); err != nil {
			return res, err
		}
		res.Timeout = &mcpTimeout{Path: cfgPath, Seconds: codexToolTimeoutSec, State: "would set"}
	}
	if f.dryRun {
		res.Skill.State = "would write"
		return res, nil
	}
	dir := cliDir(tool, f.scope, base)
	if err := runRemove(ctx, cliPath, dir, res.Commands[0]); err != nil {
		return res, err
	}
	if err := runCLI(ctx, cliPath, dir, res.Commands[1]); err != nil {
		return res, err
	}
	if tool.configTimeout {
		changed, err := setCodexToolTimeout(cfgPath, codexToolTimeoutSec)
		if err != nil {
			return res, err
		}
		res.Timeout.State = changedState(changed, "set", "unchanged")
	}
	res.Skill.State, err = writeSkill(skillPath, codefabricSkill)
	return res, err
}

func uninstallMCP(ctx context.Context, tool mcpTool, f mcpFlags) (mcpInstallResult, error) {
	res := mcpInstallResult{Tool: tool.name, Scope: f.scope}
	base, err := skillsBase(f.scope, tool.skillsDir)
	if err != nil {
		return res, err
	}
	skillPath := skillFilePath(tool, base)
	res.Skill.Path = skillPath
	switch cliPath, lookErr := exec.LookPath(tool.name); {
	case tool.userServer && f.scope == "project":
		res.Notes = append(res.Notes, "the "+tool.name+" server is registered for the user, not for this repository, so it was left alone; to remove it for every repository run `cf mcp uninstall --"+tool.name+"`")
	case lookErr != nil:
		res.Notes = append(res.Notes, tool.name+" is not in PATH: the registration was not touched")
	default:
		res.Commands = [][]string{removeArgs(tool, f.scope)}
		if err := runRemove(ctx, cliPath, cliDir(tool, f.scope, base), res.Commands[0]); err != nil {
			return res, err
		}
	}
	res.Skill.State, err = removeSkill(skillPath)
	return res, err
}

func changedState(changed bool, yes, no string) string {
	if changed {
		return yes
	}
	return no
}

// removeArgs and addArgs are the arguments after the CLI name. Claude always
// gets --scope: without it the server would go to the local scope.
func removeArgs(tool mcpTool, scope string) []string {
	args := []string{tool.name, "mcp", "remove"}
	if tool.scoped {
		args = append(args, "--scope", scope)
	}
	return append(args, mcpServerName)
}

func addArgs(tool mcpTool, scope, bin, profile string) []string {
	args := []string{tool.name, "mcp", "add"}
	if tool.scoped {
		args = append(args, "--scope", scope)
	}
	return append(args, mcpServerName, "--", bin, "mcp", "serve", "--profile", profile)
}

// lookTool finds the CLI of the tool. A dry run does not need it to exist.
func lookTool(tool mcpTool, dryRun bool) (string, error) {
	path, err := exec.LookPath(tool.name)
	if err != nil && !dryRun {
		return "", domain.NewError(domain.CodeNotFound, tool.name+" is not in PATH: "+tool.installHint, nil)
	}
	if err != nil {
		return tool.name, nil
	}
	return path, nil
}

// cfBinary is the path the tool will launch. The cf found in PATH wins when
// it is this very binary (a stable path such as a Homebrew link survives
// upgrades); else the running executable. Symlinks are never resolved.
func cfBinary() (string, error) {
	self, err := os.Executable()
	if err != nil {
		return "", fmt.Errorf("finding the cf binary: %w", err)
	}
	if found, err := exec.LookPath("cf"); err == nil {
		if abs, err := filepath.Abs(found); err == nil && sameFile(abs, self) {
			return abs, nil
		}
	}
	return self, nil
}

func sameFile(a, b string) bool {
	ia, err := os.Stat(a)
	if err != nil {
		return false
	}
	ib, err := os.Stat(b)
	return err == nil && os.SameFile(ia, ib)
}

func runCLI(ctx context.Context, cliPath, dir string, argv []string) error {
	out, err := runOutput(ctx, cliPath, dir, argv)
	if err != nil {
		return fmt.Errorf("%s failed: %w: %s", strings.Join(argv, " "), err, strings.TrimSpace(out))
	}
	return nil
}

// runRemove runs a `mcp remove`; a server that was not registered is fine.
func runRemove(ctx context.Context, cliPath, dir string, argv []string) error {
	out, err := runOutput(ctx, cliPath, dir, argv)
	if err != nil && !strings.Contains(strings.ToLower(out), "no mcp server") {
		return fmt.Errorf("%s failed: %w: %s", strings.Join(argv, " "), err, strings.TrimSpace(out))
	}
	return nil
}

func runOutput(ctx context.Context, cliPath, dir string, argv []string) (string, error) {
	cmd := exec.CommandContext(ctx, cliPath, argv[1:]...) //nolint:gosec // cliPath comes from LookPath of a fixed name; the arguments are built here
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	return string(out), err
}

// --- skill ---

// scopeBase is the directory a scope lives under: the home for the user, the
// root of the git repository of the cwd for the project.
func scopeBase(scope string) (string, error) {
	if scope == "project" {
		return repoRoot()
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("finding the home directory: %w", err)
	}
	return home, nil
}

func validScope(scope string) error {
	if scope != "user" && scope != "project" {
		return usageError(fmt.Errorf("--scope must be user or project, not %q", scope))
	}
	return nil
}

// skillsBase is scopeBase for a command that writes skills under skillsDir.
// A repository is untrusted: when a directory on the way to the skill is a
// symlink the write could land outside it, so the project scope refuses.
// (The home of the user may link its dotfiles anywhere.)
func skillsBase(scope, skillsDir string) (string, error) {
	base, err := scopeBase(scope)
	if err != nil || scope != "project" {
		return base, err
	}
	for dir := skillsDir; dir != "." && dir != string(filepath.Separator); dir = filepath.Dir(dir) {
		path := filepath.Join(base, dir)
		if info, err := os.Lstat(path); err == nil && info.Mode()&fs.ModeSymlink != 0 {
			return "", symlinkError(path)
		}
	}
	return base, nil
}

// skillFilePath is where the tool reads the skill under a scope base.
func skillFilePath(tool mcpTool, base string) string {
	return filepath.Join(base, tool.skillsDir, skillName, "SKILL.md")
}

// cliDir is where the CLI of the tool runs. Claude writes .mcp.json of the
// project scope in its cwd, which has to be the root of the repository, where
// the skill goes. Otherwise the cwd is of no consequence.
func cliDir(tool mcpTool, scope, base string) string {
	if tool.scoped && scope == "project" {
		return base
	}
	return ""
}

// repoRoot is the closest ancestor of the cwd that has a .git (a directory,
// or a file in a worktree).
func repoRoot() (string, error) {
	dir, err := os.Getwd()
	if err != nil {
		return "", err
	}
	for {
		if _, err := os.Lstat(filepath.Join(dir, ".git")); err == nil {
			return dir, nil
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", usageError(errors.New("--scope project needs to run inside a git repository"))
		}
		dir = parent
	}
}

func symlinkError(path string) error {
	return fmt.Errorf("%s is a symlink: refusing to write through it; remove it or install with another scope", path)
}

// writeSkill writes the skill atomically and reports "written" or "unchanged".
// It never writes through a symlink.
func writeSkill(path string, content []byte) (string, error) {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(filepath.Dir(dir), 0o755); err != nil { //nolint:gosec // skills directories are meant to be readable
		return "", err
	}
	info, err := os.Lstat(dir)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		if err := os.Mkdir(dir, 0o755); err != nil { //nolint:gosec // as above
			return "", err
		}
	case err != nil:
		return "", err
	case info.Mode()&fs.ModeSymlink != 0:
		return "", symlinkError(dir)
	case !info.IsDir():
		return "", fmt.Errorf("%s is not a directory", dir)
	}
	switch current, err := readRegular(path); {
	case err == nil && bytes.Equal(current, content):
		return "unchanged", nil
	case err != nil && !errors.Is(err, fs.ErrNotExist):
		return "", err
	}
	if err := writeAtomic(path, content, 0o644); err != nil { //nolint:gosec // the skill is not a secret
		return "", err
	}
	return "written", nil
}

// readRegular reads a file that is not a symlink.
func readRegular(path string) ([]byte, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if info.Mode()&fs.ModeSymlink != 0 {
		return nil, symlinkError(path)
	}
	return os.ReadFile(path) //nolint:gosec // a fixed path under the skills directory
}

// removeSkill removes the SKILL.md and, if it is left empty, its directory.
func removeSkill(path string) (string, error) {
	dir := filepath.Dir(path)
	info, err := os.Lstat(dir)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return "absent", nil
	case err != nil:
		return "", err
	case info.Mode()&fs.ModeSymlink != 0:
		return "", symlinkError(dir)
	}
	state := "removed"
	if fi, err := os.Lstat(path); err == nil {
		if fi.Mode()&fs.ModeSymlink != 0 {
			return "", symlinkError(path)
		}
		if err := os.Remove(path); err != nil {
			return "", err
		}
	} else if errors.Is(err, fs.ErrNotExist) {
		state = "absent"
	} else {
		return "", err
	}
	entries, err := os.ReadDir(dir)
	if err == nil && len(entries) == 0 {
		err = os.Remove(dir)
	}
	return state, err
}

// writeAtomic writes through a temporary file in the same directory and a
// rename, so a reader sees the old or the new content.
func writeAtomic(path string, data []byte, perm fs.FileMode) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), filepath.Base(path)+".tmp-*")
	if err != nil {
		return err
	}
	name := tmp.Name()
	fail := func(err error) error {
		_ = tmp.Close()
		_ = os.Remove(name)
		return err
	}
	if err := tmp.Chmod(perm); err != nil {
		return fail(err)
	}
	if _, err := tmp.Write(data); err != nil {
		return fail(err)
	}
	if err := tmp.Sync(); err != nil {
		return fail(err)
	}
	if err := tmp.Close(); err != nil {
		return fail(err)
	}
	if err := os.Rename(name, path); err != nil {
		_ = os.Remove(name)
		return err
	}
	syncDir(filepath.Dir(path))
	return nil
}

// syncDir makes a rename durable. It is best effort: not every filesystem
// can sync a directory, and the old content is still there if it is lost.
func syncDir(dir string) {
	d, err := os.Open(dir) //nolint:gosec // the directory of a file just written
	if err != nil {
		return
	}
	_ = d.Sync()
	_ = d.Close()
}

// --- codex config.toml ---

func codexConfigPath() (string, error) {
	if home := os.Getenv("CODEX_HOME"); home != "" {
		return filepath.Join(home, "config.toml"), nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("finding the home directory: %w", err)
	}
	return filepath.Join(home, ".codex", "config.toml"), nil
}

// setCodexToolTimeout sets tool_timeout_sec in the [mcp_servers.cf] table of
// the Codex config and reports whether the file changed. `codex mcp add`
// rewrites that table and drops the key, so it goes in after it.
func setCodexToolTimeout(path string, seconds int) (bool, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return false, fmt.Errorf("reading the Codex config: %w", err)
	}
	if info.Mode()&fs.ModeSymlink != 0 {
		return false, fmt.Errorf("%s is a symlink: add `tool_timeout_sec = %d` to its [mcp_servers.cf] table by hand", path, seconds)
	}
	data, err := os.ReadFile(path) //nolint:gosec // the Codex config of this user
	if err != nil {
		return false, err
	}
	updated, err := withToolTimeout(string(data), seconds)
	if err != nil {
		return false, fmt.Errorf("%s: %w", path, err)
	}
	if updated == string(data) {
		return false, nil
	}
	return true, writeAtomic(path, []byte(updated), info.Mode().Perm())
}

var (
	tomlCfTable = regexp.MustCompile(`^\s*\[\s*mcp_servers\s*\.\s*(cf|"cf"|'cf')\s*\]\s*(#.*)?$`)
	tomlTimeout = regexp.MustCompile(`^\s*tool_timeout_sec\s*=`)
)

// tomlLine says what a line of a TOML file is, as far as this file needs.
type tomlLine struct {
	header  bool // opens a table: [x], [[x]], with or without a comment
	inValue bool // continues a multi-line array or string
}

// classifyTOML marks the table headers. A header is any line that starts
// with `[`, whatever its name holds ([projects."/a b/ç"]), unless a multi-line
// array or string is open: there a `[` belongs to the value.
func classifyTOML(lines []string) []tomlLine {
	var open tomlValue
	kinds := make([]tomlLine, len(lines))
	for i, l := range lines {
		kinds[i].inValue = open.pending()
		if !open.pending() && strings.HasPrefix(strings.TrimSpace(l), "[") {
			kinds[i].header = true
			continue
		}
		open.feed(l)
	}
	return kinds
}

// tomlValue follows the multi-line values that hide a header from view.
type tomlValue struct {
	multi string // the closing delimiter of an open multi-line string
	depth int    // open brackets of a multi-line array
}

func (v *tomlValue) pending() bool { return v.multi != "" || v.depth > 0 }

func (v *tomlValue) feed(line string) {
	for i := 0; i < len(line); i++ {
		c := line[i]
		if v.multi != "" {
			switch {
			case c == '\\' && v.multi == `"""`:
				i++
			case strings.HasPrefix(line[i:], v.multi):
				v.multi = ""
				i += 2
			}
			continue
		}
		switch {
		case c == '#':
			return
		case strings.HasPrefix(line[i:], `"""`), strings.HasPrefix(line[i:], `'''`):
			v.multi = line[i : i+3]
			i += 2
		case c == '"' || c == '\'':
			i = endOfTOMLString(line, i)
		case c == '[':
			v.depth++
		case c == ']' && v.depth > 0:
			v.depth--
		}
	}
}

// endOfTOMLString is the index of the quote that closes the string opened at i.
func endOfTOMLString(line string, i int) int {
	quote := line[i]
	for i++; i < len(line); i++ {
		switch {
		case line[i] == '\\' && quote == '"':
			i++
		case line[i] == quote:
			return i
		}
	}
	return len(line)
}

// withToolTimeout edits only the lines of the [mcp_servers.cf] table. The key
// goes after the last key of the table, so the comments that introduce the next
// table stay with it.
func withToolTimeout(content string, seconds int) (string, error) {
	lines := strings.Split(content, "\n")
	kinds := classifyTOML(lines)
	start := -1
	for i, l := range lines {
		if kinds[i].header && tomlCfTable.MatchString(l) {
			start = i
			break
		}
	}
	if start < 0 {
		return "", errors.New("no [mcp_servers.cf] table: `codex mcp add` did not write it")
	}
	end := len(lines)
	for i := start + 1; i < len(lines); i++ {
		if kinds[i].header {
			end = i
			break
		}
	}
	eol := ""
	if strings.HasSuffix(lines[start], "\r") {
		eol = "\r"
	}
	line := fmt.Sprintf("tool_timeout_sec = %d", seconds) + eol
	for i := start + 1; i < end; i++ {
		if !kinds[i].inValue && tomlTimeout.MatchString(lines[i]) {
			lines[i] = line
			return strings.Join(lines, "\n"), nil
		}
	}
	last := end - 1
	for last > start && isBlankOrComment(lines[last]) {
		last--
	}
	lines = slices.Insert(lines, last+1, line)
	return strings.Join(lines, "\n"), nil
}

func isBlankOrComment(line string) bool {
	t := strings.TrimSpace(line)
	return t == "" || strings.HasPrefix(t, "#")
}

// --- output ---

func printMCPResult(w io.Writer, res mcpInstallResult, verb string) error {
	var b strings.Builder
	prefix := ""
	if res.DryRun {
		prefix = "[dry-run] "
		fmt.Fprintf(&b, "%swould %s %s (scope %s, profile %s, binary %s)\n", prefix, verb, res.Tool, res.Scope, res.Profile, res.Binary)
	}
	for _, c := range res.Commands {
		fmt.Fprintf(&b, "%srun: %s\n", prefix, strings.Join(c, " "))
	}
	if res.Timeout != nil {
		fmt.Fprintf(&b, "%s%s: %s tool_timeout_sec = %d in [mcp_servers.%s]\n", prefix, res.Timeout.Path, res.Timeout.State, res.Timeout.Seconds, mcpServerName)
	}
	fmt.Fprintf(&b, "%sskill %s: %s\n", prefix, res.Skill.Path, res.Skill.State)
	for _, n := range res.Notes {
		fmt.Fprintf(&b, "note: %s\n", n)
	}
	_, err := io.WriteString(w, b.String())
	return err
}
