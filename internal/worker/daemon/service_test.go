package daemon_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/Autarquico/codefabric/internal/adapters/runtime/tmux"
	"github.com/Autarquico/codefabric/internal/config"
	"github.com/Autarquico/codefabric/internal/domain"
	"github.com/Autarquico/codefabric/internal/support/svcunit"
	"github.com/Autarquico/codefabric/internal/support/svcunit/svcunittest"
	"github.com/Autarquico/codefabric/internal/worker/daemon"
)

// fakeManager is the service manager of goos, in memory, for a user whose home is a fresh directory.
type fakeManager interface {
	svcunit.Runner
	Calls() []string
	Fail(prefix string, err error)
}

// serviceBox is a joined Worker machine with a fake service manager.
func serviceBox(t *testing.T, goos string) (daemon.ServiceOptions, fakeManager) {
	t.Helper()
	paths := config.Paths{ConfigDir: filepath.Join(t.TempDir(), "my config"), DataDir: filepath.Join(t.TempDir(), "data 100%")}
	if err := os.MkdirAll(paths.WorkerCredentialsDir(), 0o700); err != nil {
		t.Fatal(err)
	}
	o := daemon.ServiceOptions{
		Paths: paths, Binary: svcunittest.Program(t), PATH: "/opt/cf/bin:/usr/bin:/bin",
		Manager: svcunit.Env{Home: t.TempDir(), UID: 501, GOOS: goos},
	}
	path, _, _, err := daemon.RenderService(o)
	if err != nil {
		t.Fatal(err)
	}
	var fake fakeManager
	if goos == "linux" {
		fake = svcunittest.NewSystemd(path)
	} else {
		fake = svcunittest.NewLaunchd(path, "com.codefabric.worker", 501)
	}
	o.Manager.Runner = fake
	return o, fake
}

func installService(t *testing.T, o daemon.ServiceOptions) daemon.ServiceResult {
	t.Helper()
	res, err := daemon.InstallService(t.Context(), o)
	if err != nil {
		t.Fatal(err)
	}
	return res
}

// mutating are the commands that change something; the rest only read.
func mutating(calls []string) []string {
	var out []string
	for _, c := range calls {
		if !strings.Contains(c, " show") && !strings.HasPrefix(c, "launchctl print") {
			out = append(out, c)
		}
	}
	return out
}

// kills are the commands that could end what the Worker started.
func kills(calls []string) []string {
	var out []string
	for _, c := range calls {
		if strings.Contains(c, "kill") || strings.Contains(c, "kickstart -k") {
			out = append(out, c)
		}
	}
	return out
}

// TestScenario_ServiceUnits_Idempotencia: installing what is installed and
// running only reads, writes nothing and says nothing changed.
func TestScenario_ServiceUnits_Idempotencia(t *testing.T) {
	for _, goos := range []string{"linux", "darwin"} {
		t.Run(goos, func(t *testing.T) {
			o, fake := serviceBox(t, goos)
			if first := installService(t, o); !first.Changed {
				t.Fatalf("the first install changed nothing: %+v", first)
			}
			before, err := os.Stat(servicePath(t, o))
			if err != nil {
				t.Fatal(err)
			}
			n := len(fake.Calls())

			again := installService(t, o)
			if again.Changed || len(again.Actions) != 0 {
				t.Errorf("the second install: %+v", again)
			}
			if m := mutating(fake.Calls()[n:]); len(m) != 0 {
				t.Errorf("the second install ran %q", m)
			}
			after, err := os.Stat(again.Path)
			if err != nil || !os.SameFile(before, after) || !after.ModTime().Equal(before.ModTime()) {
				t.Error("the file was rewritten")
			}
		})
	}
}

func servicePath(t *testing.T, o daemon.ServiceOptions) string {
	t.Helper()
	path, _, _, err := daemon.RenderService(o)
	if err != nil {
		t.Fatal(err)
	}
	return path
}

// TestScenario_ServiceUnits_Uninstall: with Runs in tmux, uninstalling stops
// the service, kills nothing, removes the file and names the Runs that go on.
func TestScenario_ServiceUnits_Uninstall(t *testing.T) {
	for _, goos := range []string{"linux", "darwin"} {
		t.Run(goos, func(t *testing.T) {
			o, fake := serviceBox(t, goos)
			o.TmuxTmpDir = "/tmp/elsewhere"
			installService(t, o)
			runs := []string{"cf-run_a-e1", "cf-run_b-e2"}
			var asked tmux.Options
			// Uninstall reads the identity of the installed service, not that of the shell.
			un := daemon.ServiceOptions{Manager: o.Manager, Sessions: func(_ context.Context, server tmux.Options) ([]string, error) {
				asked = server
				return runs, nil
			}}
			n := len(fake.Calls())

			res, err := daemon.UninstallService(t.Context(), un)
			if err != nil {
				t.Fatal(err)
			}
			want := tmux.Options{Socket: tmux.SocketName(o.Paths.WorkerDataDir()), TmpDir: "/tmp/elsewhere", OwnTmpDir: true}
			if asked != want {
				t.Errorf("the Runs were listed in %+v", asked)
			}
			if k := kills(fake.Calls()[n:]); len(k) != 0 {
				t.Errorf("uninstall ran %q", k)
			}
			if !slices.Equal(res.ActiveRuns, runs) || len(res.Warnings) != 1 ||
				!strings.Contains(res.Warnings[0], "2 Runs (cf-run_a-e1, cf-run_b-e2) go on in tmux") {
				t.Errorf("result = %+v", res)
			}
			if _, err := os.Stat(res.Path); !os.IsNotExist(err) {
				t.Errorf("the file is still there: %v", err)
			}
		})
	}
}

// A unit without TMUX_TMPDIR has its tmux server in tmux's default directory:
// the Runs are listed there, not where this shell's TMUX_TMPDIR points.
func TestUninstallService_AUnitWithoutTmuxTmpDirIgnoresTheCallers(t *testing.T) {
	o, _ := serviceBox(t, "linux")
	installService(t, o)
	t.Setenv("TMUX_TMPDIR", "/tmp/the-callers")
	var asked tmux.Options
	o.Sessions = func(_ context.Context, server tmux.Options) ([]string, error) {
		asked = server
		return nil, nil
	}
	if _, err := daemon.UninstallService(t.Context(), o); err != nil {
		t.Fatal(err)
	}
	if want := (tmux.Options{Socket: tmux.SocketName(o.Paths.WorkerDataDir()), OwnTmpDir: true}); asked != want {
		t.Errorf("the Runs were listed in %+v, want %+v", asked, want)
	}
}

func TestUninstallService_RunsThatCannotBeListedAreUnknownNotNone(t *testing.T) {
	o, _ := serviceBox(t, "linux")
	installService(t, o)
	o.Sessions = func(context.Context, tmux.Options) ([]string, error) { return nil, errors.New("no tmux") }
	res, err := daemon.UninstallService(t.Context(), o)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.ActiveRuns) != 0 || len(res.Warnings) != 1 || !strings.Contains(res.Warnings[0], "could not list the Runs") {
		t.Errorf("result = %+v", res)
	}
}

// With the tmux of the PATH, which exits 1 both when there is no server and
// when it cannot tell: only the first means there are no Runs.
func TestUninstallService_ATmuxThatCannotListLeavesTheRunsUnknown(t *testing.T) {
	for stderr, unknown := range map[string]bool{
		"no server running on /tmp/tmux-501/cf-abc":      false,
		"directory /tmp/tmux-501 has unsafe permissions": true,
	} {
		t.Run(stderr, func(t *testing.T) {
			o, _ := serviceBox(t, "linux")
			installService(t, o)
			bin := t.TempDir()
			script := "#!/bin/sh\necho '" + stderr + "' >&2\nexit 1\n"
			if err := os.WriteFile(filepath.Join(bin, "tmux"), []byte(script), 0o700); err != nil { //nolint:gosec // a fake tmux
				t.Fatal(err)
			}
			t.Setenv("PATH", bin)
			res, err := daemon.UninstallService(t.Context(), o)
			if err != nil {
				t.Fatal(err)
			}
			warned := len(res.Warnings) == 1 && strings.Contains(res.Warnings[0], "could not list the Runs") && strings.Contains(res.Warnings[0], stderr)
			if len(res.ActiveRuns) != 0 || warned != unknown || !unknown && len(res.Warnings) != 0 {
				t.Errorf("result = %+v", res)
			}
		})
	}
}

func TestUninstallService_NothingInstalledIsNotAnError(t *testing.T) {
	o, fake := serviceBox(t, "darwin")
	res, err := daemon.UninstallService(t.Context(), o)
	if err != nil || res.Changed || len(res.Warnings) != 0 {
		t.Errorf("%+v %v", res, err)
	}
	if m := mutating(fake.Calls()); len(m) != 0 {
		t.Errorf("ran %q", m)
	}
}

func TestUninstallService_PropagatesTheFailureAndKeepsTheFile(t *testing.T) {
	o, fake := serviceBox(t, "linux")
	res := installService(t, o)
	o.Sessions = func(context.Context, tmux.Options) ([]string, error) { return nil, nil }
	boom := errors.New("Failed to connect to bus")
	fake.Fail("systemctl --user stop", boom)
	if _, err := daemon.UninstallService(t.Context(), o); !errors.Is(err, boom) {
		t.Errorf("err = %v", err)
	}
	if _, err := os.Stat(res.Path); err != nil {
		t.Errorf("the file went: %v", err)
	}
}

func TestInstallService_NeedsAJoinedMachine(t *testing.T) {
	o, fake := serviceBox(t, "linux")
	if err := os.RemoveAll(o.Paths.WorkerCredentialsDir()); err != nil {
		t.Fatal(err)
	}
	_, err := daemon.InstallService(t.Context(), o)
	if !errors.Is(err, &domain.Error{Code: domain.CodeUnauthenticated}) {
		t.Errorf("err = %v", err)
	}
	if len(fake.Calls()) != 0 {
		t.Errorf("ran %q", fake.Calls())
	}
}

func TestInstallService_RefusesWhatTheServiceCouldNotUse(t *testing.T) {
	for name, mutate := range map[string]func(*daemon.ServiceOptions){
		"relative binary":      func(o *daemon.ServiceOptions) { o.Binary = "cf" },
		"relative PATH":        func(o *daemon.ServiceOptions) { o.PATH = "bin:/usr/bin" },
		"empty PATH entry":     func(o *daemon.ServiceOptions) { o.PATH = "/usr/bin::/bin" },
		"invalid ssh target":   func(o *daemon.ServiceOptions) { o.SSHTarget = "a b" },
		"relative TMUX_TMPDIR": func(o *daemon.ServiceOptions) { o.TmuxTmpDir = "tmux" },
		"binary others can replace": func(o *daemon.ServiceOptions) {
			dir := filepath.Join(t.TempDir(), "open")
			if err := os.Mkdir(dir, 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.Chmod(dir, 0o777); err != nil { //nolint:gosec // the unsafe directory the test is about
				t.Fatal(err)
			}
			o.Binary = filepath.Join(dir, "cf")
		},
	} {
		t.Run(name, func(t *testing.T) {
			o, _ := serviceBox(t, "linux")
			mutate(&o)
			if _, err := daemon.InstallService(t.Context(), o); !errors.Is(err, &domain.Error{Code: domain.CodeInvalidRequest}) {
				t.Errorf("err = %v", err)
			}
		})
	}
}

func TestInstallService_UnsupportedOS(t *testing.T) {
	o, _ := serviceBox(t, "linux")
	o.Manager.GOOS = "windows"
	if _, err := daemon.InstallService(t.Context(), o); !errors.Is(err, &domain.Error{Code: domain.CodeNotImplemented}) {
		t.Errorf("err = %v", err)
	}
}

func TestInstallService_WarnsWhenTheTmuxServerMoves(t *testing.T) {
	for name, move := range map[string]func(*daemon.ServiceOptions) string{
		"data directory": func(o *daemon.ServiceOptions) string {
			old := o.Paths.DataDir
			o.Paths.DataDir = filepath.Join(t.TempDir(), "new")
			return old
		},
		"TMUX_TMPDIR": func(o *daemon.ServiceOptions) string {
			o.TmuxTmpDir = filepath.Join(t.TempDir(), "elsewhere")
			return "TMUX_TMPDIR"
		},
	} {
		t.Run(name, func(t *testing.T) {
			o, _ := serviceBox(t, "linux")
			installService(t, o)
			want := move(&o)
			res := installService(t, o)
			// Besides the one of a program in a temporary directory.
			moved := slices.DeleteFunc(slices.Clone(res.Warnings), func(w string) bool { return !strings.Contains(w, "not adopted") })
			if !res.Changed || len(moved) != 1 || !strings.Contains(moved[0], want) {
				t.Errorf("result = %+v", res)
			}
		})
	}
}

// The unit and the agent pass what `cf worker run` needs, and keep the Runs
// alive across a restart (TestScenario_ServiceUnits_Systemd and _Launchd run
// them for real).
func TestInstallService_TheFileRunsTheWorkerAndKeepsItsRuns(t *testing.T) {
	for goos, keeps := range map[string]string{"linux": "\nKillMode=process\n", "darwin": "<key>AbandonProcessGroup</key>\n\t<true/>"} {
		o, _ := serviceBox(t, goos)
		o.SSHTarget, o.Controller = "me@box", "ctl:7443"
		_, content, _, err := daemon.RenderService(o)
		if err != nil {
			t.Fatal(err)
		}
		for _, want := range []string{keeps, o.Binary, "--ssh-target", "me@box", "--controller", "ctl:7443", o.Paths.ConfigDir, "data 100"} {
			if !strings.Contains(string(content), want) {
				t.Errorf("%s: no %q in\n%s", goos, want, content)
			}
		}
	}
}
