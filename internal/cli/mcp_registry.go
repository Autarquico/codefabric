package cli

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
)

// mcpRegistration is a server as a tool has it registered: what it launches.
type mcpRegistration struct {
	Command string   `json:"command" toml:"command"`
	Args    []string `json:"args" toml:"args"`
}

// mcpServeArgs are the arguments after the binary that `mcp install` registers.
func mcpServeArgs(profile string) []string {
	return []string{"mcp", "serve", "--profile", profile}
}

// isCodeFlock says whether the registration launches `cflk mcp serve` (or the
// `cf` of the beta.6) the way `mcp install` writes it: the arguments are
// `mcp serve --profile <profile>` and the executable is the one being
// installed (bin) or is called cflk or cf. A Cloud Foundry `cf` registered
// under that name does not match.
func (r mcpRegistration) isCodeFlock(bin string) bool {
	if name := filepath.Base(r.Command); name != "cflk" && name != "cf" && r.Command != bin {
		return false
	}
	return len(r.Args) == 4 && slices.Contains(mcpProfiles, r.Args[3]) && slices.Equal(r.Args, mcpServeArgs(r.Args[3]))
}

// readRegistrations is every server registered under the scope, by name, read
// from the file where the tool keeps them: ~/.claude.json (user) or .mcp.json
// of the repository (project) for Claude Code, config.toml for Codex.
func readRegistrations(tool mcpTool, scope, base string) (map[string]mcpRegistration, error) {
	if tool.userServer {
		return readCodexRegistrations()
	}
	return readClaudeRegistrations(claudeServersFile(scope, base))
}

// claudeServersFile is the file Claude Code keeps the servers of a scope in.
func claudeServersFile(scope, base string) string {
	if scope == "project" {
		return filepath.Join(base, ".mcp.json")
	}
	if dir := os.Getenv("CLAUDE_CONFIG_DIR"); dir != "" {
		return filepath.Join(dir, ".claude.json")
	}
	return filepath.Join(base, ".claude.json")
}

func readClaudeRegistrations(path string) (map[string]mcpRegistration, error) {
	data, err := os.ReadFile(path) //nolint:gosec // the Claude Code config of this user or .mcp.json of the repository
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var file struct {
		Servers map[string]mcpRegistration `json:"mcpServers"`
	}
	if err := json.Unmarshal(data, &file); err != nil {
		return nil, fmt.Errorf("reading the MCP servers in %s: %w", path, err)
	}
	return file.Servers, nil
}

func readCodexRegistrations() (map[string]mcpRegistration, error) {
	path, err := codexConfigPath()
	if err != nil {
		return nil, err
	}
	data, err := os.ReadFile(path) //nolint:gosec // the Codex config of this user
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return parseCodexRegistrations(string(data)), nil
}

var (
	tomlServerHeader = regexp.MustCompile(`^\s*\[\s*mcp_servers\s*\.\s*("[^"]+"|'[^']+'|[A-Za-z0-9_-]+)\s*\]\s*(#.*)?$`)
	tomlLaunchKey    = regexp.MustCompile(`^\s*(command|args)\s*=\s*(.*)$`)
	tomlString       = regexp.MustCompile(`"((?:[^"\\]|\\.)*)"|'([^']*)'`)
)

// parseCodexRegistrations reads command and args of each [mcp_servers.<name>]
// table, which is all `codex mcp add` writes of a stdio server. Anything it
// cannot read comes out as a registration that is not Code Flock's, and so
// is never touched.
func parseCodexRegistrations(content string) map[string]mcpRegistration {
	lines := strings.Split(content, "\n")
	kinds := classifyTOML(lines)
	servers := map[string]mcpRegistration{}
	current := ""
	for i := 0; i < len(lines); i++ {
		line := strings.TrimSuffix(lines[i], "\r")
		switch {
		case kinds[i].header:
			current = ""
			if m := tomlServerHeader.FindStringSubmatch(line); m != nil {
				current = strings.Trim(m[1], `"'`)
				servers[current] = mcpRegistration{}
			}
		case current == "" || kinds[i].inValue:
		default:
			m := tomlLaunchKey.FindStringSubmatch(line)
			if m == nil {
				continue
			}
			value, open := m[2], tomlValue{}
			open.feed(m[2])
			for open.pending() && i+1 < len(lines) {
				i++
				value += "\n" + lines[i]
				open.feed(lines[i])
			}
			reg := servers[current]
			if m[1] == "command" {
				reg.Command = firstString(value)
			} else {
				reg.Args = allStrings(value)
			}
			servers[current] = reg
		}
	}
	return servers
}

func tomlStringValue(m []string) string {
	if m[2] != "" || strings.HasPrefix(m[0], "'") {
		return m[2]
	}
	if s, err := strconv.Unquote(`"` + m[1] + `"`); err == nil {
		return s
	}
	return m[1]
}

func firstString(value string) string {
	if m := tomlString.FindStringSubmatch(value); m != nil {
		return tomlStringValue(m)
	}
	return ""
}

func allStrings(value string) []string {
	var out []string
	for _, m := range tomlString.FindAllStringSubmatch(value, -1) {
		out = append(out, tomlStringValue(m))
	}
	return out
}
