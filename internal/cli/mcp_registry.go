package cli

import (
	"debug/buildinfo"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"

	"github.com/pelletier/go-toml/v2"
)

// mcpRegistration is a server as a tool has it registered: what it launches.
type mcpRegistration struct {
	Command string
	Args    []string
	Env     map[string]string
	// Unsupported names the other settings of the entry (cwd, env_vars, ...):
	// `mcp add` cannot write them, so a move to another name would lose them.
	Unsupported []string
	// CustomTimeout: the entry has a tool_timeout_sec other than the one install sets.
	CustomTimeout bool
}

// ownSettings are the settings of the entry that `mcp add` would lose.
func (r mcpRegistration) ownSettings() []string {
	out := slices.Clone(r.Unsupported)
	if r.CustomTimeout {
		out = append(out, "tool_timeout_sec")
	}
	return out
}

func (r mcpRegistration) hasSettingsAddCannotWrite() bool { return len(r.ownSettings()) > 0 }

// notMovableTo says, empty when it can, what of r (the cf) would be lost by
// retiring it in favour of the cflk registration: settings `mcp add` cannot
// write, or environment variables that cflk does not have with the same value.
func (r mcpRegistration) notMovableTo(cflk mcpRegistration, hasCflk bool) string {
	if own := r.ownSettings(); len(own) > 0 {
		return "settings (" + strings.Join(own, ", ") + ")"
	}
	if !hasCflk {
		return ""
	}
	var missing []string
	for key, value := range r.Env {
		if got, ok := cflk.Env[key]; !ok || got != value {
			missing = append(missing, key)
		}
	}
	if len(missing) > 0 {
		slices.Sort(missing)
		return "environment variables that cflk does not have (" + strings.Join(missing, ", ") + ")"
	}
	return ""
}

// mcpServeArgs are the arguments after the binary that `mcp install` registers.
func mcpServeArgs(profile string) []string {
	return []string{"mcp", "serve", "--profile", profile}
}

// codeFlockModules are the Go modules the cflk and the cf of the beta.6 are built from.
var codeFlockModules = []string{"github.com/Autarquico/codeflock", "github.com/Autarquico/codefabric"}

// isCodeFlock says whether the registration launches `cflk mcp serve` (or the
// `cf` of the beta.6) the way `mcp install` writes it: the arguments are
// `mcp serve --profile <profile>` and the executable is the one being
// installed (bin) or a binary built from the module of Code Flock. The name
// of the file proves nothing: a Cloud Foundry `cf`, or any script called cf,
// does not match. The executable is read, never run: the registration may come
// from the .mcp.json of an untrusted repository.
func (r mcpRegistration) isCodeFlock(bin string) bool {
	if len(r.Args) != 4 || !slices.Contains(mcpProfiles, r.Args[3]) || !slices.Equal(r.Args, mcpServeArgs(r.Args[3])) {
		return false
	}
	return r.Command == bin || isCodeFlockExecutable(r.Command, bin)
}

// isStaleCodeFlock: an absolute path called cf or cflk that no longer exists
// (the install directory changed). Its arguments are the ones mcp install
// writes, so it is ours and expired. A relative command not found in PATH is not.
func isStaleCodeFlock(command string) bool {
	if !filepath.IsAbs(command) {
		return false
	}
	switch strings.TrimSuffix(strings.ToLower(filepath.Base(command)), ".exe") {
	case "cf", "cflk":
	default:
		return false
	}
	_, err := os.Lstat(command)
	return errors.Is(err, fs.ErrNotExist)
}

func isCodeFlockExecutable(command, bin string) bool {
	if isStaleCodeFlock(command) {
		return true
	}
	path, err := exec.LookPath(command)
	if err != nil {
		return false
	}
	if sameFile(path, bin) {
		return true
	}
	if info, err := os.Stat(path); err != nil || !info.Mode().IsRegular() {
		return false // buildinfo.ReadFile would block on a FIFO
	}
	bi, err := buildinfo.ReadFile(path)
	if err != nil {
		return false
	}
	return slices.ContainsFunc(codeFlockModules, func(module string) bool {
		return bi.Main.Path == module || strings.HasPrefix(bi.Path, module+"/")
	})
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
		if env, ok := fields["env"]; ok && json.Unmarshal(env, &reg.Env) != nil {
			reg.Env = nil
			reg.Unsupported = append(reg.Unsupported, "env")
		}
		reg.Unsupported = append(reg.Unsupported, unknownKeys(fields, "command", "args", "env", "type")...)
		servers[name] = reg
	}
	return servers, nil
}

// unknownKeys are the keys of fields that are not in known, sorted.
func unknownKeys[V any](fields map[string]V, known ...string) []string {
	var out []string
	for key := range fields {
		if !slices.Contains(known, key) {
			out = append(out, key)
		}
	}
	slices.Sort(out)
	return out
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
		if env, ok := fields["env"]; ok {
			if reg.Env = stringMap(env); reg.Env == nil {
				reg.Unsupported = append(reg.Unsupported, "env")
			}
		}
		if timeout, ok := fields["tool_timeout_sec"]; ok && timeout != int64(codexToolTimeoutSec) {
			reg.CustomTimeout = true
		}
		reg.Unsupported = append(reg.Unsupported, unknownKeys(fields, "command", "args", "env", "tool_timeout_sec")...)
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

// stringMap is the table of strings, or nil when it is anything else.
func stringMap(v any) map[string]string {
	table, ok := v.(map[string]any)
	if !ok {
		return nil
	}
	out := make(map[string]string, len(table))
	for key, item := range table {
		s, ok := item.(string)
		if !ok {
			return nil
		}
		out[key] = s
	}
	return out
}
