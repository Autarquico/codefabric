package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Autarquico/codefabric/internal/domain"
)

func runClaude(t *testing.T, args ...string) (code int, stdout, stderr string) {
	t.Helper()
	var out, errOut bytes.Buffer
	code = RunContext(context.Background(), append([]string{"claude"}, args...), strings.NewReader(""), &out, &errOut)
	return code, out.String(), errOut.String()
}

func TestHandtoSkill_FrontmatterIsExact(t *testing.T) {
	out, err := renderHandto("v1.2.3")
	if err != nil {
		t.Fatal(err)
	}
	want := "---\n" +
		"name: handto\n" +
		"description: Hand this Claude Code session over to a CodeFabric Worker, so the work goes on there with the same context and the same repository state. Use only when the user types /handto <worker>.\n" +
		"disable-model-invocation: true\n" +
		"argument-hint: <worker>\n" +
		"arguments: [worker]\n" +
		"allowed-tools: Bash(cf handoff *) Bash(cf worker list*)\n" +
		"---\n" +
		"<!-- cf-version: v1.2.3 -->\n"
	if !strings.HasPrefix(string(out), want) {
		t.Errorf("frontmatter differs:\n%s", out)
	}
	for _, line := range []string{"!`cf worker list`", "cf handoff --session ${CLAUDE_SESSION_ID} --to $worker", "/exit", "cf attach <task>", "CF_*"} {
		if !strings.Contains(string(out), line) {
			t.Errorf("the body lacks %q", line)
		}
	}
}

func TestHandtoSkill_RejectsAVersionThatBreaksTheMarker(t *testing.T) {
	for _, v := range []string{"", "a b", "x-->", "v1\n2"} {
		if _, err := renderHandto(v); err == nil {
			t.Errorf("renderHandto(%q) accepted it", v)
		}
	}
}

func TestScenario_InstallIdempotente(t *testing.T) {
	home := t.TempDir()
	if s, err := installHandto("user", home, "v1", false, false); err != nil || s.Action != "created" {
		t.Fatalf("first install: %+v, %v", s, err)
	}
	before, _ := os.Stat(handtoPath(home))
	if s, err := installHandto("user", home, "v1", false, false); err != nil || s.Action != "unchanged" {
		t.Fatalf("same version: %+v, %v", s, err)
	}
	if after, _ := os.Stat(handtoPath(home)); !after.ModTime().Equal(before.ModTime()) {
		t.Error("the same version rewrote the file")
	}
	if s, _ := handtoStatus("user", home, "v2"); s.State != skillOutdated || s.Version != "v1" {
		t.Errorf("status under v2 = %+v", s)
	}
	if s, err := installHandto("user", home, "v2", false, false); err != nil || s.Action != "updated" {
		t.Fatalf("other version: %+v, %v", s, err)
	}
	if s, _ := handtoStatus("user", home, "v2"); s.State != skillCurrent {
		t.Errorf("status = %+v", s)
	}
}

func TestScenario_InstallIdempotente_Foreign(t *testing.T) {
	home := t.TempDir()
	mine := "---\nname: handto\n---\nmy own skill\n"
	if err := os.MkdirAll(filepath.Dir(handtoPath(home)), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(handtoPath(home), []byte(mine), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := installHandto("user", home, "v1", false, false)
	var de *domain.Error
	if !errors.As(err, &de) || de.Code != domain.CodeConfirmationRequired {
		t.Fatalf("a foreign skill without --force: %v", err)
	}
	if b, _ := os.ReadFile(handtoPath(home)); string(b) != mine {
		t.Error("a foreign skill was overwritten without force")
	}
	if s, err := installHandto("user", home, "v1", true, false); err != nil || s.Action != "replaced" {
		t.Fatalf("force: %+v, %v", s, err)
	}
}

func TestHandtoStatus_EditedIsNotAnotherVersion(t *testing.T) {
	home := t.TempDir()
	if _, err := installHandto("user", home, "v1", false, false); err != nil {
		t.Fatal(err)
	}
	f, err := os.OpenFile(handtoPath(home), os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = f.WriteString("an edit\n")
	_ = f.Close()
	s, _ := handtoStatus("user", home, "v1")
	if s.State != skillModified {
		t.Fatalf("status = %+v", s)
	}
	var out bytes.Buffer
	_ = printClaudeSkill(&out, s)
	if strings.Contains(out.String(), "another version") || !strings.Contains(out.String(), "edited") {
		t.Errorf("message: %s", out.String())
	}
	if d := doctorSkill(&s); d.Warning == "" || strings.Contains(d.Warning, "otra versión") || d.Action != "cf claude install" {
		t.Errorf("doctor: %+v", d)
	}
	if s, err := installHandto("user", home, "v1", false, false); err != nil || s.Action != "restored" {
		t.Fatalf("install: %+v, %v", s, err)
	}
}

func TestHandtoStatus_MarkerOnlyCountsRightAfterTheFrontmatter(t *testing.T) {
	home := t.TempDir()
	body := "---\nname: handto\n---\nquoting:\n<!-- cf-version: v1 -->\n"
	_ = os.MkdirAll(filepath.Dir(handtoPath(home)), 0o700)
	_ = os.WriteFile(handtoPath(home), []byte(body), 0o600)
	if s, _ := handtoStatus("user", home, "v1"); s.State != skillForeign {
		t.Errorf("status = %+v", s)
	}
	if s, err := uninstallHandto("user", home); err != nil || s.Action != "kept" {
		t.Errorf("uninstall: %+v, %v", s, err)
	}
}

func TestHandtoSkill_NeverLetsTheModelOverrideAGuard(t *testing.T) {
	out, _ := renderHandto("v1")
	for _, want := range []string{
		"Never add flags to `cf handoff`",
		"run it themselves in their own terminal",
		"do not run it yourself",
		"do not retry",
	} {
		if !strings.Contains(strings.ToLower(string(out)), strings.ToLower(want)) {
			t.Errorf("the skill lacks %q", want)
		}
	}
}

func TestClaudeProjectScope_RefusesSymlinkedClaudeDirs(t *testing.T) {
	for _, link := range []string{".claude", filepath.Join(".claude", "skills")} {
		t.Run(link, func(t *testing.T) {
			repo := t.TempDir()
			elsewhere := t.TempDir()
			if err := os.Mkdir(filepath.Join(repo, ".git"), 0o750); err != nil {
				t.Fatal(err)
			}
			if link != ".claude" {
				if err := os.Mkdir(filepath.Join(repo, ".claude"), 0o750); err != nil {
					t.Fatal(err)
				}
			}
			if err := os.Symlink(elsewhere, filepath.Join(repo, link)); err != nil {
				t.Fatal(err)
			}
			t.Chdir(repo)
			for _, cmd := range []string{"install", "uninstall"} {
				if code, _, errOut := runClaude(t, cmd, "--scope", "project"); code == 0 || !strings.Contains(errOut, "symlink") {
					t.Errorf("%s: %d %s", cmd, code, errOut)
				}
			}
			if entries, _ := os.ReadDir(elsewhere); len(entries) != 0 {
				t.Errorf("something was written through the link: %v", entries)
			}
		})
	}
}

func TestInstall_DryRunWritesNothing(t *testing.T) {
	home := t.TempDir()
	if s, err := installHandto("user", home, "v1", false, true); err != nil || s.Action != "created" || !s.DryRun {
		t.Fatalf("%+v, %v", s, err)
	}
	var out bytes.Buffer
	_ = printClaudeSkill(&out, ClaudeSkill{Action: "created", DryRun: true, Path: "p"})
	_ = printClaudeSkill(&out, ClaudeSkill{Action: "unchanged", DryRun: true, Path: "p"})
	if !strings.Contains(out.String(), "would be created") || strings.Contains(out.String(), "would be unchanged") {
		t.Errorf("dry-run copy: %s", out.String())
	}
	if _, err := os.Stat(filepath.Join(home, ".claude")); !os.IsNotExist(err) {
		t.Error("dry-run wrote something")
	}
}

func TestUninstall_OnlyRemovesWhatIsOurs(t *testing.T) {
	home := t.TempDir()
	if s, err := uninstallHandto("user", home); err != nil || s.Action != "absent" {
		t.Fatalf("absent: %+v, %v", s, err)
	}
	if _, err := installHandto("user", home, "v1", false, false); err != nil {
		t.Fatal(err)
	}
	if s, err := uninstallHandto("user", home); err != nil || s.Action != "removed" {
		t.Fatalf("removed: %+v, %v", s, err)
	}
	if _, err := os.Stat(filepath.Dir(handtoPath(home))); !os.IsNotExist(err) {
		t.Error("the empty directory of the skill stayed")
	}
	foreign := "someone else's\n"
	_ = os.MkdirAll(filepath.Dir(handtoPath(home)), 0o700)
	_ = os.WriteFile(handtoPath(home), []byte(foreign), 0o600)
	if s, err := uninstallHandto("user", home); err != nil || s.Action != "kept" {
		t.Fatalf("foreign: %+v, %v", s, err)
	}
	if b, _ := os.ReadFile(handtoPath(home)); string(b) != foreign {
		t.Error("a foreign skill was removed")
	}
}

func TestClaudeCmd_InstallStatusUninstallOverHome(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	if code, out, errOut := runClaude(t, "install"); code != 0 || !strings.Contains(out, "created") {
		t.Fatalf("install: %d %s %s", code, out, errOut)
	}
	code, out, _ := runClaude(t, "status", "--json")
	var s ClaudeSkill
	if err := json.Unmarshal([]byte(out), &s); err != nil || code != 0 || s.State != skillCurrent || s.Scope != "user" {
		t.Fatalf("status: %d %s %v", code, out, err)
	}
	if code, out, _ := runClaude(t, "install"); code != 0 || !strings.Contains(out, "unchanged") {
		t.Fatalf("second install: %d %s", code, out)
	}
	if code, out, _ := runClaude(t, "uninstall"); code != 0 || !strings.Contains(out, "removed") {
		t.Fatalf("uninstall: %d %s", code, out)
	}
	if code, _, errOut := runClaude(t, "install", "--scope", "nowhere"); code != 2 {
		t.Errorf("bad scope: %d %s", code, errOut)
	}
}

func TestDoctorSkill_WarnsWhenMissingOrStaleWithTheAction(t *testing.T) {
	for _, tc := range []struct {
		skill ClaudeSkill
		warns bool
	}{
		{ClaudeSkill{State: skillMissing}, true},
		{ClaudeSkill{State: skillOutdated, Version: "v0"}, true},
		{ClaudeSkill{State: skillCurrent}, false},
	} {
		d := doctorSkill(&tc.skill)
		if (d.Warning != "") != tc.warns || (tc.warns && d.Action != "cf claude install") {
			t.Errorf("%s: %+v", tc.skill.State, d)
		}
	}
	if doctorSkill(nil) != nil {
		t.Error("an unknown state must not be reported")
	}
}
