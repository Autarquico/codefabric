// Package claude is the harness of Claude Code (Anthropic). This file is its
// identity; README.md documents it as §7.3 asks. The process of a Run is in
// start.go, its stream in events.go and its usage in usage.go.
package claude

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/Autarquico/codefabric/internal/adapters/harness"
	"github.com/Autarquico/codefabric/internal/domain"
)

// ID is what a Run asks for in requirements.harness.
const ID = "claude-code"

// Binary is the program looked up in the PATH of the host.
const Binary = "claude"

// ConfigHomeEnv names the variable that points Claude Code at its
// configuration directory: the ConfigDir of the Run.
const ConfigHomeEnv = "CLAUDE_CONFIG_DIR"

// OAuthTokenEnv is the variable a Run authenticates with: a token made by
// `claude setup-token`, delivered as a secret.
const OAuthTokenEnv = //nolint:gosec // a variable name, not a credential
"CLAUDE_CODE_OAUTH_TOKEN"

// OAuthTokenRef is the secret ref the Worker must hold for OAuthTokenEnv.
const OAuthTokenRef = //nolint:gosec // a secret ref, not a credential
"claude/oauth-token"

// Adapter is the harness `claude-code`: all of §7.2 but Resume,
// Checkpoint, Stop and SendInput, which stay unsupported.
type Adapter struct {
	harness.Unsupported
}

var (
	_ harness.Adapter        = Adapter{}
	_ harness.SecretRequirer = Adapter{}
)

// ID is "claude-code".
func (Adapter) ID() string { return ID }

// Capabilities are both modes, usage, model selection and a configuration
// directory per Run, and Install.
func (Adapter) Capabilities() harness.Capabilities {
	return harness.Capabilities{
		Modes:                  []domain.Mode{domain.ModeHeadless, domain.ModeInteractive},
		UsageEvents:            true,
		ModelSelection:         true,
		ConfigIsolation:        true,
		PreapprovedPermissions: true,
		Install:                true,
	}
}

// RequiredSecrets is the token of every Run: the Worker that lacks its ref is
// not eligible (§16.7). It says nothing of the login in ~/.claude, which
// Diagnose reports apart.
func (Adapter) RequiredSecrets() []harness.SecretEnv {
	return []harness.SecretEnv{{Name: OAuthTokenEnv, Ref: OAuthTokenRef}}
}

// Detect looks for the binary in the PATH of the host. Not finding it is
// Installed=false, not an error. It says nothing of authentication.
func (Adapter) Detect(_ context.Context, host harness.Host) (harness.Detection, error) {
	path, err := host.LookPath(Binary)
	if errors.Is(err, exec.ErrNotFound) {
		return harness.Detection{}, nil
	}
	if err != nil {
		return harness.Detection{}, fmt.Errorf("claude-code: detecting %s: %w", Binary, err)
	}
	return harness.Detection{Installed: true, Path: path}, nil
}

var versionPattern = regexp.MustCompile(`^(\d+\.\d+\.\d+\S*) \(Claude Code\)$`)

// Version runs `claude --version` and returns the number of "X.Y.Z (Claude Code)".
func (Adapter) Version(ctx context.Context, host harness.Host) (string, error) {
	res, err := host.Run(ctx, harness.Cmd{Argv: []string{Binary, "--version"}})
	if err != nil {
		return "", fmt.Errorf("claude-code: running %s --version: %w", Binary, err)
	}
	if res.ExitCode != 0 {
		return "", fmt.Errorf("claude-code: %s --version exited with %d", Binary, res.ExitCode)
	}
	out := strings.TrimSpace(string(res.Stdout))
	m := versionPattern.FindStringSubmatch(out)
	if m == nil {
		return "", fmt.Errorf("claude-code: unexpected version output %q", out)
	}
	return m[1], nil
}

// AuthDiagnosis is what the host has to authenticate a Run, apart from
// whether the binary is there.
type AuthDiagnosis struct {
	// Token is the secret every Run needs; whether the Worker holds it is for
	// its secret store to say, not the adapter.
	Token harness.SecretEnv
	// LoginFile says that ~/.claude/.credentials.json exists, which a Run is
	// linked to.
	LoginFile bool
}

// Diagnose reads the auth of the host. It does not read the credentials file.
func (a Adapter) Diagnose(host harness.Host) (AuthDiagnosis, error) {
	_, err := os.Lstat(a.GlobalAuth(host))
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return AuthDiagnosis{}, fmt.Errorf("claude-code: %w", err)
	}
	return AuthDiagnosis{Token: a.RequiredSecrets()[0], LoginFile: err == nil}, nil
}

// GlobalAuth is ~/.claude/.credentials.json.
func (Adapter) GlobalAuth(host harness.Host) string {
	return filepath.Join(globalDir(host), AuthFile)
}
