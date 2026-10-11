package command_test

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"testing"

	"github.com/Autarquico/codefabric/internal/adapters/harness"
	"github.com/Autarquico/codefabric/internal/adapters/harness/command"
	"github.com/Autarquico/codefabric/internal/domain"
)

func TestLaunchWritesThePromptAsAScriptAndRunsItWithShell(t *testing.T) {
	dir := t.TempDir()
	var l harness.Launcher = command.Launcher{}
	launch, err := l.Launch(harness.LaunchSpec{RunDir: dir, Worktree: dir, Prompt: "echo \"it's $((1+1))\"; exit 3", Mode: domain.ModeHeadless})
	if err != nil {
		t.Fatal(err)
	}
	script := filepath.Join(dir, command.ScriptFile)
	if !slices.Equal(launch.Argv, []string{"/bin/sh", script}) {
		t.Fatalf("argv = %v", launch.Argv)
	}
	info, err := os.Stat(script)
	if err != nil || info.Mode().Perm() != 0o700 {
		t.Errorf("script: %v %v", info, err)
	}
	out, err := exec.CommandContext(t.Context(), launch.Argv[0], launch.Argv[1:]...).Output() //nolint:gosec // the script of the test
	var exit *exec.ExitError
	if string(out) != "it's 2\n" || !errors.As(err, &exit) || exit.ExitCode() != 3 {
		t.Errorf("the script printed %q and ended with %v", out, err)
	}
}

func TestLauncherIsCalledCommandAndSupportsBothModes(t *testing.T) {
	l := command.Launcher{}
	if l.ID() != "command" || !slices.Contains(l.Modes(), domain.ModeHeadless) || !slices.Contains(l.Modes(), domain.ModeInteractive) {
		t.Errorf("%s %v", l.ID(), l.Modes())
	}
}

func TestAnEmptyPromptIsRefused(t *testing.T) {
	if _, err := (command.Launcher{}).Launch(harness.LaunchSpec{RunDir: t.TempDir()}); err == nil {
		t.Error("accepted")
	}
}
