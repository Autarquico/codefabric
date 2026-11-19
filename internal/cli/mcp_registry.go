package cli

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"

	"github.com/pelletier/go-toml/v2"
)

// mcpRegistration is a server as a tool has it registered: what it launches.
type mcpRegistration struct {
	Command string
	Args    []string
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
// of the repository (project) for Claude Code, config.toml for Codex. A file
// that cannot be parsed is an error, so that nothing is changed on a guess.
// The .mcp.json of a repository is untrusted and is not read through a symlink.
func readRegistrations(tool mcpTool, scope, base string) (map[string]mcpRegistration, error) {
	if tool.userServer {
		path, err := codexConfigPath()
		if err != nil {
			return nil, err
		}
		return readRegistrationsFile(path, os.ReadFile, parseCodexRegistrations)
	}
	read := os.ReadFile
	if scope == "project" {
		read = readRegular
	}
	return readRegistrationsFile(claudeServersFile(scope, base), read, parseClaudeRegistrations)
}

func readRegistrationsFile(path string, read func(string) ([]byte, error), parse func([]byte) (map[string]mcpRegistration, error)) (map[string]mcpRegistration, error) {
	data, err := read(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	servers, err := parse(data)
	if err != nil {
		return nil, fmt.Errorf("reading the MCP servers in %s: %w", path, err)
	}
	return servers, nil
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

// parseClaudeRegistrations reads `mcpServers`. Keys are matched as they are
// written, as Claude does: encoding/json would also take `MCPSERVERS` or `Command`.
// An entry that is not an object, or has a command or args of another type,
// comes out without them, which is never Code Flock's.
func parseClaudeRegistrations(data []byte) (map[string]mcpRegistration, error) {
	var file map[string]json.RawMessage
	if err := json.Unmarshal(data, &file); err != nil {
		return nil, err
	}
	var entries map[string]json.RawMessage
	if raw, ok := file["mcpServers"]; ok {
		if err := json.Unmarshal(raw, &entries); err != nil {
			return nil, err
		}
	}
	servers := make(map[string]mcpRegistration, len(entries))
	for name, raw := range entries {
		var fields map[string]json.RawMessage
		_ = json.Unmarshal(raw, &fields)
		var reg mcpRegistration
		if json.Unmarshal(fields["command"], &reg.Command) != nil {
			reg.Command = ""
		}
		if json.Unmarshal(fields["args"], &reg.Args) != nil {
			reg.Args = nil
		}
		servers[name] = reg
	}
	return servers, nil
}

// parseCodexRegistrations reads command and args of each server under
// [mcp_servers], in whatever form TOML allows: a [mcp_servers.<name>] table, an
// inline table, dotted keys. An entry that is not a table, or has a command or
// args of another type, comes out without them, which is never Code Flock's.
func parseCodexRegistrations(data []byte) (map[string]mcpRegistration, error) {
	var file struct {
		Servers map[string]any `toml:"mcp_servers"`
	}
	if err := toml.Unmarshal(data, &file); err != nil {
		return nil, err
	}
	servers := make(map[string]mcpRegistration, len(file.Servers))
	for name, entry := range file.Servers {
		fields, _ := entry.(map[string]any)
		var reg mcpRegistration
		reg.Command, _ = fields["command"].(string)
		reg.Args = stringList(fields["args"])
		servers[name] = reg
	}
	return servers, nil
}

// stringList is the list of strings, or nil when it is anything else.
func stringList(v any) []string {
	list, ok := v.([]any)
	if !ok {
		return nil
	}
	out := make([]string, 0, len(list))
	for _, item := range list {
		s, ok := item.(string)
		if !ok {
			return nil
		}
		out = append(out, s)
	}
	return out
}
