package migrate

import (
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
		"sudo -u ana mv '/Users/ana/.config/codefabric' '/Users/ana/.config/codeflock'",
		"sudo -u ana mv '/Users/ana/Library/Logs/codefabric' '/Users/ana/Library/Logs/codeflock'",
		"sudo tee '/Library/LaunchDaemons/com.codeflock.worker.plist'",
		"sudo chown root:wheel '/Library/LaunchDaemons/com.codeflock.worker.plist'",
		"sudo launchctl bootstrap system '/Library/LaunchDaemons/com.codeflock.worker.plist'",
		"<string>com.codeflock.worker</string>", "<key>CFLK_DATA_DIR</key>", "<string>/usr/local/bin/cflk</string>",
		"/Users/ana/Library/Logs/codeflock/com.codeflock.worker.log", "<string>/Users/ana/.config/codeflock</string>",
		"sudo launchctl bootout system/com.codeflock.worker",
		"sudo -u ana mv '/Users/ana/.config/codeflock' '/Users/ana/.config/codefabric'",
		"sudo launchctl bootstrap system '/Library/LaunchDaemons/com.codefabric.worker.plist'",
		"sudo rm '/Library/LaunchDaemons/com.codefabric.worker.plist'",
	} {
		if !strings.Contains(script, want) {
			t.Errorf("missing %q in:\n%s", want, script)
		}
	}
	// The directory the owner chose is theirs: it is not moved, and the plist keeps its path.
	if strings.Contains(script, "mv '/Users/ana/mine'") || !strings.Contains(script, "<string>/Users/ana/mine</string>") {
		t.Errorf("a chosen directory moved:\n%s", script)
	}
	// The way back puts the logs back last-moved-first.
	if strings.Index(script, "mv '/Users/ana/Library/Logs/codeflock'") > strings.Index(script, "mv '/Users/ana/.config/codeflock'") {
		t.Errorf("rollback is not in reverse order:\n%s", script)
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
