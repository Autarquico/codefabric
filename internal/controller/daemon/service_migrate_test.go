package daemon_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Autarquico/codeflock/internal/adapters/runtime/tmux"
	"github.com/Autarquico/codeflock/internal/config"
	"github.com/Autarquico/codeflock/internal/controller/daemon"
	"github.com/Autarquico/codeflock/internal/support/svcunit"
	"github.com/Autarquico/codeflock/internal/support/svcunit/svcunittest"
)

// oldController is a Controller initialised under the old directory names,
// with a service of the old name running.
func oldController(t *testing.T) (daemon.ServiceOptions, *svcunittest.SystemdFleet, config.Paths) {
	t.Helper()
	home := t.TempDir()
	old := config.Paths{ConfigDir: filepath.Join(home, ".config", "codefabric"), DataDir: filepath.Join(home, ".local", "share", "codefabric")}
	initController(t, old)
	fleet := svcunittest.NewSystemdFleet(home)
	env := svcunit.Env{Runner: fleet, Home: home, UID: 1000, GOOS: "linux"}
	spec := svcunit.Spec{
		Name: "cf-controller", Label: "com.codefabric.controller", Installer: "cf controller install-service",
		Argv: []string{"/opt/cf/bin/cf", "controller", "run"},
		Env:  []svcunit.KV{{Key: "CF_CONFIG_DIR", Value: old.ConfigDir}, {Key: "CF_DATA_DIR", Value: old.DataDir}, {Key: "PATH", Value: "/usr/bin"}},
	}
	if _, err := svcunit.Install(t.Context(), env, spec); err != nil {
		t.Fatal(err)
	}
	fleet.Set("cf-controller.service", true, true)
	return daemon.ServiceOptions{
		Paths: old, Binary: svcunittest.Program(t), PATH: "/usr/bin:/bin", Manager: env,
		Settle: func(context.Context) error { return nil },
	}, fleet, old
}

func TestScenario_RenameCLI_Controller_MueveElServicioYLosDirectorios(t *testing.T) {
	o, fleet, old := oldController(t)
	dbBefore, err := os.ReadFile(old.DatabaseFile())
	if err != nil {
		t.Fatal(err)
	}

	res, err := daemon.InstallService(t.Context(), o)
	if err != nil {
		t.Fatal(err)
	}

	if !res.Changed || fleet.Active("cf-controller.service") || !fleet.Active("codeflock-controller.service") {
		t.Errorf("%+v", res)
	}
	home := o.Manager.Home
	moved := config.Paths{ConfigDir: filepath.Join(home, ".config", "codeflock"), DataDir: filepath.Join(home, ".local", "share", "codeflock")}
	if got, err := os.ReadFile(moved.DatabaseFile()); err != nil || string(got) != string(dbBefore) {
		t.Errorf("the database did not move intact: %v", err)
	}
	for _, p := range []string{moved.ControllerConfigFile(), moved.PKIDir()} {
		if _, err := os.Stat(p); err != nil {
			t.Errorf("%v", err)
		}
	}
	if _, err := os.Stat(old.DataDir); err == nil {
		t.Error("the old data directory is still there")
	}
	got, _, _ := svcunit.InstalledEnv(o.Manager, svcunit.Spec{Name: "codeflock-controller", Label: "com.codeflock.controller"})
	if got["CFLK_DATA_DIR"] != moved.DataDir || got["CFLK_CONFIG_DIR"] != moved.ConfigDir {
		t.Errorf("env = %v", got)
	}
}

func TestInstallService_ControllerFailureAfterTheMovePutsEverythingBack(t *testing.T) {
	o, fleet, old := oldController(t)
	fleet.Fail("systemctl --user start codeflock-controller", errors.New("boom"))
	if _, err := daemon.InstallService(t.Context(), o); err == nil || !strings.Contains(err.Error(), "boom") {
		t.Fatalf("err = %v", err)
	}
	if !fleet.Active("cf-controller.service") {
		t.Error("the old Controller does not run again")
	}
	for _, p := range []string{old.DatabaseFile(), old.ControllerConfigFile()} {
		if _, err := os.Stat(p); err != nil {
			t.Errorf("%v", err)
		}
	}
	if _, err := os.Stat(filepath.Join(o.Manager.Home, ".local", "share", "codeflock")); err == nil {
		t.Error("the new data directory is left")
	}
}

func TestScenario_RenameCLI_ControllerEnLaunchd_ImprimeLosComandos(t *testing.T) {
	home := t.TempDir()
	dir := filepath.Join(home, "Library", "LaunchAgents")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	plist := `<plist><dict><key>Label</key><string>com.codefabric.controller</string>
<key>ProgramArguments</key><array><string>/usr/local/bin/cf</string><string>controller</string><string>run</string></array>
<key>EnvironmentVariables</key><dict><key>CF_CONFIG_DIR</key><string>` + home + `/.config/codefabric</string>
<key>CF_DATA_DIR</key><string>` + home + `/.local/share/codefabric</string></dict></dict></plist>`
	path := filepath.Join(dir, "com.codefabric.controller.plist")
	if err := os.WriteFile(path, []byte(plist), 0o600); err != nil {
		t.Fatal(err)
	}
	fake := svcunittest.NewLaunchd(path, "com.codefabric.controller", 501)
	o := daemon.ServiceOptions{Paths: newPaths(t), Manager: svcunit.Env{Runner: fake, Home: home, UID: 501, GOOS: "darwin"}, Home: home}

	_, err := daemon.InstallService(t.Context(), o)

	if err == nil {
		t.Fatal("moved a launchd Controller")
	}
	for _, want := range []string{
		"launchctl bootout gui/$(id -u)/com.codefabric.controller",
		"mv -n '" + home + "/.local/share/codefabric' '" + home + "/.local/share/codeflock'",
		"tee '" + filepath.Join(dir, "com.codeflock.controller.plist") + "'",
		"launchctl bootstrap gui/$(id -u) '" + path + "'", // the way back
	} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("missing %q in:\n%s", want, err)
		}
	}
	if len(fake.Calls()) != 0 {
		t.Errorf("launchctl was run: %v", fake.Calls())
	}
	if got, _ := os.ReadFile(path); string(got) != plist { //nolint:gosec // a file of the test
		t.Error("the plist changed")
	}
}

// withWorker is a Worker of the old name on the same root, with its program and arguments.
func withWorker(t *testing.T, o daemon.ServiceOptions, old config.Paths) {
	t.Helper()
	spec := svcunit.Spec{
		Name: "cf-worker", Label: "com.codefabric.worker", KeepProcesses: true, Installer: "cf worker install-service",
		Argv: []string{"/opt/cf/bin/cf", "worker", "run", "--ssh-target", "me@host"},
		Env:  []svcunit.KV{{Key: "CF_CONFIG_DIR", Value: old.ConfigDir}, {Key: "CF_DATA_DIR", Value: old.DataDir}, {Key: "TMUX_TMPDIR", Value: "/run/tmux"}},
	}
	if _, err := svcunit.Install(t.Context(), o.Manager, spec); err != nil {
		t.Fatal(err)
	}
}

func TestInstallService_AWorkerOnTheSameRootIsMovedInTheSameRun(t *testing.T) {
	o, fleet, old := oldController(t)
	withWorker(t, o, old)
	fleet.Set("cf-worker.service", true, true)

	if _, err := daemon.InstallService(t.Context(), o); err != nil {
		t.Fatal(err)
	}

	if fleet.Active("cf-worker.service") || fleet.Active("cf-controller.service") || !fleet.Active("codeflock-worker.service") || !fleet.Active("codeflock-controller.service") {
		t.Errorf("worker %v, controller %v", fleet.Active("codeflock-worker.service"), fleet.Active("codeflock-controller.service"))
	}
	in, err := svcunit.Inspect(t.Context(), o.Manager, svcunit.Spec{Name: "codeflock-worker", Label: "com.codeflock.worker"})
	if err != nil || strings.Join(in.Argv[1:], " ") != "worker run --ssh-target me@host" || in.Env["TMUX_TMPDIR"] != "/run/tmux" {
		t.Errorf("the Worker unit: %+v %v", in, err)
	}
}

func TestInstallService_AWorkerWithLiveRunsStopsTheMoveOfTheController(t *testing.T) {
	o, fleet, old := oldController(t)
	withWorker(t, o, old)
	fleet.Set("cf-worker.service", true, true)
	o.Sessions = func(context.Context, tmux.Options) ([]string, error) { return []string{"cf-run_a-e1"}, nil }

	if _, err := daemon.InstallService(t.Context(), o); err == nil || !strings.Contains(err.Error(), "1 Runs are live") {
		t.Fatalf("err = %v", err)
	}
	if !fleet.Active("cf-controller.service") || !fleet.Active("cf-worker.service") {
		t.Error("an old service was stopped")
	}
	if _, err := os.Stat(old.DatabaseFile()); err != nil {
		t.Errorf("the data directory moved: %v", err)
	}
}
