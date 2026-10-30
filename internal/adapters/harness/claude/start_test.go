package claude_test

import (
	"path/filepath"
	"slices"
	"testing"

	"github.com/Autarquico/codefabric/internal/adapters/harness"
	"github.com/Autarquico/codefabric/internal/adapters/harness/claude"
	"github.com/Autarquico/codefabric/internal/domain"
)

func spec(mode domain.Mode) harness.RunSpec {
	return harness.RunSpec{RunID: "r1", LeaseEpoch: 1, Mode: mode, Prompt: "fix it", RunDir: "/run", ConfigDir: "/run/config", Worktree: "/wt"}
}

func TestStartHeadless(t *testing.T) {
	h := hostWithBinary(t, "#!/bin/sh\n")
	bin, _ := h.LookPath(claude.Binary)
	run := spec(domain.ModeHeadless)
	run.Model = "opus"
	l, err := claude.Adapter{}.Start(t.Context(), h, run)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{
		bin, "-p", "--output-format", "stream-json", "--verbose", "--permission-mode", "bypassPermissions",
		"--disallowedTools", "AskUserQuestion", "--model", "opus", "--", "fix it",
	}
	if !slices.Equal(l.Argv, want) {
		t.Errorf("argv = %q", l.Argv)
	}
	if !l.StructuredStdout || !slices.Equal(l.Env, []string{"CLAUDE_CONFIG_DIR=/run/config"}) ||
		!slices.Equal(l.SecretEnv, claude.Adapter{}.RequiredSecrets()) {
		t.Errorf("launch = %+v", l)
	}
}

func TestStartInteractive(t *testing.T) {
	h := hostWithBinary(t, "#!/bin/sh\n")
	bin, _ := h.LookPath(claude.Binary)
	l, err := claude.Adapter{}.Start(t.Context(), h, spec(domain.ModeInteractive))
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{bin, "--permission-mode", "bypassPermissions", "--", "fix it"}; !slices.Equal(l.Argv, want) {
		t.Errorf("argv = %q", l.Argv)
	}
	if l.StructuredStdout || filepath.Base(l.Argv[0]) != claude.Binary || len(l.SecretEnv) != 1 {
		t.Errorf("launch = %+v", l)
	}
}

func TestStartRefuses(t *testing.T) {
	h := hostWithBinary(t, "#!/bin/sh\n")
	for name, mutate := range map[string]func(*harness.RunSpec){
		"empty prompt":    func(r *harness.RunSpec) { r.Prompt = "" },
		"empty ConfigDir": func(r *harness.RunSpec) { r.ConfigDir = "" },
		"model as option": func(r *harness.RunSpec) { r.Model = "--dangerously-skip-permissions" },
		"unknown mode":    func(r *harness.RunSpec) { r.Mode = "batch" },
	} {
		run := spec(domain.ModeHeadless)
		mutate(&run)
		if _, err := (claude.Adapter{}).Start(t.Context(), h, run); err == nil {
			t.Errorf("%s: no error", name)
		}
	}
	if _, err := (claude.Adapter{}).Start(t.Context(), hostWithBinary(t, ""), spec(domain.ModeHeadless)); err == nil {
		t.Error("no binary: no error")
	}
}
