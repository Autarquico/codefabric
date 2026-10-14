package svcunit_test

import (
	"bytes"
	"errors"
	"os"
	"slices"
	"strings"
	"testing"

	"github.com/Autarquico/codefabric/internal/support/svcunit"
	"github.com/Autarquico/codefabric/internal/support/svcunit/svcunittest"
)

// newSystemd is a fake systemd of a user whose home is a fresh directory.
func newSystemd(t *testing.T) (svcunit.Env, *svcunittest.Systemd) {
	t.Helper()
	e := env("linux", t.TempDir())
	path, _, err := svcunit.Render(e, workerSpec())
	if err != nil {
		t.Fatal(err)
	}
	fake := svcunittest.NewSystemd(path)
	e.Runner = fake
	return e, fake
}

func install(t *testing.T, e svcunit.Env, s svcunit.Spec) svcunit.Result {
	t.Helper()
	res, err := svcunit.Install(t.Context(), e, s)
	if err != nil {
		t.Fatal(err)
	}
	return res
}

func assertActions(t *testing.T, res svcunit.Result, want ...string) {
	t.Helper()
	if !slices.Equal(res.Actions, want) || res.Changed != (len(want) > 0) {
		t.Errorf("actions = %q (changed %t), want %q", res.Actions, res.Changed, want)
	}
}

// queriesOnly says whether every command was one that only reads.
func queriesOnly(calls []string) bool {
	for _, c := range calls {
		if !strings.HasPrefix(c, "systemctl --user show ") && !strings.HasPrefix(c, "loginctl show-user ") &&
			!strings.HasPrefix(c, "launchctl print") {
			return false
		}
	}
	return true
}

func TestSystemd_FirstInstallWritesReloadsEnablesAndStarts(t *testing.T) {
	e, fake := newSystemd(t)
	res := install(t, e, workerSpec())
	assertActions(t, res, "write "+fake.Path, "systemctl --user daemon-reload",
		"systemctl --user enable --no-reload cf-worker.service", "systemctl --user start cf-worker.service")
	if res.Manager != "systemd" || res.Unit != "cf-worker.service" || res.Path != fake.Path || len(res.Warnings) != 0 {
		t.Errorf("result = %+v", res)
	}
	if info, err := os.Stat(fake.Path); err != nil || info.Mode().Perm() != 0o644 {
		t.Errorf("unit file: %v %v", info, err)
	}
}

func TestSystemd_SecondInstallOnlyReads(t *testing.T) {
	e, fake := newSystemd(t)
	install(t, e, workerSpec())
	before, err := os.Stat(fake.Path)
	if err != nil {
		t.Fatal(err)
	}
	n := len(fake.Calls())

	res := install(t, e, workerSpec())
	assertActions(t, res)
	if calls := fake.Calls()[n:]; !queriesOnly(calls) {
		t.Errorf("the second install ran %q", calls)
	}
	after, err := os.Stat(fake.Path)
	if err != nil || !os.SameFile(before, after) || !after.ModTime().Equal(before.ModTime()) {
		t.Errorf("the unit file was rewritten")
	}
}

func TestSystemd_InstallRepairsWhatIsMissing(t *testing.T) {
	cases := []struct {
		name            string
		enabled, active bool
		needReload      bool
		want            []string
	}{
		{"enabled but inactive", true, false, false, []string{"systemctl --user start cf-worker.service"}},
		{"disabled but active", false, true, false, []string{"systemctl --user enable --no-reload cf-worker.service"}},
		{"identical but needs a reload", true, true, true, []string{"systemctl --user daemon-reload"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			e, fake := newSystemd(t)
			install(t, e, workerSpec())
			fake.Loaded(c.enabled, c.active)
			fake.NeedDaemonReload = c.needReload
			assertActions(t, install(t, e, workerSpec()), c.want...)
		})
	}
}

func TestSystemd_ChangedContentIsRewrittenReloadedAndRestarted(t *testing.T) {
	e, fake := newSystemd(t)
	install(t, e, workerSpec())
	spec := workerSpec()
	spec.Argv = append(spec.Argv, "--controller", "other:7443")
	assertActions(t, install(t, e, spec),
		"write "+fake.Path, "systemctl --user daemon-reload", "systemctl --user restart cf-worker.service")
}

// A drop-in that changes KillMode would make every restart kill the Runs:
// install refuses before it starts or restarts anything.
func TestSystemd_DropInKillModeIsRefused(t *testing.T) {
	e, fake := newSystemd(t)
	install(t, e, workerSpec())
	fake.DropInKillMode = "control-group"
	fake.Loaded(true, true)
	spec := workerSpec()
	spec.Argv = append(spec.Argv, "--controller", "other:7443")
	n := len(fake.Calls())
	res, err := svcunit.Install(t.Context(), e, spec)
	if err == nil || !strings.Contains(err.Error(), "KillMode") {
		t.Fatalf("err = %v", err)
	}
	for _, c := range fake.Calls()[n:] {
		if strings.Contains(c, "restart") || strings.Contains(c, "start") || strings.Contains(c, "stop") {
			t.Errorf("ran %q", c)
		}
	}
	if slices.Contains(res.Actions, "systemctl --user restart cf-worker.service") {
		t.Errorf("actions = %q", res.Actions)
	}
	if _, err := svcunit.Uninstall(t.Context(), e, workerSpec()); err == nil || !strings.Contains(err.Error(), "KillMode") {
		t.Errorf("uninstall: %v", err)
	}
	if _, err := os.Stat(fake.Path); err != nil {
		t.Errorf("the refused uninstall removed the file: %v", err)
	}
}

func TestSystemd_AUnitLoadedFromElsewhereIsRefused(t *testing.T) {
	e, fake := newSystemd(t)
	install(t, e, workerSpec())
	fake.Fragment = "/etc/systemd/user/cf-worker.service"
	if _, err := svcunit.Install(t.Context(), e, workerSpec()); err == nil || !strings.Contains(err.Error(), "shadows") {
		t.Errorf("install: %v", err)
	}
	if _, err := svcunit.Uninstall(t.Context(), e, workerSpec()); err == nil {
		t.Error("uninstall went on")
	}
}

// A systemd that does not find the unit where it was written says so, rather
// than reporting the KillMode of a unit it never loaded.
func TestSystemd_AUnitTheManagerDoesNotFindIsReportedAsSuch(t *testing.T) {
	e, fake := newSystemd(t)
	fake.Elsewhere = true
	res, err := svcunit.Install(t.Context(), e, workerSpec())
	if err == nil || !strings.Contains(err.Error(), "does not find") || strings.Contains(err.Error(), "KillMode") {
		t.Errorf("install: %v", err)
	}
	// The refused file goes: what is left is what was there.
	assertActions(t, res, "write "+fake.Path, "systemctl --user daemon-reload", "remove "+fake.Path, "systemctl --user daemon-reload")
}

func TestSystemd_WarnsWithoutLinger(t *testing.T) {
	for linger, want := range map[string]string{"no": "lingering is off", "": "could not tell"} {
		e, fake := newSystemd(t)
		fake.Linger = linger
		res := install(t, e, workerSpec())
		if len(res.Warnings) != 1 || !strings.Contains(res.Warnings[0], want) {
			t.Errorf("linger %q: warnings %q", linger, res.Warnings)
		}
	}
}

// The Controller's unit keeps systemd's KillMode: its children go with it.
func TestSystemd_WithoutKeepProcessesKillModeIsNotChecked(t *testing.T) {
	e := env("linux", t.TempDir())
	path, content, err := svcunit.Render(e, controllerSpec())
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(content), "KillMode") {
		t.Errorf("the Controller's unit sets KillMode:\n%s", content)
	}
	e.Runner = svcunittest.NewSystemd(path)
	install(t, e, controllerSpec())
}

func TestSystemd_UninstallStopsDisablesAndRemoves(t *testing.T) {
	e, fake := newSystemd(t)
	install(t, e, workerSpec())
	res, err := svcunit.Uninstall(t.Context(), e, workerSpec())
	if err != nil {
		t.Fatal(err)
	}
	assertActions(t, res, "systemctl --user stop cf-worker.service", "systemctl --user disable --no-reload cf-worker.service",
		"remove "+fake.Path, "systemctl --user daemon-reload")
	if _, err := os.Stat(fake.Path); !os.IsNotExist(err) {
		t.Errorf("the unit file is still there: %v", err)
	}
	res, err = svcunit.Uninstall(t.Context(), e, workerSpec())
	if err != nil || res.Changed {
		t.Errorf("uninstalling again: %+v %v", res, err)
	}
}

func TestSystemd_UninstallPropagatesFailuresAndKeepsTheFile(t *testing.T) {
	for _, failing := range []string{"systemctl --user show", "systemctl --user stop", "systemctl --user disable"} {
		t.Run(failing, func(t *testing.T) {
			e, fake := newSystemd(t)
			install(t, e, workerSpec())
			boom := errors.New("Failed to connect to bus")
			fake.Fail(failing, boom)
			if _, err := svcunit.Uninstall(t.Context(), e, workerSpec()); !errors.Is(err, boom) {
				t.Errorf("err = %v", err)
			}
			if _, err := os.Stat(fake.Path); err != nil {
				t.Errorf("the unit file went: %v", err)
			}
		})
	}
}

// A change that systemd did not apply is undone, so that installing again
// applies it rather than finding the file already there.
func TestSystemd_AFailedChangeIsUndoneAndAppliedOnRetry(t *testing.T) {
	changed := workerSpec()
	changed.Argv = append(changed.Argv, "--controller", "other:7443")
	for _, failing := range []string{"systemctl --user daemon-reload", "systemctl --user restart"} {
		t.Run(failing, func(t *testing.T) {
			e, fake := newSystemd(t)
			install(t, e, workerSpec())
			before, err := os.ReadFile(fake.Path)
			if err != nil {
				t.Fatal(err)
			}
			boom := errors.New("Failed to connect to bus")
			fake.Fail(failing, boom)
			res, err := svcunit.Install(t.Context(), e, changed)
			if !errors.Is(err, boom) {
				t.Fatalf("err = %v", err)
			}
			if after, _ := os.ReadFile(fake.Path); !bytes.Equal(after, before) {
				t.Errorf("the unit file was left changed:\n%s", after)
			}
			if failing == "systemctl --user restart" && !slices.Contains(res.Actions, "restore "+fake.Path) {
				t.Errorf("actions = %q", res.Actions)
			}

			fake.Fail(failing, nil)
			assertActions(t, install(t, e, changed),
				"write "+fake.Path, "systemctl --user daemon-reload", "systemctl --user restart cf-worker.service")
		})
	}
}

// A fresh install that fails leaves no file behind, and the error says when
// putting things back failed too.
func TestSystemd_AFailedFreshInstallLeavesNoFile(t *testing.T) {
	e, fake := newSystemd(t)
	boom := errors.New("Failed to connect to bus")
	fake.Fail("systemctl --user daemon-reload", boom)
	_, err := svcunit.Install(t.Context(), e, workerSpec())
	if !errors.Is(err, boom) || !strings.Contains(err.Error(), "putting back the previous file failed too") {
		t.Errorf("err = %v", err)
	}
	if _, err := os.Stat(fake.Path); !os.IsNotExist(err) {
		t.Errorf("the unit file is there: %v", err)
	}
	fake.Fail("systemctl --user daemon-reload", nil)
	assertActions(t, install(t, e, workerSpec()), "write "+fake.Path, "systemctl --user daemon-reload",
		"systemctl --user enable --no-reload cf-worker.service", "systemctl --user start cf-worker.service")
}
