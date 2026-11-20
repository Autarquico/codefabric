package migrate

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// planRun is a plan of an agent in a temporary home, run for real with a
// launchctl that answers `print` as told and records the rest.
type planRun struct {
	t       *testing.T
	home    string
	plist   string
	bin     string
	log     string
	forward string
	back    string
}

func newPlanRun(t *testing.T) *planRun {
	t.Helper()
	home := t.TempDir()
	agents := filepath.Join(home, "LaunchAgents")
	bin := filepath.Join(t.TempDir(), "bin")
	for _, dir := range []string{agents, bin, filepath.Join(home, ".config", "codefabric"), filepath.Join(home, ".local", "share", "codefabric")} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	fake := `#!/bin/sh
echo "$@" >>"$FAKE_LOG"
if [ "$1" = print ]; then
	case "$2" in
	*codeflock*) exit "${FAKE_NEW:-113}" ;;
	*) exit "${FAKE_OLD:-113}" ;;
	esac
fi
exit 0
`
	if err := os.WriteFile(filepath.Join(bin, "launchctl"), []byte(fake), 0o755); err != nil { //nolint:gosec // the fake must be executable
		t.Fatal(err)
	}
	content := strings.NewReplacer("/Users/ana", home).Replace(`<?xml version="1.0" encoding="UTF-8"?>
<plist version="1.0">
<dict>
	<key>Label</key>
	<string>com.codefabric.worker</string>
	<key>ProgramArguments</key>
	<array>
		<string>/usr/local/bin/cf</string>
	</array>
	<key>EnvironmentVariables</key>
	<dict>
		<key>HOME</key>
		<string>/Users/ana</string>
	</dict>
</dict>
</plist>
`)
	r := &planRun{t: t, home: home, plist: filepath.Join(agents, "com.codefabric.worker.plist"), bin: bin, log: filepath.Join(t.TempDir(), "log")}
	if err := os.WriteFile(r.plist, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	cmds, err := LaunchPlan{Plist: r.plist, Content: []byte(content), Binary: "/opt/cflk"}.Commands()
	if err != nil {
		t.Fatal(err)
	}
	var back string
	r.forward, back = parts(t, cmds)
	for _, line := range strings.Split(back, "\n")[1:] {
		r.back += strings.TrimPrefix(line, "# ") + "\n"
	}
	return r
}

func (r *planRun) run(script string, env ...string) (string, error) {
	cmd := exec.CommandContext(r.t.Context(), "sh")
	cmd.Stdin = strings.NewReader(script)
	cmd.Env = append([]string{"PATH=" + r.bin + ":/usr/bin:/bin:/usr/sbin:/sbin", "FAKE_LOG=" + r.log}, env...)
	out, err := cmd.CombinedOutput()
	return string(out), err
}

func (r *planRun) path(parts ...string) string {
	return filepath.Join(append([]string{r.home}, parts...)...)
}

func (r *planRun) logged() string {
	b, _ := os.ReadFile(r.log)
	return string(b)
}

func TestLaunchPlanRunsWhenLaunchdSaysNotLoaded(t *testing.T) {
	r := newPlanRun(t)
	if out, err := r.run(r.forward); err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	for _, p := range []string{".config/codeflock", ".local/share/codeflock", "LaunchAgents/com.codeflock.worker.plist.cflk-moved-0", "LaunchAgents/com.codeflock.worker.plist.cflk-moved-1"} {
		if !has(t, r.path(p)) {
			t.Errorf("%s is not there after the plan", p)
		}
	}
	if strings.Contains(r.logged(), "bootout") {
		t.Errorf("a job that is not loaded is stopped:\n%s", r.logged())
	}
}

func TestLaunchPlanStopsWhenLaunchdCannotSay(t *testing.T) {
	r := newPlanRun(t)
	out, err := r.run(r.forward, "FAKE_OLD=5", "FAKE_NEW=113")
	if err == nil || !strings.Contains(out, "cannot tell whether com.codefabric.") {
		t.Fatalf("a probe that fails with 5 does not stop the plan: %v\n%s", err, out)
	}
	if !has(t, r.path(".config/codefabric")) || has(t, r.path(".config/codeflock")) {
		t.Error("something moved after a probe that could not say")
	}
}

func TestLaunchPlanBootsOutALoadedJobBeforeMoving(t *testing.T) {
	r := newPlanRun(t)
	if out, err := r.run(r.forward, "FAKE_OLD=0"); err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	if !strings.Contains(r.logged(), "bootout gui/") || !strings.Contains(r.logged(), "bootstrap gui/") {
		t.Errorf("the loaded job was not stopped and the new one loaded:\n%s", r.logged())
	}
}

func TestLaunchPlanRefusesALoadedSuccessorAndOneItCannotTell(t *testing.T) {
	for env, want := range map[string]string{"FAKE_NEW=0": "is loaded already", "FAKE_NEW=113": "", "FAKE_NEW=7": "cannot tell whether com.codeflock.worker"} {
		r := newPlanRun(t)
		out, err := r.run(r.forward, env)
		if want == "" {
			if err != nil {
				t.Errorf("%s: %v\n%s", env, err, out)
			}
			continue
		}
		if err == nil || !strings.Contains(out, want) {
			t.Errorf("%s: want a stop saying %q: %v\n%s", env, want, err, out)
		}
	}
}

// The way back moves only what this plan moved: a directory another job moved,
// which the plan skipped, stays where it is.
func TestLaunchPlanWayBackMovesOnlyWhatThePlanMoved(t *testing.T) {
	r := newPlanRun(t)
	if err := os.Rename(r.path(".config/codefabric"), r.path(".config/codeflock")); err != nil { // a Controller moved it
		t.Fatal(err)
	}
	controller := strings.NewReplacer("worker", "controller", "<key>HOME</key>", "<key>CFLK_CONFIG_DIR</key><string>"+r.path(".config/codeflock")+"</string><key>HOME</key>").Replace(mustRead(t, r.plist))
	if err := os.WriteFile(filepath.Join(filepath.Dir(r.plist), "com.codeflock.controller.plist"), []byte(controller), 0o600); err != nil {
		t.Fatal(err)
	}
	cmds, err := LaunchPlan{Plist: r.plist, Content: []byte(mustRead(t, r.plist)), Binary: "/opt/cflk"}.Commands()
	if err != nil {
		t.Fatal(err)
	}
	forward, _ := parts(t, cmds)
	if out, err := r.run(forward); err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	if has(t, r.path("LaunchAgents/com.codeflock.worker.plist.cflk-moved-0")) || !has(t, r.path("LaunchAgents/com.codeflock.worker.plist.cflk-moved-1")) {
		t.Fatal("the marks are not those of what the plan moved")
	}
	if out, err := r.run(r.back); err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	if !has(t, r.path(".config/codeflock")) || has(t, r.path(".config/codefabric")) {
		t.Error("the way back moved a directory the plan did not")
	}
	if !has(t, r.path(".local/share/codefabric")) || has(t, r.path(".local/share/codeflock")) || has(t, r.path("LaunchAgents/com.codeflock.worker.plist.cflk-moved-1")) {
		t.Error("the way back did not undo what the plan moved, or left its mark")
	}
}

func TestLaunchPlanWayBackStopsWhenLaunchdCannotSay(t *testing.T) {
	r := newPlanRun(t)
	if out, err := r.run(r.forward); err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	if _, err := r.run(r.back, "FAKE_NEW=5"); err == nil {
		t.Error("the way back went on with a successor it cannot tell is loaded")
	}
	if !has(t, r.path(".config/codeflock")) {
		t.Error("the way back moved a directory after a probe that could not say")
	}
}

func TestLaunchPlanStopsOnAMissingSourceWhoseDestinationIsStale(t *testing.T) {
	r := newPlanRun(t)
	if err := os.Rename(r.path(".config/codefabric"), r.path(".config/codeflock")); err != nil { // nobody uses it
		t.Fatal(err)
	}
	if out, err := r.run(r.forward); err == nil || !strings.Contains(out, "is not there") || has(t, r.path("LaunchAgents/com.codeflock.worker.plist")) {
		t.Errorf("a stale destination stands in for the source: %v\n%s", err, out)
	}
}

func mustRead(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path) //nolint:gosec // a path of the test
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}
