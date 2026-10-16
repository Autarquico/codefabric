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
	"github.com/Autarquico/codefabric/internal/adapters/harness/localhost"
	"github.com/Autarquico/codefabric/internal/domain"
)

func spec(dir, prompt string, mode domain.Mode) harness.RunSpec {
	return harness.RunSpec{RunID: "r1", LeaseEpoch: 1, RunDir: dir, Worktree: dir, Prompt: prompt, Mode: mode}
}

func TestStartWritesThePromptAsAScriptAndRunsItWithShell(t *testing.T) {
	dir := t.TempDir()
	var a harness.Adapter = command.Adapter{}
	launch, err := a.Start(t.Context(), nil, spec(dir, "echo \"it's $((1+1))\"; exit 3", domain.ModeHeadless))
	if err != nil {
		t.Fatal(err)
	}
	script := filepath.Join(dir, command.ScriptFile)
	if !slices.Equal(launch.Argv, []string{"/bin/sh", script}) || len(launch.Env) != 0 || len(launch.SecretEnv) != 0 {
		t.Fatalf("launch = %+v", launch)
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

func TestStartIsIdempotent(t *testing.T) {
	dir := t.TempDir()
	s := spec(dir, "echo once", domain.ModeInteractive)
	first, err := command.Adapter{}.Start(t.Context(), nil, s)
	if err != nil {
		t.Fatal(err)
	}
	before, _ := os.ReadFile(filepath.Join(dir, command.ScriptFile)) //nolint:gosec // a file of the test
	again, err := command.Adapter{}.Start(t.Context(), nil, s)
	if err != nil {
		t.Fatal(err)
	}
	after, _ := os.ReadFile(filepath.Join(dir, command.ScriptFile)) //nolint:gosec // a file of the test
	entries, _ := os.ReadDir(dir)
	if !slices.Equal(first.Argv, again.Argv) || string(before) != string(after) || len(entries) != 1 {
		t.Errorf("a second Start gave %+v and %q (was %+v and %q), %d files", again, after, first, before, len(entries))
	}
}

func TestItIsCalledCommandAndSupportsBothModesAndNoConfiguration(t *testing.T) {
	a := command.Adapter{}
	caps := a.Capabilities()
	if a.ID() != "command" || !caps.Supports(domain.ModeHeadless) || !caps.Supports(domain.ModeInteractive) {
		t.Errorf("%s %v", a.ID(), caps.Modes)
	}
	if caps.Resume || caps.NativeCheckpoint || caps.NeedsInputSignal || caps.StructuredInput || caps.UsageEvents || caps.ModelSelection || caps.ConfigIsolation {
		t.Errorf("it declares more than its modes: %+v", caps)
	}
}

func TestAnEmptyPromptOrAnUnknownModeIsRefusedWithoutWriting(t *testing.T) {
	dir := t.TempDir()
	if _, err := (command.Adapter{}).Start(t.Context(), nil, spec(dir, "", domain.ModeHeadless)); err == nil {
		t.Error("an empty prompt was accepted")
	}
	if _, err := (command.Adapter{}).Start(t.Context(), nil, spec(dir, "echo", "batch")); !errors.Is(err, harness.ErrUnsupportedMode) {
		t.Errorf("an unknown mode: %v", err)
	}
	if entries, _ := os.ReadDir(dir); len(entries) != 0 {
		t.Errorf("a refused Start left %d files", len(entries))
	}
}

func TestWhatItDoesNotDoIsUnsupported(t *testing.T) {
	a, ctx, h := command.Adapter{}, t.Context(), harness.Handle{}
	_, resume := a.Resume(ctx, nil, harness.RunSpec{}, harness.Checkpoint{})
	_, snapshot := a.SnapshotConfig(ctx, nil, harness.SnapshotOptions{})
	_, apply := a.ApplyConfig(ctx, nil, harness.NativeSnapshot{}, harness.ApplyOptions{})
	_, diff := a.DiffConfig(ctx, nil, harness.NativeSnapshot{})
	_, checkpoint := a.Checkpoint(ctx, nil, h)
	_, usage := a.CollectUsage(ctx, nil, h)
	for name, err := range map[string]error{
		"resume": resume, "snapshot": snapshot, "apply": apply, "diff": diff, "checkpoint": checkpoint, "usage": usage,
		"stop": a.Stop(ctx, nil, h), "input": a.SendInput(ctx, nil, h, harness.Input{}),
	} {
		if !errors.Is(err, harness.ErrUnsupported) {
			t.Errorf("%s: %v", name, err)
		}
	}
}

// Detect looks at the shell Start runs with, not at the PATH of the host: a
// host with an sh on its PATH and no /bin/sh would otherwise be announced and
// fail at exec.
func TestDetectFindsTheShellItsRunsAreStartedWith(t *testing.T) {
	noPath, err := localhost.New(localhost.Options{Home: t.TempDir(), Env: []string{"PATH="}})
	if err != nil {
		t.Fatal(err)
	}
	d, err := command.Adapter{}.Detect(t.Context(), noPath)
	if err != nil || !d.Installed || d.Path != command.DefaultShell {
		t.Errorf("Detect on a host with no PATH = %+v, %v; want %s", d, err, command.DefaultShell)
	}
	dir := t.TempDir()
	launch, err := command.Adapter{}.Start(t.Context(), nil, spec(dir, "true", domain.ModeHeadless))
	if err != nil || launch.Argv[0] != d.Path {
		t.Errorf("Start runs %v (%v), Detect found %s", launch.Argv, err, d.Path)
	}
}

func TestAShellThatIsMissingOrNotExecutableIsNotInstalled(t *testing.T) {
	dir := t.TempDir()
	notExecutable := filepath.Join(dir, "sh")
	if err := os.WriteFile(notExecutable, []byte("#!/bin/sh\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	for name, shell := range map[string]string{
		"missing": filepath.Join(dir, "absent"), "not executable": notExecutable, "a directory": dir,
	} {
		d, err := command.Adapter{Shell: shell}.Detect(t.Context(), nil)
		if err != nil || d.Installed || d.Path != "" {
			t.Errorf("%s: Detect = %+v, %v; want not installed and no error", name, d, err)
		}
	}
}
