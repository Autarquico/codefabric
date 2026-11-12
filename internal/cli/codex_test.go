package cli

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Autarquico/codefabric/internal/domain"
)

func runCodex(t *testing.T, args ...string) (code int, stdout, stderr string) {
	t.Helper()
	var out, errOut bytes.Buffer
	code = RunContext(context.Background(), append([]string{"codex"}, args...), strings.NewReader(""), &out, &errOut)
	return code, out.String(), errOut.String()
}

func mustRead(t *testing.T, path string) []byte {
	t.Helper()
	b, err := os.ReadFile(path) //nolint:gosec // a path of the test
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestCodexHandtoSkill_AsksForNoFlagsAndNoSessionArgument(t *testing.T) {
	out, err := renderHandto(codexHandto, "v1")
	if err != nil {
		t.Fatal(err)
	}
	text := string(out)
	for _, want := range []string{"name: handto\n", "<!-- cf-version: v1 -->", "`cf handoff --to <worker>`", "CODEX_SESSION_ID", "Never add flags", "never pass `--session`", "CF_*"} {
		if !strings.Contains(text, want) {
			t.Errorf("the Codex skill lacks %q", want)
		}
	}
	if strings.Contains(text, "CLAUDE_SESSION_ID") || strings.Contains(text, "allowed-tools") || strings.Contains(text, "$worker") {
		t.Errorf("the Codex skill carries what only Claude Code understands:\n%s", text)
	}
	if got := handtoMarkerVersion(out); got != "v1" {
		t.Errorf("marker version %q", got)
	}
}

func TestScenario_InstallCodexReusesTheAgentsSkillsDirectory(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	want := filepath.Join(home, ".agents", "skills", "handto", "SKILL.md")

	code, out, errOut := runCodex(t, "install")
	if code != 0 || !strings.Contains(out, "created") || !strings.Contains(out, want) {
		t.Fatalf("install: %d %q %q", code, out, errOut)
	}
	if _, err := os.Stat(filepath.Join(home, ".claude")); !os.IsNotExist(err) {
		t.Error("cf codex install wrote under .claude")
	}
	if code, out, _ := runCodex(t, "install"); code != 0 || !strings.Contains(out, "unchanged") {
		t.Errorf("second install: %d %q", code, out)
	}
	if code, out, _ := runCodex(t, "status"); code != 0 || !strings.Contains(out, "current") {
		t.Errorf("status: %d %q", code, out)
	}

	// Another version updates it; one that cf did not write is not overwritten.
	old := bytes.Replace(mustRead(t, want), []byte("cf-version: "), []byte("cf-version: old-"), 1)
	if err := os.WriteFile(want, old, 0o600); err != nil {
		t.Fatal(err)
	}
	if code, out, _ := runCodex(t, "install"); code != 0 || !strings.Contains(out, "updated") {
		t.Errorf("another version: %d %q", code, out)
	}
	if err := os.WriteFile(want, []byte("---\nname: handto\n---\nmine\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if code, _, errOut := runCodex(t, "install"); code == 0 || !strings.Contains(errOut, "CF_CONFIRMATION_REQUIRED") {
		t.Errorf("a foreign skill: %d %q", code, errOut)
	}
	if code, _, _ := runCodex(t, "uninstall"); code != 0 || !strings.Contains(string(mustRead(t, want)), "mine") {
		t.Error("uninstall removed a skill that cf did not write")
	}
	if code, _, _ := runCodex(t, "install", "--force"); code != 0 {
		t.Error("--force did not replace it")
	}
	if code, out, _ := runCodex(t, "uninstall"); code != 0 || !strings.Contains(out, "removed") {
		t.Errorf("uninstall: %d %q", code, out)
	}
}

func TestCodexProjectScope_RefusesASymlinkedAgentsDir(t *testing.T) {
	repo, elsewhere := t.TempDir(), t.TempDir()
	if err := os.Mkdir(filepath.Join(repo, ".git"), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(elsewhere, filepath.Join(repo, ".agents")); err != nil {
		t.Fatal(err)
	}
	t.Chdir(repo)
	if code, _, errOut := runCodex(t, "install", "--scope", "project"); code == 0 {
		t.Errorf("install followed the link: %q", errOut)
	}
	if entries, _ := os.ReadDir(elsewhere); len(entries) != 0 {
		t.Errorf("something was written outside the repository: %v", entries)
	}
}

func TestDoctor_WarnsAboutTheCodexSkillOnlyWhenCodexIsInstalled(t *testing.T) {
	home, bin := t.TempDir(), t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("PATH", bin)
	f := &fakeDoctor{status: healthyController()}
	probe := func(context.Context, domain.SSHTarget) error { return nil }
	if code, _, errOut := runClaude(t, "install"); code != 0 {
		t.Fatalf("install claude: %s", errOut)
	}

	if _, out, _ := runDoctorHere(t, f, probe); strings.Contains(out, "Codex") {
		t.Errorf("no codex on the PATH, yet: %q", out)
	}
	if err := os.WriteFile(filepath.Join(bin, "codex"), []byte("#!/bin/sh\n"), 0o700); err != nil { //nolint:gosec // a fake executable
		t.Fatal(err)
	}
	_, out, _ := runDoctorHere(t, f, probe)
	if !strings.Contains(out, "aviso: la skill handto de Codex no está instalada") || !strings.Contains(out, "(acción: cf codex install)") {
		t.Fatalf("codex installed, skill missing: %q", out)
	}
	if code, _, errOut := runCodex(t, "install"); code != 0 {
		t.Fatalf("install codex: %s", errOut)
	}
	if _, out, _ := runDoctorHere(t, f, probe); strings.Contains(out, "aviso:") {
		t.Errorf("installed, yet: %q", out)
	}
}
