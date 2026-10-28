package codex_test

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"testing"

	"github.com/Autarquico/codefabric/internal/adapters/harness"
	"github.com/Autarquico/codefabric/internal/adapters/harness/codex"
	"github.com/Autarquico/codefabric/internal/adapters/harness/localhost"
	"github.com/Autarquico/codefabric/internal/domain"
)

func spec(mode domain.Mode) harness.RunSpec {
	return harness.RunSpec{RunID: "r1", LeaseEpoch: 1, Mode: mode, Prompt: "fix it", RunDir: "/run", ConfigDir: "/run/config", Worktree: "/wt"}
}

// startHost has a codex at <bin>/codex; Start names it by that path.
func startHost(t *testing.T) harness.Host {
	t.Helper()
	return hostWith(t, "#!/bin/sh\n")
}

func codexPath(t *testing.T, h harness.Host) string {
	t.Helper()
	p, err := h.LookPath(codex.Binary)
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func TestHeadlessStart(t *testing.T) {
	s := spec(domain.ModeHeadless)
	s.Model = "gpt-x"
	h := startHost(t)
	l, err := codex.Adapter{}.Start(t.Context(), h, s)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{
		codexPath(t, h), "exec", "--json", "--sandbox", "workspace-write", "-c", `approval_policy="never"`,
		"--skip-git-repo-check", "-C", "/wt", "-m", "gpt-x", "--", "fix it",
	}
	if !slices.Equal(l.Argv, want) || !slices.Equal(l.Env, []string{"CODEX_HOME=/run/config"}) || !l.StructuredStdout || len(l.SecretEnv) != 0 {
		t.Errorf("launch = %+v", l)
	}
}

func TestInteractiveStartWithoutModel(t *testing.T) {
	h := startHost(t)
	l, err := codex.Adapter{}.Start(t.Context(), h, spec(domain.ModeInteractive))
	if err != nil {
		t.Fatal(err)
	}
	want := []string{codexPath(t, h), "--sandbox", "workspace-write", "-a", "never", "-C", "/wt", "--", "fix it"}
	if !slices.Equal(l.Argv, want) || !slices.Equal(l.Env, []string{"CODEX_HOME=/run/config"}) || l.StructuredStdout {
		t.Errorf("launch = %+v", l)
	}
}

func TestPromptStartingWithDashStaysAfterTheSeparator(t *testing.T) {
	s := spec(domain.ModeHeadless)
	s.Prompt = "--dangerously-bypass-approvals-and-sandbox"
	l, err := codex.Adapter{}.Start(t.Context(), startHost(t), s)
	if err != nil {
		t.Fatal(err)
	}
	n := len(l.Argv)
	if l.Argv[n-2] != "--" || l.Argv[n-1] != s.Prompt {
		t.Errorf("argv = %v", l.Argv)
	}
}

func TestStartIsIdempotentAndWritesNothing(t *testing.T) {
	dir := t.TempDir()
	s := spec(domain.ModeHeadless)
	s.RunDir, s.ConfigDir = dir, filepath.Join(dir, "config")
	h := startHost(t)
	a, err := codex.Adapter{}.Start(t.Context(), h, s)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := codex.Adapter{}.Start(t.Context(), h, s)
	entries, _ := os.ReadDir(dir)
	if !slices.Equal(a.Argv, b.Argv) || len(entries) != 0 {
		t.Errorf("%v %v, %d files", a.Argv, b.Argv, len(entries))
	}
}

func TestStartRefusals(t *testing.T) {
	s := spec(domain.ModeHeadless)
	s.Prompt = ""
	if _, err := (codex.Adapter{}).Start(t.Context(), startHost(t), s); err == nil {
		t.Error("empty prompt accepted")
	}
	s = spec(domain.ModeHeadless)
	s.ConfigDir = ""
	if _, err := (codex.Adapter{}).Start(t.Context(), startHost(t), s); err == nil {
		t.Error("empty config dir accepted")
	}
	if _, err := (codex.Adapter{}).Start(t.Context(), startHost(t), spec("batch")); !errors.Is(err, harness.ErrUnsupportedMode) {
		t.Errorf("unknown mode: %v", err)
	}
}

func TestStartWithoutTheBinaryInTheHost(t *testing.T) {
	_, err := codex.Adapter{}.Start(t.Context(), hostWith(t, ""), spec(domain.ModeHeadless))
	if !errors.Is(err, exec.ErrNotFound) {
		t.Errorf("err = %v, want exec.ErrNotFound", err)
	}
}

func TestCapabilitiesAndUnsupported(t *testing.T) {
	a := codex.Adapter{}
	c := a.Capabilities()
	if a.ID() != "codex" || !c.Supports(domain.ModeHeadless) || !c.Supports(domain.ModeInteractive) ||
		!c.UsageEvents || !c.ModelSelection || !c.ConfigIsolation || c.Install || c.Resume {
		t.Errorf("%s %+v", a.ID(), c)
	}
	if _, err := a.Install(t.Context(), nil, harness.InstallOptions{}); !errors.Is(err, harness.ErrUnsupported) {
		t.Errorf("install: %v", err)
	}
}

func hostWith(t *testing.T, script string) harness.Host {
	t.Helper()
	bin := t.TempDir()
	if script != "" {
		if err := os.WriteFile(filepath.Join(bin, "codex"), []byte(script), 0o700); err != nil { //nolint:gosec // fake binary
			t.Fatal(err)
		}
	}
	h, err := localhost.New(localhost.Options{Home: t.TempDir(), Env: []string{"PATH=" + bin}})
	if err != nil {
		t.Fatal(err)
	}
	return h
}

func TestDetect(t *testing.T) {
	h := hostWith(t, "#!/bin/sh\n")
	d, err := codex.Adapter{}.Detect(t.Context(), h)
	if err != nil || !d.Installed || filepath.Base(d.Path) != "codex" {
		t.Errorf("%+v %v", d, err)
	}
	d, err = codex.Adapter{}.Detect(t.Context(), hostWith(t, ""))
	if err != nil || d.Installed {
		t.Errorf("missing: %+v %v", d, err)
	}
}

func TestVersion(t *testing.T) {
	v, err := codex.Adapter{}.Version(t.Context(), hostWith(t, "#!/bin/sh\necho 'codex-cli 0.160.1'\n"))
	if err != nil || v != "0.160.1" {
		t.Errorf("%q %v", v, err)
	}
	for name, script := range map[string]string{
		"garbage": "#!/bin/sh\necho hello\n",
		"exit":    "#!/bin/sh\nexit 2\n",
		"missing": "",
	} {
		if _, err := (codex.Adapter{}).Version(t.Context(), hostWith(t, script)); err == nil {
			t.Errorf("%s: no error", name)
		}
	}
}
