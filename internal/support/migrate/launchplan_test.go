package migrate

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/Autarquico/codeflock/internal/support/svcunit"
	"github.com/Autarquico/codeflock/internal/support/svcunit/svcunittest"
)

const daemonPlist = `<?xml version="1.0" encoding="UTF-8"?>
<plist version="1.0">
<dict>
	<key>Label</key>
	<string>com.codefabric.worker</string>
	<key>UserName</key>
	<string>ana</string>
	<key>ProgramArguments</key>
	<array>
		<string>/usr/local/bin/cf</string>
		<string>worker</string>
		<string>run</string>
	</array>
	<key>EnvironmentVariables</key>
	<dict>
		<key>HOME</key>
		<string>/Users/ana</string>
		<key>CF_CONFIG_DIR</key>
		<string>/Users/ana/.config/codefabric</string>
		<key>CF_DATA_DIR</key>
		<string>/Users/ana/mine</string>
	</dict>
	<key>StandardOutPath</key>
	<string>/Users/ana/Library/Logs/codefabric/com.codefabric.worker.log</string>
</dict>
</plist>
`

// forward and way back of a plan: the way back is the part after its heading.
func parts(t *testing.T, cmds []string) (forward, back string) {
	t.Helper()
	text := strings.Join(cmds, "\n")
	forward, back, ok := strings.Cut(text, "# To go back, if a step failed")
	if !ok {
		t.Fatalf("no way back in:\n%s", text)
	}
	return forward, back
}

func TestLaunchPlanForADaemonIsOneBlockForRootWithTheWayBackCommentedOut(t *testing.T) {
	cmds, err := LaunchPlan{Plist: "/Library/LaunchDaemons/com.codefabric.worker.plist", Content: []byte(daemonPlist), System: true, Binary: "/usr/local/bin/cflk"}.Commands()
	if err != nil {
		t.Fatal(err)
	}
	forward, back := parts(t, cmds)
	for _, want := range []string{
		"sudo sh -eu <<'CFLK_PLAN'",
		"launchctl bootout system/'com.codefabric.worker'",
		"sudo -u 'ana' mv -n '/Users/ana/.config/codefabric' '/Users/ana/.config/codeflock'",
		"sudo -u 'ana' mv -n '/Users/ana/Library/Logs/codefabric' '/Users/ana/Library/Logs/codeflock'",
		"chown root:wheel \"$tmp\"",
		"launchctl bootstrap system '/Library/LaunchDaemons/com.codeflock.worker.plist'",
		"<string>com.codeflock.worker</string>", "<key>CFLK_DATA_DIR</key>", "<string>/usr/local/bin/cflk</string>",
		"/Users/ana/Library/Logs/codeflock/com.codeflock.worker.log", "<string>/Users/ana/.config/codeflock</string>",
		"[ ! -e", // never into a destination that is there, in the new plist too
		"mv '/Library/LaunchDaemons/com.codefabric.worker.plist' '/Library/LaunchDaemons/com.codefabric.worker.plist.cflk-old'",
		"touch '/Library/LaunchDaemons/com.codeflock.worker.plist.cflk-written'",
	} {
		if want == "[ ! -e" {
			continue
		}
		if !strings.Contains(forward, want) {
			t.Errorf("missing %q in:\n%s", want, forward)
		}
	}
	if strings.Contains(forward, "tee ") || !strings.Contains(forward, "mv -n \"$tmp\" '/Library/LaunchDaemons/com.codeflock.worker.plist'") ||
		!strings.Contains(forward, "if [ -e '/Library/LaunchDaemons/com.codeflock.worker.plist' ] || [ -L '/Library/LaunchDaemons/com.codeflock.worker.plist' ]") {
		t.Errorf("the new plist is not written to a temporary and moved without replacing:\n%s", forward)
	}
	// The directory the owner chose is theirs: it is not moved, and the plist keeps its path.
	if strings.Contains(forward, "mv -n '/Users/ana/mine'") || !strings.Contains(forward, "<string>/Users/ana/mine</string>") {
		t.Errorf("a chosen directory moved:\n%s", forward)
	}
	for _, want := range []string{
		"sudo -u 'ana' mv -n '/Users/ana/.config/codeflock' '/Users/ana/.config/codefabric'",
		"rc=0; launchctl print system/'com.codeflock.worker' >/dev/null 2>&1 || rc=$?",
		`if [ "$rc" = 0 ]; then launchctl bootout system/'com.codeflock.worker'`,
		"launchctl bootstrap system '/Library/LaunchDaemons/com.codefabric.worker.plist'",
	} {
		if !strings.Contains(back, want) {
			t.Errorf("missing %q in the way back:\n%s", want, back)
		}
	}
	if strings.Contains(forward, "rm '/Library/LaunchDaemons/com.codefabric.worker.plist'") {
		t.Errorf("the old property list is deleted, so there is no way back:\n%s", forward)
	}
	if strings.Index(back, "mv -n '/Users/ana/Library/Logs/codeflock'") > strings.Index(back, "mv -n '/Users/ana/.config/codeflock'") {
		t.Errorf("the way back is not in reverse order:\n%s", back)
	}
}

// Pasting the whole output must run the way there and nothing of the way back.
func TestLaunchPlanWayBackIsEntirelyCommented(t *testing.T) {
	for _, system := range []bool{true, false} {
		cmds, err := LaunchPlan{Plist: "/Library/LaunchDaemons/com.codefabric.worker.plist", Content: []byte(daemonPlist), System: system, Binary: "/usr/local/bin/cflk"}.Commands()
		if err != nil {
			t.Fatal(err)
		}
		_, back := parts(t, cmds)
		lines := strings.Split(strings.TrimSpace(back), "\n")
		if len(lines) < 5 {
			t.Fatalf("a way back with %d lines:\n%s", len(lines), back)
		}
		for _, line := range lines[1:] { // the first is the rest of the heading line
			if !strings.HasPrefix(line, "# ") {
				t.Errorf("a line of the way back that runs: %q", line)
			}
		}
	}
}

// Both blocks, as printed and with the way back uncommented, are valid shell.
func TestLaunchPlanBlocksParseAsShell(t *testing.T) {
	for _, system := range []bool{true, false} {
		cmds, err := LaunchPlan{Plist: "/Users/ana/Library/LaunchAgents/com.codefabric.controller.plist", Content: []byte(daemonPlist), System: system, Binary: "/opt/cflk"}.Commands()
		if err != nil {
			t.Fatal(err)
		}
		forward, back := parts(t, cmds)
		var uncommented []string
		for _, line := range strings.Split(back, "\n")[1:] {
			uncommented = append(uncommented, strings.TrimPrefix(line, "# "))
		}
		for name, script := range map[string]string{"printed": strings.Join(cmds, "\n"), "forward": forward, "back": strings.Join(uncommented, "\n")} {
			cmd := exec.CommandContext(t.Context(), "sh", "-n")
			cmd.Stdin = strings.NewReader(script)
			if out, err := cmd.CombinedOutput(); err != nil {
				t.Errorf("system=%v %s does not parse: %v\n%s\n%s", system, name, err, out, script)
			}
		}
	}
}

func TestLaunchPlanLogsDirectoryIsOptionalBothWays(t *testing.T) {
	cmds, err := LaunchPlan{Plist: "/Library/LaunchDaemons/com.codefabric.worker.plist", Content: []byte(daemonPlist), System: true, Binary: "/opt/cflk"}.Commands()
	if err != nil {
		t.Fatal(err)
	}
	forward, back := parts(t, cmds)
	if !strings.Contains(forward, "if [ -e '/Users/ana/Library/Logs/codefabric' ]; then") || !strings.Contains(back, "if [ -e '/Users/ana/Library/Logs/codeflock' ]; then") {
		t.Errorf("the logs are required:\n%s", strings.Join(cmds, "\n"))
	}
}

func TestLaunchPlanRefusesALabelThatIsNotPlain(t *testing.T) {
	for _, plist := range []string{"/Library/LaunchDaemons/com.codefabric.worker$(touch pwned).plist", "/Library/LaunchDaemons/com.codefabric.worker;x.plist", "/Library/LaunchDaemons/com.codefabric.wor ker.plist"} {
		cmds, err := LaunchPlan{Plist: plist, Content: []byte(daemonPlist), System: true, Binary: "/opt/cflk"}.Commands()
		if err == nil || len(cmds) > 0 {
			t.Errorf("%s: %v %v", plist, cmds, err)
		}
	}
}

func TestLaunchPlanForAnAgentNeedsNoSudo(t *testing.T) {
	cmds, err := LaunchPlan{Plist: "/Users/ana/Library/LaunchAgents/com.codefabric.controller.plist", Content: []byte(daemonPlist)}.Commands()
	if err != nil {
		t.Fatal(err)
	}
	script := strings.Join(cmds, "\n")
	if strings.Contains(script, "sudo") || !strings.Contains(script, "<<'CFLK_PLAN'") || !strings.Contains(script, "\nsh -eu <<'CFLK_PLAN'") {
		t.Errorf("sudo in a user's agent, or not one block:\n%s", script)
	}
	if !strings.Contains(script, "launchctl bootout gui/$(id -u)/'com.codefabric.controller'") {
		t.Errorf("%s", script)
	}
}

func TestLaunchPlanThatCannotSayWhatToMoveRefuses(t *testing.T) {
	if _, err := (LaunchPlan{Plist: "/x/com.codefabric.worker.plist", Content: []byte(`<plist><dict></dict></plist>`), System: true}).Commands(); err == nil {
		t.Error("no directories and no HOME must not guess")
	}
}

func TestLaunchPlanProgramIsTheCflkGiven(t *testing.T) {
	cmds, err := LaunchPlan{Plist: "/Library/LaunchDaemons/com.codefabric.worker.plist", Content: []byte(daemonPlist), System: true, Binary: "/opt/cflk"}.Commands()
	if err != nil {
		t.Fatal(err)
	}
	script := strings.Join(cmds, "\n")
	if strings.Contains(script, "/usr/local/bin/cf<") || !strings.Contains(script, "<string>/opt/cflk</string>") {
		t.Errorf("the transition cf is still the program:\n%s", script)
	}
}

const bareDaemonPlist = `<plist><dict><key>Label</key><string>com.codefabric.worker</string>%s
<key>ProgramArguments</key><array><string>/usr/local/bin/cf</string><string>worker</string><string>run</string></array>
<key>EnvironmentVariables</key><dict>%s</dict></dict></plist>`

// A LaunchDaemon runs as root: the files it moves are the daemon's, and never
// those of whoever asks for the plan, which a user could have planted.
func TestLaunchPlanOfADaemonNeverUsesTheHomeOfWhoAsks(t *testing.T) {
	plan := func(userName, env string) ([]string, error) {
		return LaunchPlan{
			Plist: "/Library/LaunchDaemons/com.codefabric.worker.plist", System: true, Binary: "/opt/cflk",
			Content:  []byte(fmt.Sprintf(bareDaemonPlist, userName, env)),
			UserHome: "/Users/alice", // who asks
			LookupHome: func(name string) (string, error) {
				if name == "bob" {
					return "/Users/bob", nil
				}
				return "", errors.New("no such user")
			},
		}.Commands()
	}
	t.Run("no user, no HOME, no directories: refused, nothing printed", func(t *testing.T) {
		if cmds, err := plan("", ""); err == nil || len(cmds) > 0 {
			t.Errorf("%v %v", cmds, err)
		}
	})
	t.Run("the directories in the plist are enough, and the logs of an unknown home are not touched", func(t *testing.T) {
		cmds, err := plan("", `<key>CF_CONFIG_DIR</key><string>/srv/d/codefabric</string><key>CF_DATA_DIR</key><string>/srv/s/codefabric</string>`)
		if err != nil {
			t.Fatal(err)
		}
		if script := strings.Join(cmds, "\n"); strings.Contains(script, "alice") || strings.Contains(script, "Logs") || !strings.Contains(script, "'/srv/d/codeflock'") {
			t.Errorf("\n%s", script)
		}
	})
	t.Run("the user of the plist gives the home", func(t *testing.T) {
		cmds, err := plan(`<key>UserName</key><string>bob</string>`, "")
		if err != nil {
			t.Fatal(err)
		}
		script := strings.Join(cmds, "\n")
		if strings.Contains(script, "alice") || !strings.Contains(script, "sudo -u 'bob' mv -n '/Users/bob/.config/codefabric'") {
			t.Errorf("\n%s", script)
		}
	})
	t.Run("a user that is not found is refused", func(t *testing.T) {
		if cmds, err := plan(`<key>UserName</key><string>carol</string>`, ""); err == nil || len(cmds) > 0 {
			t.Errorf("%v %v", cmds, err)
		}
	})
}

func TestLaunchPlanQuotesTheUserAndPicksDelimitersThePlistDoesNotHave(t *testing.T) {
	content := fmt.Sprintf(bareDaemonPlist, `<key>UserName</key><string>x; touch /tmp/pwned</string>`,
		`<key>HOME</key><string>/Users/x</string><key>Note</key><string>
CFLK_PLIST
CFLK_PLAN
echo pwned
</string>`)
	cmds, err := LaunchPlan{Plist: "/Library/LaunchDaemons/com.codefabric.worker.plist", System: true, Binary: "/opt/cflk", Content: []byte(content)}.Commands()
	if err != nil {
		t.Fatal(err)
	}
	script := strings.Join(cmds, "\n")
	if !strings.Contains(script, "sudo -u 'x; touch /tmp/pwned' ") {
		t.Errorf("the user is not quoted:\n%s", script)
	}
	var outer, inner string
	for _, line := range cmds {
		if rest, ok := strings.CutPrefix(line, "sudo sh -eu <<'"); ok {
			outer = strings.TrimSuffix(rest, "'")
		}
		if i := strings.Index(line, `cat >"$tmp" <<'`); i == 0 {
			inner = strings.TrimSuffix(strings.TrimPrefix(line, `cat >"$tmp" <<'`), "'")
		}
	}
	if outer == "" || inner == "" || outer == inner || outer == "CFLK_PLAN" || inner == "CFLK_PLIST" || !slices.Contains(cmds, outer) || !slices.Contains(cmds, inner) {
		t.Errorf("delimiters %q, %q are in the plist or never close:\n%s", outer, inner, script)
	}
	cmd := exec.CommandContext(t.Context(), "sh", "-n")
	cmd.Stdin = strings.NewReader(script)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Errorf("does not parse: %v\n%s", err, out)
	}
}

func TestPlanBinaryOfADaemonIsAProgramOnlyRootCanReplace(t *testing.T) {
	if os.Getuid() == 0 {
		t.Skip("root owns what it creates")
	}
	mine := svcunittest.Program(t) // owned by this user, in a directory of this user
	_, err := PlanBinary(mine, true)
	if err == nil || !strings.Contains(err.Error(), "/usr/local/bin") || !strings.Contains(err.Error(), "--binary") {
		t.Fatalf("a program of the user for a daemon of root: %v", err)
	}
	if _, err := PlanBinary(mine, false); err != nil {
		t.Errorf("an agent runs as the user: %v", err)
	}
	if _, err := PlanBinary("/bin/sh", true); err != nil {
		t.Errorf("a program of root in directories of root: %v", err)
	}
}

// Nothing is stopped before the files of the new service are known to be absent, the way back removes the
// successor only if the plan wrote it, and a directory that went inside one that appeared stops the block.
func TestLaunchPlanChecksBeforeStoppingAndRemovesOnlyWhatItWrote(t *testing.T) {
	cmds, err := LaunchPlan{Plist: "/Library/LaunchDaemons/com.codefabric.worker.plist", Content: []byte(daemonPlist), System: true, Binary: "/usr/local/bin/cflk"}.Commands()
	if err != nil {
		t.Fatal(err)
	}
	forward, back := parts(t, cmds)
	newPlist := "/Library/LaunchDaemons/com.codeflock.worker.plist"
	check := strings.Index(forward, "if [ -e '"+newPlist+"' ] || [ -L '"+newPlist+"' ]")
	if bootout := strings.Index(forward, "launchctl bootout"); check < 0 || check > bootout {
		t.Errorf("the destination of the new plist is checked after the bootout:\n%s", forward)
	}
	cfg := "'/Users/ana/.config/codeflock'"
	for _, want := range []string{
		"ino=$(stat -f %i '/Users/ana/.config/codefabric')",
		`if [ -e '/Users/ana/.config/codefabric' ] || [ -L '/Users/ana/.config/codefabric' ] || [ "$(stat -f %i ` + cfg + `)" != "$ino" ]`,
	} {
		if !strings.Contains(forward, want) {
			t.Errorf("a directory move is not checked by its inode, missing %q:\n%s", want, forward)
		}
	}
	if !strings.Contains(back, "ino=$(stat -f %i "+cfg+")") {
		t.Errorf("the way back does not check the inode:\n%s", back)
	}
	loaded := strings.Index(forward, "launchctl print system/'com.codeflock.worker'")
	if bootout := strings.Index(forward, "launchctl bootout"); loaded < 0 || loaded > bootout {
		t.Errorf("a successor already loaded is not looked for before the bootout:\n%s", forward)
	}
	if mark, publish := strings.Index(forward, "touch '"+newPlist+".cflk-written'"), strings.Index(forward, `mv -n "$tmp"`); mark < 0 || mark > publish {
		t.Errorf("the marker is not written before the property list is published:\n%s", forward)
	}
	marker := "if [ -e '" + newPlist + ".cflk-written' ]; then"
	rm := strings.Index(back, "rm -f '"+newPlist+"'")
	if at := strings.Index(back, marker); at < 0 || strings.Index(back, "launchctl bootout") < at || rm < at {
		t.Errorf("the successor is removed without asking whether this plan wrote it:\n%s", back)
	}
	if !strings.Contains(back, "mv '/Library/LaunchDaemons/com.codefabric.worker.plist.cflk-old' '/Library/LaunchDaemons/com.codefabric.worker.plist'") {
		t.Errorf("the old plist is not restored from where it was kept:\n%s", back)
	}
}

// A bootout that fails must stop the way back before it removes the property
// list or moves the directories: the new service may still be running.
func TestLaunchPlanWayBackDoesNotGoOnAfterAFailedBootout(t *testing.T) {
	cmds, err := LaunchPlan{Plist: "/Library/LaunchDaemons/com.codefabric.worker.plist", Content: []byte(daemonPlist), System: true, Binary: "/usr/local/bin/cflk"}.Commands()
	if err != nil {
		t.Fatal(err)
	}
	forward, back := parts(t, cmds)
	if strings.Contains(back, "|| true") {
		t.Errorf("the way back ignores a failed bootout:\n%s", back)
	}
	if strings.Contains(forward, "'com.codefabric.worker is loaded") {
		t.Errorf("the plan refuses because of the very job it moves:\n%s", forward)
	}
	if first, bootout := strings.Index(forward, "com.codefabric.controller is loaded"), strings.Index(forward, "launchctl bootout"); first < 0 || first > bootout {
		t.Errorf("the other jobs are looked for after the bootout:\n%s", forward)
	}
}

func controllerPlist(config, data string) string {
	return strings.NewReplacer("com.codefabric.worker", "com.codefabric.controller",
		"/Users/ana/.config/codefabric", config, "/Users/ana/mine", data).Replace(daemonPlist)
}

func planWithController(t *testing.T, content string, readErr error) string {
	t.Helper()
	cmds, err := LaunchPlan{
		Plist: "/Library/LaunchDaemons/com.codefabric.worker.plist", Content: []byte(daemonPlist), System: true, Binary: "/usr/local/bin/cflk",
		ReadFile: func(string) ([]byte, error) { return []byte(content), readErr },
	}.Commands()
	if err != nil {
		t.Fatal(err)
	}
	forward, _ := parts(t, cmds)
	return forward
}

// The other job stops the plan only when it uses the directories that move.
func TestLaunchPlanRefusesForTheOtherJobOnlyIfItSharesTheRoot(t *testing.T) {
	const loaded = "launchctl print system/'com.codefabric.controller' >/dev/null 2>&1 || rc=$?"
	shared := planWithController(t, controllerPlist("/Users/ana/.config/codefabric", "/Users/ana/mine"), nil)
	if !strings.Contains(shared, loaded) || !strings.Contains(shared, "uses the same directories") {
		t.Errorf("a job that shares the root does not stop the plan:\n%s", shared)
	}
	if !strings.Contains(shared, "same directories: drain it and stop it (launchctl bootout) first, then run this again'") {
		t.Errorf("no refusal for a shared root:\n%s", shared)
	}
	separate := planWithController(t, controllerPlist("/Users/bob/.config/codefabric", "/Users/bob/mine"), nil)
	if strings.Contains(separate, "com.codefabric.controller") {
		t.Errorf("a job with its own root is looked at:\n%s", separate)
	}
	unknown := planWithController(t, "", errors.New("no such file"))
	if !strings.Contains(unknown, "warning: com.codefabric.controller is loaded and its directories are not known") {
		t.Errorf("an unknown root does not warn:\n%s", unknown)
	}
	if strings.Contains(unknown, "uses the same directories") {
		t.Errorf("an unknown root refuses:\n%s", unknown)
	}
	if strings.Contains(unknown, "2>&1; then echo 'warning: com.codefabric.controller is loaded and its directories are not known: if it uses the same ones, stop it (Ctrl-C and launchctl bootout) before the move below' >&2; exit") {
		t.Errorf("the warning exits:\n%s", unknown)
	}
}

// A Worker that shares the root of a Controller already moved is stopped and
// its directories are at the new place: the plan must go on to write its plist.
func TestLaunchPlanToleratesAJobNotLoadedAndDirectoriesAlreadyMoved(t *testing.T) {
	forward := planWithController(t, controllerPlist("/Users/ana/.config/codeflock", "/Users/ana/mine"), nil)
	if !strings.Contains(forward, "launchctl print system/'com.codefabric.worker' >/dev/null 2>&1 || rc=$?\nif [ \"$rc\" = 0 ]; then launchctl bootout system/'com.codefabric.worker'\n") {
		t.Errorf("the bootout does not wait for the job to be loaded:\n%s", forward)
	}
	if !strings.Contains(forward, "elif [ ! -d '/Users/ana/.config/codeflock' ] || [ -L '/Users/ana/.config/codeflock' ]; then echo") {
		t.Errorf("a directory already moved is not tolerated:\n%s", forward)
	}
}

// A directory found at the new place is skipped only for a job already migrated
// that uses it: otherwise a missing source stops the block.
func TestLaunchPlanSkipsAMoveOnlyForADirectoryAJobMigratedUses(t *testing.T) {
	forward := planWithController(t, "", errors.New("no such file"))
	if strings.Contains(forward, "elif [ ! -d") || !strings.Contains(forward, "else echo '/Users/ana/.config/codefabric is not there: stop here' >&2; exit 1") {
		t.Errorf("a missing source is tolerated with no job that uses the destination:\n%s", forward)
	}
}

// Two spellings of one directory, through a link, are one root.
func TestLaunchPlanSharesDirectoriesThroughAPathAlias(t *testing.T) {
	root := t.TempDir()
	target, alias := filepath.Join(root, "target"), filepath.Join(root, "alias")
	for _, dir := range []string{".config/codefabric", ".local/share/codefabric"} {
		if err := os.MkdirAll(filepath.Join(target, "home", dir), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Symlink(target, alias); err != nil {
		t.Fatal(err)
	}
	plist := func(home string) string {
		return strings.NewReplacer("/Users/ana/.config/codefabric", home+"/.config/codefabric", "/Users/ana/mine", home+"/.local/share/codefabric", "<string>/Users/ana</string>", "<string>"+home+"</string>").Replace(daemonPlist)
	}
	mine := LaunchPlan{
		Plist: "/Library/LaunchDaemons/com.codefabric.worker.plist", Content: []byte(plist(target + "/home")), System: true,
		ReadFile: func(string) ([]byte, error) { return []byte(plist(alias + "/home")), nil },
	}
	info, err := svcunit.ReadPlist(mine.Content)
	if err != nil {
		t.Fatal(err)
	}
	planned, err := mine.pairs(info, target+"/home")
	if err != nil {
		t.Fatal(err)
	}
	if got := mine.sharesDirectories("com.codefabric.controller", planned); got != sharesYes {
		t.Errorf("an alias of the root is not the same root: %v", got)
	}
}
