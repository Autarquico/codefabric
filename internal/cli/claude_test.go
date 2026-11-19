package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/spf13/cobra"

	"github.com/Autarquico/codeflock/internal/domain"
)

func runClaude(t *testing.T, args ...string) (code int, stdout, stderr string) {
	t.Helper()
	var out, errOut bytes.Buffer
	code = RunContext(context.Background(), append([]string{"claude"}, args...), strings.NewReader(""), &out, &errOut)
	return code, out.String(), errOut.String()
}

func TestHandtoSkill_FrontmatterIsExact(t *testing.T) {
	out, err := renderSkill(claudeHandto, "v1.2.3")
	if err != nil {
		t.Fatal(err)
	}
	want := "---\n" +
		"name: handto\n" +
		"description: Hand this Claude Code session over to a Code Flock Worker, so the work goes on there with the same context and the same repository state. Use only when the user types /handto <worker>.\n" +
		"disable-model-invocation: true\n" +
		"argument-hint: <worker>\n" +
		"arguments: [worker]\n" +
		"allowed-tools: Bash(cflk worker list --state online,degraded) Bash(cflk handoff --session * --to *)\n" +
		"---\n" +
		"<!-- cflk-version: v1.2.3 -->\n"
	if !strings.HasPrefix(string(out), want) {
		t.Errorf("frontmatter differs:\n%s", out)
	}
	for _, line := range []string{"!`cflk worker list --state online,degraded`", "cflk handoff --session ${CLAUDE_SESSION_ID} --to $worker", "/exit", "cflk attach <task>", "CF_*"} {
		if !strings.Contains(string(out), line) {
			t.Errorf("the body lacks %q", line)
		}
	}
}

func TestWorkersSkill_ListsWorkersAndRemoteRuns(t *testing.T) {
	out, err := renderSkill(claudeWorkers, "v1.2.3")
	if err != nil {
		t.Fatal(err)
	}
	want := "---\n" +
		"name: workers\n" +
		"description: List the Code Flock Workers and the interactive Runs that are live on them, and say how to enter one. Use only when the user types /workers.\n" +
		"disable-model-invocation: true\n" +
		"allowed-tools: Bash(cflk worker list --timeout 10s) Bash(cflk ps --timeout 10s)\n" +
		"---\n" +
		"<!-- cflk-version: v1.2.3 -->\n"
	if !strings.HasPrefix(string(out), want) {
		t.Errorf("frontmatter differs:\n%s", out)
	}
	if !strings.Contains(string(out), "!`cflk worker list --timeout 10s`") {
		t.Errorf("the body does not list the Workers:\n%s", out)
	}
}

func TestClaudeCmd_InstallsAndRemovesBothSkills(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	if code, out, errOut := runClaude(t, "install"); code != 0 || !strings.Contains(out, "handto skill: created") || !strings.Contains(out, "workers skill: created") {
		t.Fatalf("install: %d %s %s", code, out, errOut)
	}
	for _, name := range []string{"handto", "workers"} {
		if _, err := os.Stat(filepath.Join(home, ".claude", "skills", name, "SKILL.md")); err != nil {
			t.Errorf("%s was not written: %v", name, err)
		}
	}
	if code, out, _ := runClaude(t, "uninstall"); code != 0 || strings.Count(out, "removed") != 2 {
		t.Fatalf("uninstall: %d %s", code, out)
	}
	if _, err := os.Stat(filepath.Join(home, ".claude", "skills", "workers")); !os.IsNotExist(err) {
		t.Error("the workers skill stayed")
	}
}

func TestHandtoSkill_RejectsAVersionThatBreaksTheMarker(t *testing.T) {
	for _, v := range []string{"", "a b", "x-->", "v1\n2"} {
		if _, err := renderSkill(claudeHandto, v); err == nil {
			t.Errorf("renderSkill(claudeHandto, %q) accepted it", v)
		}
	}
}

func TestScenario_InstallIdempotente(t *testing.T) {
	home := t.TempDir()
	if s, err := installSkill(claudeHandto, "user", home, "v1", false, false); err != nil || s.Action != "created" {
		t.Fatalf("first install: %+v, %v", s, err)
	}
	before, _ := os.Stat(skillPath(claudeHandto, home))
	if s, err := installSkill(claudeHandto, "user", home, "v1", false, false); err != nil || s.Action != "unchanged" {
		t.Fatalf("same version: %+v, %v", s, err)
	}
	if after, _ := os.Stat(skillPath(claudeHandto, home)); !after.ModTime().Equal(before.ModTime()) {
		t.Error("the same version rewrote the file")
	}
	if s, _ := skillStatus(claudeHandto, "user", home, "v2"); s.State != skillOutdated || s.Version != "v1" {
		t.Errorf("status under v2 = %+v", s)
	}
	if s, err := installSkill(claudeHandto, "user", home, "v2", false, false); err != nil || s.Action != "updated" {
		t.Fatalf("other version: %+v, %v", s, err)
	}
	if s, _ := skillStatus(claudeHandto, "user", home, "v2"); s.State != skillCurrent {
		t.Errorf("status = %+v", s)
	}
}

func TestScenario_InstallIdempotente_Foreign(t *testing.T) {
	home := t.TempDir()
	mine := "---\nname: handto\n---\nmy own skill\n"
	if err := os.MkdirAll(filepath.Dir(skillPath(claudeHandto, home)), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(skillPath(claudeHandto, home), []byte(mine), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := installSkill(claudeHandto, "user", home, "v1", false, false)
	var de *domain.Error
	if !errors.As(err, &de) || de.Code != domain.CodeConfirmationRequired {
		t.Fatalf("a foreign skill without --force: %v", err)
	}
	if b, _ := os.ReadFile(skillPath(claudeHandto, home)); string(b) != mine {
		t.Error("a foreign skill was overwritten without force")
	}
	if s, err := installSkill(claudeHandto, "user", home, "v1", true, false); err != nil || s.Action != "replaced" {
		t.Fatalf("force: %+v, %v", s, err)
	}
}

func TestHandtoStatus_EditedIsNotAnotherVersion(t *testing.T) {
	home := t.TempDir()
	if _, err := installSkill(claudeHandto, "user", home, "v1", false, false); err != nil {
		t.Fatal(err)
	}
	f, err := os.OpenFile(skillPath(claudeHandto, home), os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = f.WriteString("an edit\n")
	_ = f.Close()
	s, _ := skillStatus(claudeHandto, "user", home, "v1")
	if s.State != skillModified {
		t.Fatalf("status = %+v", s)
	}
	var out bytes.Buffer
	_ = printSkill(&out, claudeHandto, s)
	if strings.Contains(out.String(), "another version") || !strings.Contains(out.String(), "edited") {
		t.Errorf("message: %s", out.String())
	}
	if d := doctorSkill(claudeHandto, &s); d.Warning == "" || strings.Contains(d.Warning, "otra versión") || d.Action != "cflk claude install" {
		t.Errorf("doctor: %+v", d)
	}
	if s, err := installSkill(claudeHandto, "user", home, "v1", false, false); err != nil || s.Action != "restored" {
		t.Fatalf("install: %+v, %v", s, err)
	}
}

func TestHandtoStatus_MarkerOnlyCountsRightAfterTheFrontmatter(t *testing.T) {
	home := t.TempDir()
	body := "---\nname: handto\n---\nquoting:\n<!-- cflk-version: v1 -->\n"
	_ = os.MkdirAll(filepath.Dir(skillPath(claudeHandto, home)), 0o700)
	_ = os.WriteFile(skillPath(claudeHandto, home), []byte(body), 0o600)
	if s, _ := skillStatus(claudeHandto, "user", home, "v1"); s.State != skillForeign {
		t.Errorf("status = %+v", s)
	}
	if s, err := uninstallSkill(claudeHandto, "user", home); err != nil || s.Action != "kept" {
		t.Errorf("uninstall: %+v, %v", s, err)
	}
}

func TestHandtoSkill_NeverLetsTheModelOverrideAGuard(t *testing.T) {
	out, _ := renderSkill(claudeHandto, "v1")
	for _, want := range []string{
		"Never add flags to `cflk handoff`",
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
	if s, err := installSkill(claudeHandto, "user", home, "v1", false, true); err != nil || s.Action != "created" || !s.DryRun {
		t.Fatalf("%+v, %v", s, err)
	}
	var out bytes.Buffer
	_ = printSkill(&out, claudeHandto, SkillStatus{Action: "created", DryRun: true, Path: "p"})
	_ = printSkill(&out, claudeHandto, SkillStatus{Action: "unchanged", DryRun: true, Path: "p"})
	if !strings.Contains(out.String(), "would be created") || strings.Contains(out.String(), "would be unchanged") {
		t.Errorf("dry-run copy: %s", out.String())
	}
	if _, err := os.Stat(filepath.Join(home, ".claude")); !os.IsNotExist(err) {
		t.Error("dry-run wrote something")
	}
}

func TestUninstall_OnlyRemovesWhatIsOurs(t *testing.T) {
	home := t.TempDir()
	if s, err := uninstallSkill(claudeHandto, "user", home); err != nil || s.Action != "absent" {
		t.Fatalf("absent: %+v, %v", s, err)
	}
	if _, err := installSkill(claudeHandto, "user", home, "v1", false, false); err != nil {
		t.Fatal(err)
	}
	if s, err := uninstallSkill(claudeHandto, "user", home); err != nil || s.Action != "removed" {
		t.Fatalf("removed: %+v, %v", s, err)
	}
	if _, err := os.Stat(filepath.Dir(skillPath(claudeHandto, home))); !os.IsNotExist(err) {
		t.Error("the empty directory of the skill stayed")
	}
	foreign := "someone else's\n"
	_ = os.MkdirAll(filepath.Dir(skillPath(claudeHandto, home)), 0o700)
	_ = os.WriteFile(skillPath(claudeHandto, home), []byte(foreign), 0o600)
	if s, err := uninstallSkill(claudeHandto, "user", home); err != nil || s.Action != "kept" {
		t.Fatalf("foreign: %+v, %v", s, err)
	}
	if b, _ := os.ReadFile(skillPath(claudeHandto, home)); string(b) != foreign {
		t.Error("a foreign skill was removed")
	}
}

func TestClaudeCmd_InstallStatusUninstallOverHome(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	if code, out, errOut := runClaude(t, "install"); code != 0 || !strings.Contains(out, "created") {
		t.Fatalf("install: %d %s %s", code, out, errOut)
	}
	code, out, _ := runClaude(t, "status", "--json")
	var skills []SkillStatus
	if err := json.Unmarshal([]byte(out), &skills); err != nil || code != 0 || len(skills) != 2 {
		t.Fatalf("status: %d %s %v", code, out, err)
	}
	for i, name := range []string{"handto", "workers"} {
		if s := skills[i]; s.Name != name || s.State != skillCurrent || s.Scope != "user" {
			t.Errorf("status of %s: %+v", name, s)
		}
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

func TestDoctorSkill_NamesTheSkillItWarnsAbout(t *testing.T) {
	d := doctorSkill(claudeWorkers, &SkillStatus{State: skillMissing})
	if !strings.Contains(d.Warning, "la skill workers de Claude Code") || d.Action != "cflk claude install" {
		t.Errorf("doctor: %+v", d)
	}
}

func TestDoctorSkill_WarnsWhenMissingOrStaleWithTheAction(t *testing.T) {
	for _, tc := range []struct {
		skill SkillStatus
		warns bool
	}{
		{SkillStatus{State: skillMissing}, true},
		{SkillStatus{State: skillOutdated, Version: "v0"}, true},
		{SkillStatus{State: skillCurrent}, false},
	} {
		d := doctorSkill(claudeHandto, &tc.skill)
		if (d.Warning != "") != tc.warns || (tc.warns && d.Action != "cflk claude install") {
			t.Errorf("%s: %+v", tc.skill.State, d)
		}
	}
	if doctorSkill(claudeHandto, nil) != nil {
		t.Error("an unknown state must not be reported")
	}
}

func TestInstall_ForeignSkillDoesNotHideTheOthers(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	mine := "---\nname: workers\n---\nmy own skill\n"
	foreign := skillPath(claudeWorkers, home)
	if err := os.MkdirAll(filepath.Dir(foreign), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(foreign, []byte(mine), 0o600); err != nil {
		t.Fatal(err)
	}
	code, out, errOut := runClaude(t, "install")
	if code == 0 || !strings.Contains(errOut, "did not write") {
		t.Errorf("the refusal must fail the command: %d %q", code, errOut)
	}
	if !strings.Contains(out, "handto skill: created") || !strings.Contains(out, "workers skill: skipped") || !strings.Contains(out, "use --force") {
		t.Errorf("each skill must be reported: %q", out)
	}
	if _, err := os.Stat(skillPath(claudeHandto, home)); err != nil {
		t.Errorf("handto was not written: %v", err)
	}
	if b, _ := os.ReadFile(foreign); string(b) != mine { //nolint:gosec // a path of the test
		t.Error("the foreign skill was overwritten")
	}
}

func TestInstallUserSkills_ReportsWhatWasWrittenBesideTheRefusal(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	foreign := skillPath(claudeWorkers, home)
	if err := os.MkdirAll(filepath.Dir(foreign), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(foreign, []byte("---\nname: workers\n---\nmine\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := InstallUserSkills()
	if err == nil || len(got) != 2 || got[0].Action != "created" || got[1].Action != "skipped" {
		t.Fatalf("got %+v, %v", got, err)
	}
}

func TestSkillNames(t *testing.T) {
	if got := skillNames(claudeSkills); got != "handto and workers" {
		t.Errorf("got %q", got)
	}
}

func TestSkills_AllowedToolsNameOnlyTheCommandsTheSkillRuns(t *testing.T) {
	for _, c := range []struct {
		k    skillKind
		want string
	}{
		{claudeHandto, "allowed-tools: Bash(cflk worker list --state online,degraded) Bash(cflk handoff --session * --to *)\n"},
		{claudeWorkers, "allowed-tools: Bash(cflk worker list --timeout 10s) Bash(cflk ps --timeout 10s)\n"},
	} {
		out, err := renderSkill(c.k, "v1")
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(string(out), c.want) || !strings.Contains(string(out), "<!-- cflk-version: v1 -->\n") {
			t.Errorf("%s skill:\n%s", c.k.name, out)
		}
	}
}

func TestSkills_ASkillOfTheBetaWithTheCfMarkerIsOutdatedAndUpdatesWithoutForce(t *testing.T) {
	for _, k := range append(append([]skillKind{}, claudeSkills...), codexSkills...) {
		t.Run(k.command+"/"+k.name, func(t *testing.T) {
			home := t.TempDir()
			current, err := renderSkill(k, "v1")
			if err != nil {
				t.Fatal(err)
			}
			// the beta.6 wrote the same version under the old marker and the old commands
			legacy := bytes.Replace(current, []byte("<!-- cflk-version: v1 -->"), []byte("<!-- cf-version: v1 -->"), 1)
			legacy = bytes.ReplaceAll(legacy, []byte("cflk "), []byte("cf "))
			path := skillPath(k, home)
			if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil { //nolint:gosec // a test directory
				t.Fatal(err)
			}
			if err := os.WriteFile(path, legacy, 0o600); err != nil {
				t.Fatal(err)
			}
			if s, _ := skillStatus(k, "user", home, "v1"); s.State != skillOutdated || s.Version != "v1" {
				t.Fatalf("status = %+v", s)
			}
			if d := doctorSkill(k, &SkillStatus{State: skillOutdated, Version: "v1"}); d.Action != k.installCommand() {
				t.Errorf("doctor: %+v", d)
			}
			if s, err := installSkill(k, "user", home, "v1", false, false); err != nil || s.Action != "updated" {
				t.Fatalf("install: %+v, %v", s, err)
			}
			requireEqual(t, string(mustRead(t, path)), string(current))
		})
	}
}

func TestSkills_InstallLeavesTheFilesTheUserAddedAlone(t *testing.T) {
	home := t.TempDir()
	if _, err := installSkill(claudeHandto, "user", home, "v1", false, false); err != nil {
		t.Fatal(err)
	}
	mine := filepath.Join(filepath.Dir(skillPath(claudeHandto, home)), "notes.md")
	if err := os.WriteFile(mine, []byte("mine"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := installSkill(claudeHandto, "user", home, "v2", false, false); err != nil {
		t.Fatal(err)
	}
	if s, err := uninstallSkill(claudeHandto, "user", home); err != nil || s.Action != "removed" {
		t.Fatalf("uninstall: %+v, %v", s, err)
	}
	requireEqual(t, string(mustRead(t, mine)), "mine")
}

func TestUserVisibleTexts_NameCflkAndNotCf(t *testing.T) {
	cf := regexp.MustCompile(`\bcf\b`)
	var texts []string
	for _, state := range []string{skillMissing, skillOutdated, skillModified, skillForeign} {
		d := doctorSkill(claudeHandto, &SkillStatus{State: state, Version: "v0"})
		texts = append(texts, d.Warning, d.Action)
	}
	for _, c := range []*cobra.Command{newClaudeCmd(), newCodexCmd()} {
		texts = append(texts, c.Short)
		for _, sub := range c.Commands() {
			texts = append(texts, sub.Short, sub.Long)
		}
	}
	var out bytes.Buffer
	_ = printSkill(&out, claudeHandto, SkillStatus{Name: "handto", State: skillModified})
	_ = printSkill(&out, claudeHandto, SkillStatus{Name: "handto", State: skillCurrent, Version: "v1"})
	texts = append(texts, out.String())
	for _, text := range texts {
		if cf.MatchString(text) {
			t.Errorf("a text that the user reads still says cf: %q", text)
		}
	}
}

func TestHandtoSkill_AllowedToolsLeaveOutTheOverrideFlagsAndWildcardCommands(t *testing.T) {
	out, err := renderSkill(claudeHandto, "v1")
	if err != nil {
		t.Fatal(err)
	}
	var line string
	for _, l := range strings.Split(string(out), "\n") {
		if strings.HasPrefix(l, "allowed-tools:") {
			line = l
		}
	}
	for _, bad := range []string{"handoff *", "worker list*", "--force", "--yes", "--allow"} {
		if strings.Contains(line, bad) {
			t.Errorf("allowed-tools %q has %q", line, bad)
		}
	}
}
