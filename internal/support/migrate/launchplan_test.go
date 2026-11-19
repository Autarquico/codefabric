package migrate

import (
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"
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

func TestLaunchPlanForADaemonPrintsEveryCommandWithSudoAndTheWayBack(t *testing.T) {
	cmds, err := LaunchPlan{Plist: "/Library/LaunchDaemons/com.codefabric.worker.plist", Content: []byte(daemonPlist), System: true, Binary: "/usr/local/bin/cflk"}.Commands()
	if err != nil {
		t.Fatal(err)
	}
	script := strings.Join(cmds, "\n")
	for _, want := range []string{
		"sudo launchctl bootout system/com.codefabric.worker",
		"sudo -u 'ana' mv -n '/Users/ana/.config/codefabric' '/Users/ana/.config/codeflock'",
		"sudo -u 'ana' mv -n '/Users/ana/Library/Logs/codefabric' '/Users/ana/Library/Logs/codeflock'",
		"sudo tee '/Library/LaunchDaemons/com.codeflock.worker.plist'",
		"sudo chown root:wheel '/Library/LaunchDaemons/com.codeflock.worker.plist'",
		"sudo launchctl bootstrap system '/Library/LaunchDaemons/com.codeflock.worker.plist'",
		"<string>com.codeflock.worker</string>", "<key>CFLK_DATA_DIR</key>", "<string>/usr/local/bin/cflk</string>",
		"/Users/ana/Library/Logs/codeflock/com.codeflock.worker.log", "<string>/Users/ana/.config/codeflock</string>",
		"sudo launchctl bootout system/com.codeflock.worker",
		"sudo -u 'ana' mv -n '/Users/ana/.config/codeflock' '/Users/ana/.config/codefabric'",
		"[ ! -e '/Users/ana/.config/codeflock' ]", // it never goes into a destination that is there
		"sudo launchctl bootstrap system '/Library/LaunchDaemons/com.codefabric.worker.plist'",
		"sudo rm '/Library/LaunchDaemons/com.codefabric.worker.plist'",
	} {
		if !strings.Contains(script, want) {
			t.Errorf("missing %q in:\n%s", want, script)
		}
	}
	// The directory the owner chose is theirs: it is not moved, and the plist keeps its path.
	if strings.Contains(script, "mv -n '/Users/ana/mine'") || !strings.Contains(script, "<string>/Users/ana/mine</string>") {
		t.Errorf("a chosen directory moved:\n%s", script)
	}
	// The way back puts the logs back last-moved-first.
	if strings.Index(script, "mv -n '/Users/ana/Library/Logs/codeflock'") > strings.Index(script, "mv -n '/Users/ana/.config/codeflock'") {
		t.Errorf("rollback is not in reverse order:\n%s", script)
	}
}

// A plan that is pasted whole must stop at a move that was refused: it must not
// go on to write and load the new property list.
func TestLaunchPlanStopsAtAMoveThatWasRefused(t *testing.T) {
	cmds, err := LaunchPlan{Plist: "/Library/LaunchDaemons/com.codefabric.worker.plist", Content: []byte(daemonPlist), System: true}.Commands()
	if err != nil {
		t.Fatal(err)
	}
	moves := 0
	for _, line := range cmds {
		if !strings.Contains(line, "mv -n ") {
			continue
		}
		moves++
		if !strings.Contains(line, "|| { echo ") || !strings.HasSuffix(line, "; exit 1; }") {
			t.Errorf("a refused move does not stop the script: %s", line)
		}
	}
	if moves == 0 {
		t.Fatal("no moves in the plan")
	}
}

func TestLaunchPlanForAnAgentNeedsNoSudo(t *testing.T) {
	cmds, err := LaunchPlan{Plist: "/Users/ana/Library/LaunchAgents/com.codefabric.controller.plist", Content: []byte(daemonPlist)}.Commands()
	if err != nil {
		t.Fatal(err)
	}
	script := strings.Join(cmds, "\n")
	if strings.Contains(script, "sudo") {
		t.Errorf("sudo in a user's agent:\n%s", script)
	}
	if !strings.Contains(script, "launchctl bootout gui/$(id -u)/com.codefabric.controller") {
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

func TestLaunchPlanQuotesTheUserAndPicksADelimiterThePlistDoesNotHave(t *testing.T) {
	content := fmt.Sprintf(bareDaemonPlist, `<key>UserName</key><string>x; touch /tmp/pwned</string>`,
		`<key>HOME</key><string>/Users/x</string><key>Note</key><string>
CFLK_PLIST
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
	var open string
	for _, line := range cmds {
		if rest, ok := strings.CutPrefix(line, "sudo tee "); ok {
			open = rest[strings.Index(rest, "<<'")+3 : len(rest)-1]
		}
	}
	if open == "" || open == "CFLK_PLIST" || !slices.Contains(cmds, open) {
		t.Errorf("delimiter %q is in the plist or never closes:\n%s", open, script)
	}
}
