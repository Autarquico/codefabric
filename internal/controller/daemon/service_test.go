package daemon_test

import (
	"errors"
	"os"
	"slices"
	"strings"
	"testing"

	"github.com/Autarquico/codefabric/internal/config"
	"github.com/Autarquico/codefabric/internal/controller/daemon"
	"github.com/Autarquico/codefabric/internal/domain"
	"github.com/Autarquico/codefabric/internal/support/svcunit"
	"github.com/Autarquico/codefabric/internal/support/svcunit/svcunittest"
)

func serviceOptions(t *testing.T) (daemon.ServiceOptions, *svcunittest.Systemd) {
	t.Helper()
	o := daemon.ServiceOptions{
		Paths: newPaths(t), Binary: "/usr/local/bin/cf", PATH: "/usr/bin:/bin",
		Manager: svcunit.Env{Home: t.TempDir(), UID: 1000, GOOS: "linux"},
	}
	path, _, _, err := daemon.RenderService(o)
	if err != nil {
		t.Fatal(err)
	}
	fake := svcunittest.NewSystemd(path)
	o.Manager.Runner = fake
	return o, fake
}

func TestInstallService_ControllerUnit(t *testing.T) {
	o, fake := serviceOptions(t)
	if _, err := daemon.InstallService(t.Context(), o); !errors.Is(err, config.ErrNotInitialized) {
		t.Fatalf("before init: %v", err)
	}
	if len(fake.Calls()) != 0 {
		t.Errorf("ran %q before init", fake.Calls())
	}
	initController(t, o.Paths)

	res, err := daemon.InstallService(t.Context(), o)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{
		"write " + fake.Path, "systemctl --user daemon-reload",
		"systemctl --user enable --no-reload cf-controller.service", "systemctl --user start cf-controller.service",
	}
	if !slices.Equal(res.Actions, want) {
		t.Errorf("actions = %q", res.Actions)
	}
	unit, err := os.ReadFile(fake.Path)
	if err != nil {
		t.Fatal(err)
	}
	// The Controller hosts no Run (invariant 3): systemd's KillMode takes what it started down with it.
	if !strings.Contains(string(unit), `ExecStart="/usr/local/bin/cf" "controller" "run"`) || strings.Contains(string(unit), "KillMode") {
		t.Errorf("unit:\n%s", unit)
	}

	if again, err := daemon.InstallService(t.Context(), o); err != nil || again.Changed {
		t.Errorf("installing again: %+v %v", again, err)
	}
	gone, err := daemon.UninstallService(t.Context(), o)
	if err != nil || !gone.Changed {
		t.Fatalf("uninstall: %+v %v", gone, err)
	}
	if _, err := os.Stat(fake.Path); !os.IsNotExist(err) {
		t.Errorf("the unit is still there: %v", err)
	}
}

func TestInstallService_ControllerOnlyOnLinux(t *testing.T) {
	o, _ := serviceOptions(t)
	initController(t, o.Paths)
	o.Manager.GOOS = "darwin"
	for name, err := range map[string]error{
		"install":   func() error { _, err := daemon.InstallService(t.Context(), o); return err }(),
		"uninstall": func() error { _, err := daemon.UninstallService(t.Context(), o); return err }(),
	} {
		if !errors.Is(err, &domain.Error{Code: domain.CodeNotImplemented}) {
			t.Errorf("%s: %v", name, err)
		}
	}
}

func TestInstallService_ControllerRefusesARelativePATH(t *testing.T) {
	o, _ := serviceOptions(t)
	initController(t, o.Paths)
	o.PATH = "bin"
	if _, err := daemon.InstallService(t.Context(), o); !errors.Is(err, &domain.Error{Code: domain.CodeInvalidRequest}) {
		t.Errorf("err = %v", err)
	}
}
