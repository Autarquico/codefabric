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

// newLaunchd is a fake launchd of a user whose home is a fresh directory.
func newLaunchd(t *testing.T) (svcunit.Env, *svcunittest.Launchd) {
	t.Helper()
	e := env("darwin", t.TempDir())
	path, _, err := svcunit.Render(e, workerSpec())
	if err != nil {
		t.Fatal(err)
	}
	fake := svcunittest.NewLaunchd(path, workerSpec().Label, e.UID)
	e.Runner = fake
	return e, fake
}

const label = "com.codefabric.worker"

func TestLaunchd_FirstInstallBootstrapsIntoTheGraphicalSession(t *testing.T) {
	e, fake := newLaunchd(t)
	res := install(t, e, workerSpec())
	assertActions(t, res, "write "+fake.Path, "launchctl bootstrap gui/501 "+fake.Path)
	if res.Manager != "launchd" || res.Unit != "gui/501/"+label || len(res.Warnings) != 0 {
		t.Errorf("result = %+v", res)
	}
	if info, err := os.Stat(e.Home + "/Library/Logs/codefabric"); err != nil || info.Mode().Perm() != 0o700 {
		t.Errorf("log directory: %v %v", info, err)
	}
}

func TestLaunchd_WithoutAGraphicalSessionTheUserDomainIsUsedWithAWarning(t *testing.T) {
	e, fake := newLaunchd(t)
	fake.GUI = false
	res := install(t, e, workerSpec())
	assertActions(t, res, "write "+fake.Path, "launchctl bootstrap user/501 "+fake.Path)
	if len(res.Warnings) != 1 || !strings.Contains(res.Warnings[0], "no graphical session") {
		t.Errorf("warnings = %q", res.Warnings)
	}
}

// The domain is chosen only on a fresh install: an agent stays where it is loaded.
func TestLaunchd_AnAgentInTheUserDomainStaysThereWhenTheGUIAppears(t *testing.T) {
	e, fake := newLaunchd(t)
	fake.GUI = false
	install(t, e, workerSpec())
	fake.GUI = true
	spec := workerSpec()
	spec.Argv = append(spec.Argv, "--controller", "other:7443")
	res := install(t, e, spec)
	assertActions(t, res, "write "+fake.Path, "launchctl bootout user/501/"+label, "launchctl bootstrap user/501 "+fake.Path)
}

func TestLaunchd_SecondInstallOnlyReads(t *testing.T) {
	e, fake := newLaunchd(t)
	install(t, e, workerSpec())
	n := len(fake.Calls())
	assertActions(t, install(t, e, workerSpec()))
	if calls := fake.Calls()[n:]; !queriesOnly(calls) {
		t.Errorf("the second install ran %q", calls)
	}
}

func TestLaunchd_InstallRepairsWhatIsMissing(t *testing.T) {
	t.Run("disabled", func(t *testing.T) {
		e, fake := newLaunchd(t)
		install(t, e, workerSpec())
		fake.Load("", false)
		fake.Disabled["gui/501"] = true
		assertActions(t, install(t, e, workerSpec()), "launchctl enable gui/501/"+label, "launchctl bootstrap gui/501 "+fake.Path)
	})
	t.Run("loaded but stopped", func(t *testing.T) {
		e, fake := newLaunchd(t)
		install(t, e, workerSpec())
		fake.Running = false
		// The fake refuses kickstart -k: a stopped agent is started, not killed and started.
		assertActions(t, install(t, e, workerSpec()), "launchctl kickstart gui/501/"+label)
	})
}

func TestLaunchd_ChangedContentIsBootedOutAndBootstrapped(t *testing.T) {
	e, fake := newLaunchd(t)
	install(t, e, workerSpec())
	spec := workerSpec()
	spec.Argv = append(spec.Argv, "--controller", "other:7443")
	assertActions(t, install(t, e, spec),
		"write "+fake.Path, "launchctl bootout gui/501/"+label, "launchctl bootstrap gui/501 "+fake.Path)
}

func TestLaunchd_LoadedInBothDomainsIsAnError(t *testing.T) {
	e, fake := newLaunchd(t)
	install(t, e, workerSpec())
	fake.Twin = true
	for name, f := range map[string]func() (svcunit.Result, error){
		"install":   func() (svcunit.Result, error) { return svcunit.Install(t.Context(), e, workerSpec()) },
		"uninstall": func() (svcunit.Result, error) { return svcunit.Uninstall(t.Context(), e, workerSpec()) },
	} {
		if res, err := f(); err == nil || !strings.Contains(err.Error(), "both") || res.Changed {
			t.Errorf("%s: %+v %v", name, res, err)
		}
	}
}

func TestLaunchd_AnAgentThatWouldKillItsChildrenIsRefused(t *testing.T) {
	e, fake := newLaunchd(t)
	install(t, e, workerSpec())
	fake.Abandon = "runatload | inferred program"
	if _, err := svcunit.Uninstall(t.Context(), e, workerSpec()); err == nil || !strings.Contains(err.Error(), "abandon") {
		t.Errorf("uninstall: %v", err)
	}
	if _, err := os.Stat(fake.Path); err != nil {
		t.Errorf("the refused uninstall removed the file: %v", err)
	}
}

// Not knowing whether the agent abandons its process group is a warning while
// nothing stops it, and a refusal before bootout, on update and on uninstall.
func TestLaunchd_UnknownPropertiesRefuseABootout(t *testing.T) {
	e, fake := newLaunchd(t)
	install(t, e, workerSpec())
	before, err := os.ReadFile(fake.Path)
	if err != nil {
		t.Fatal(err)
	}
	fake.Abandon = "unknown"
	res := install(t, e, workerSpec())
	if len(res.Warnings) != 1 || !strings.Contains(res.Warnings[0], "could not read") {
		t.Errorf("warnings = %q", res.Warnings)
	}

	changed := workerSpec()
	changed.Argv = append(changed.Argv, "--controller", "other:7443")
	n := len(fake.Calls())
	if _, err := svcunit.Install(t.Context(), e, changed); err == nil || !strings.Contains(err.Error(), "launchctl print gui/501/"+label) {
		t.Errorf("update: %v", err)
	}
	if _, err := svcunit.Uninstall(t.Context(), e, workerSpec()); err == nil || !strings.Contains(err.Error(), "abandon process group") {
		t.Errorf("uninstall: %v", err)
	}
	if !queriesOnly(fake.Calls()[n:]) || fake.Domain == "" {
		t.Errorf("ran %q", fake.Calls()[n:])
	}
	if after, err := os.ReadFile(fake.Path); err != nil || !bytes.Equal(after, before) {
		t.Errorf("the plist changed: %v", err)
	}
}

// A change that launchd did not apply is undone, so that installing again
// applies it rather than finding the file already there.
func TestLaunchd_AFailedChangeIsUndoneAndAppliedOnRetry(t *testing.T) {
	changed := workerSpec()
	changed.Argv = append(changed.Argv, "--controller", "other:7443")
	for failing, retry := range map[string][]string{
		"launchctl bootout":   {"launchctl bootout gui/501/" + label, "launchctl bootstrap gui/501 "},
		"launchctl bootstrap": {"launchctl bootstrap gui/501 "}, // the bootout went through
	} {
		t.Run(failing, func(t *testing.T) {
			e, fake := newLaunchd(t)
			install(t, e, workerSpec())
			before, err := os.ReadFile(fake.Path)
			if err != nil {
				t.Fatal(err)
			}
			boom := svcunittest.Exit([]string{"launchctl"}, 5, "Input/output error")
			fake.Fail(failing, boom)
			res, err := svcunit.Install(t.Context(), e, changed)
			if !errors.Is(err, boom) || !slices.Contains(res.Actions, "restore "+fake.Path) {
				t.Fatalf("install: %q %v", res.Actions, err)
			}
			if after, _ := os.ReadFile(fake.Path); !bytes.Equal(after, before) {
				t.Errorf("the plist was left changed:\n%s", after)
			}

			fake.Fail(failing, nil)
			want := append([]string{"write " + fake.Path}, retry...)
			want[len(want)-1] += fake.Path
			assertActions(t, install(t, e, changed), want...)
		})
	}
}

func TestLaunchd_AFailedFreshInstallLeavesNoFile(t *testing.T) {
	e, fake := newLaunchd(t)
	boom := svcunittest.Exit([]string{"launchctl"}, 5, "Input/output error")
	fake.Fail("launchctl bootstrap", boom)
	res, err := svcunit.Install(t.Context(), e, workerSpec())
	if !errors.Is(err, boom) {
		t.Errorf("err = %v", err)
	}
	assertActions(t, res, "write "+fake.Path, "remove "+fake.Path)
	fake.Fail("launchctl bootstrap", nil)
	assertActions(t, install(t, e, workerSpec()), "write "+fake.Path, "launchctl bootstrap gui/501 "+fake.Path)
}

func TestLaunchd_AnAgentLoadedFromElsewhereIsLeftAlone(t *testing.T) {
	e, fake := newLaunchd(t)
	install(t, e, workerSpec())
	other := t.TempDir() + "/other.plist"
	if err := os.WriteFile(other, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	fake.Path = other
	fake.Load("gui/501", true)
	if _, err := svcunit.Uninstall(t.Context(), e, workerSpec()); err == nil || !strings.Contains(err.Error(), "leaving it alone") {
		t.Errorf("uninstall: %v", err)
	}
}

func TestLaunchd_UninstallBootsOutAndRemoves(t *testing.T) {
	e, fake := newLaunchd(t)
	install(t, e, workerSpec())
	res, err := svcunit.Uninstall(t.Context(), e, workerSpec())
	if err != nil {
		t.Fatal(err)
	}
	assertActions(t, res, "launchctl bootout gui/501/"+label, "remove "+fake.Path)
	res, err = svcunit.Uninstall(t.Context(), e, workerSpec())
	if err != nil || res.Changed {
		t.Errorf("uninstalling again: %+v %v", res, err)
	}
}

func TestLaunchd_UninstallPropagatesFailuresAndKeepsTheFile(t *testing.T) {
	e, fake := newLaunchd(t)
	install(t, e, workerSpec())
	boom := svcunittest.Exit([]string{"launchctl", "bootout"}, 5, "Boot-out failed: 5: Input/output error")
	fake.Fail("launchctl bootout", boom)
	if _, err := svcunit.Uninstall(t.Context(), e, workerSpec()); !errors.Is(err, boom) {
		t.Errorf("err = %v", err)
	}
	if _, err := os.Stat(fake.Path); err != nil {
		t.Errorf("the plist went: %v", err)
	}
}

// Only "no such service" (113) and "no such domain" (112) mean the agent is
// not loaded: any other failure of `launchctl print` cannot tell, and the plist stays.
func TestLaunchd_APrintThatCannotTellIsAnError(t *testing.T) {
	e, fake := newLaunchd(t)
	install(t, e, workerSpec())
	denied := svcunittest.Exit([]string{"launchctl", "print"}, 1, "Could not print domain: 1: Operation not permitted")
	fake.Fail("launchctl print gui/501/", denied)
	res, err := svcunit.Uninstall(t.Context(), e, workerSpec())
	if !errors.Is(err, denied) || res.Changed {
		t.Errorf("uninstall: %+v %v", res, err)
	}
	if _, err := os.Stat(fake.Path); err != nil {
		t.Errorf("the plist went: %v", err)
	}
}

func TestLaunchd_ARefusedInstallEnablesNothing(t *testing.T) {
	e, fake := newLaunchd(t)
	install(t, e, workerSpec())
	fake.Abandon = "runatload | inferred program"
	fake.Disabled["gui/501"] = true
	res, err := svcunit.Install(t.Context(), e, workerSpec())
	if err == nil || !strings.Contains(err.Error(), "abandon") || res.Changed {
		t.Errorf("install: %+v %v", res, err)
	}
	if !fake.Disabled["gui/501"] {
		t.Error("the refused install enabled the agent")
	}
}
