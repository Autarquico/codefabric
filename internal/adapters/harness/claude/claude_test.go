package claude_test

import (
	"errors"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/Autarquico/codefabric/internal/adapters/harness"
	"github.com/Autarquico/codefabric/internal/adapters/harness/claude"
	"github.com/Autarquico/codefabric/internal/adapters/harness/localhost"
	"github.com/Autarquico/codefabric/internal/domain"
)

func hostWithBinary(t *testing.T, script string) harness.Host {
	t.Helper()
	bin := t.TempDir()
	if script != "" {
		if err := os.WriteFile(filepath.Join(bin, claude.Binary), []byte(script), 0o700); err != nil { //nolint:gosec // an executable of the test
			t.Fatal(err)
		}
	}
	h, err := localhost.New(localhost.Options{Home: t.TempDir(), Path: bin})
	if err != nil {
		t.Fatal(err)
	}
	return h
}

func TestIdentityAndCapabilities(t *testing.T) {
	a := claude.Adapter{}
	if a.ID() != "claude-code" {
		t.Errorf("ID = %q", a.ID())
	}
	if _, err := harness.NewRegistry(a); err != nil {
		t.Error(err)
	}
	c := a.Capabilities()
	if !c.ConfigIsolation || c.Install || !slices.Equal(c.Modes, []domain.Mode{domain.ModeHeadless, domain.ModeInteractive}) {
		t.Errorf("capabilities = %+v", c)
	}
}

func TestInstallIsUnsupported(t *testing.T) {
	_, err := claude.Adapter{}.Install(t.Context(), hostWithBinary(t, ""), harness.InstallOptions{DryRun: true})
	if !errors.Is(err, harness.ErrUnsupported) {
		t.Errorf("Install: %v", err)
	}
}

func TestDetectFindsTheBinaryAndNotFindingItIsNotAnError(t *testing.T) {
	d, err := claude.Adapter{}.Detect(t.Context(), hostWithBinary(t, "#!/bin/sh\n"))
	if err != nil || !d.Installed || filepath.Base(d.Path) != "claude" {
		t.Errorf("detection = %+v, %v", d, err)
	}
	d, err = claude.Adapter{}.Detect(t.Context(), hostWithBinary(t, ""))
	if err != nil || d.Installed || d.Path != "" {
		t.Errorf("detection = %+v, %v", d, err)
	}
}

func TestVersion(t *testing.T) {
	h := hostWithBinary(t, "#!/bin/sh\necho '2.1.314 (Claude Code)'\n")
	if v, err := (claude.Adapter{}).Version(t.Context(), h); err != nil || v != "2.1.314" {
		t.Errorf("version = %q, %v", v, err)
	}
	for name, script := range map[string]string{
		"garbage":   "#!/bin/sh\necho hello\n",
		"exit":      "#!/bin/sh\nexit 3\n",
		"no claude": "",
	} {
		if v, err := (claude.Adapter{}).Version(t.Context(), hostWithBinary(t, script)); err == nil {
			t.Errorf("%s: version %q", name, v)
		}
	}
}

func TestTheTokenIsARequiredSecretAndAuthIsDiagnosedApartFromDetection(t *testing.T) {
	a := claude.Adapter{}
	want := []harness.SecretEnv{{Name: "CLAUDE_CODE_OAUTH_TOKEN", Ref: "claude/oauth-token"}}
	if got := a.RequiredSecrets(); !slices.Equal(got, want) {
		t.Errorf("RequiredSecrets = %v", got)
	}
	if !domain.ValidSecretRef(want[0].Ref) || !domain.ValidSecretEnvName(want[0].Name) {
		t.Error("the token ref or variable is not valid")
	}
	// A host with the binary and no login: detected, token declared, no login file.
	h := hostWithBinary(t, "#!/bin/sh\n")
	if d, _ := a.Detect(t.Context(), h); !d.Installed {
		t.Error("not detected")
	}
	if diag, err := a.Diagnose(h); err != nil || diag.LoginFile || diag.Token != want[0] {
		t.Errorf("diagnosis = %+v, %v", diag, err)
	}
	// A host with a login and no binary: not detected, login file present.
	h2, _ := newHost(t, homeFiles())
	if d, _ := a.Detect(t.Context(), h2); d.Installed {
		t.Error("detected without a binary")
	}
	if diag, _ := a.Diagnose(h2); !diag.LoginFile {
		t.Errorf("diagnosis = %+v", diag)
	}
}
