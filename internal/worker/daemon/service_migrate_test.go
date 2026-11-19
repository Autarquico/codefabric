package daemon_test

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/Autarquico/codeflock/internal/adapters/runtime/tmux"
	"github.com/Autarquico/codeflock/internal/config"
	"github.com/Autarquico/codeflock/internal/domain"
	"github.com/Autarquico/codeflock/internal/support/svcunit"
	"github.com/Autarquico/codeflock/internal/support/svcunit/svcunittest"
	"github.com/Autarquico/codeflock/internal/worker/daemon"
	"github.com/Autarquico/codeflock/internal/worker/state"
)

// oldWorker is a machine with a Worker service of the old name, running, and
// the directories it uses, with a database of the Worker, credentials and a
// file of the Controller on the same root.
type oldWorker struct {
	home  string
	fleet *svcunittest.SystemdFleet
	opts  daemon.ServiceOptions
	paths config.Paths // the old directories
}

func newOldWorker(t *testing.T) *oldWorker {
	t.Helper()
	home := t.TempDir()
	w := &oldWorker{home: home, fleet: svcunittest.NewSystemdFleet(home)}
	w.paths = config.Paths{ConfigDir: filepath.Join(home, ".config", "codefabric"), DataDir: filepath.Join(home, ".local", "share", "codefabric")}
	for path, content := range map[string]string{
		filepath.Join(w.paths.WorkerCredentialsDir(), "cert.pem"): "worker identity",
		filepath.Join(w.paths.DataDir, "controller.db"):           "controller database",
		filepath.Join(w.paths.PKIDir(), "ca.pem"):                 "pki",
		filepath.Join(w.paths.RelayDir(), "p.git", "HEAD"):        "relay",
	} {
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	db, err := state.Open(t.Context(), w.paths.WorkerStateFile())
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	env := svcunit.Env{Runner: w.fleet, Home: home, UID: 1000, GOOS: "linux"}
	old := svcunit.Spec{
		Name: "cf-worker", Label: "com.codefabric.worker", KeepProcesses: true, Installer: "cf worker install-service",
		Argv: []string{"/opt/cf/bin/cf", "worker", "run", "--ssh-target", "me@host", "--controller", "https://c:7443", "--extra"},
		Env: []svcunit.KV{
			{Key: "CF_CONFIG_DIR", Value: w.paths.ConfigDir},
			{Key: "CF_DATA_DIR", Value: w.paths.DataDir},
			{Key: "PATH", Value: "/old/bin:/usr/bin"},
			{Key: "TMUX_TMPDIR", Value: "/run/user/1000/tmux"},
			{Key: "FOO", Value: "bar"},
		},
	}
	if _, err := svcunit.Install(t.Context(), env, old); err != nil {
		t.Fatal(err)
	}
	w.fleet.Set("cf-worker.service", true, true)
	w.opts = daemon.ServiceOptions{
		// What config.Resolve gives with the old directories in use.
		Paths: w.paths, Binary: svcunittest.Program(t), Manager: env,
		Sessions: func(context.Context, tmux.Options) ([]string, error) { return nil, nil },
		Settle:   func(context.Context) error { return nil },
	}
	return w
}

func (w *oldWorker) unit(name string) string {
	return filepath.Join(w.home, ".config", "systemd", "user", name+".service")
}

// untouched says nothing changed: the old service runs, the files are where they were.
func (w *oldWorker) untouched(t *testing.T) {
	t.Helper()
	if _, err := os.Stat(w.unit("cf-worker")); err != nil || !w.fleet.Active("cf-worker.service") {
		t.Errorf("the old service: %v, active %v", err, w.fleet.Active("cf-worker.service"))
	}
	if _, err := os.Stat(w.unit("codeflock-worker")); err == nil {
		t.Errorf("the new service exists")
	}
	if b, err := os.ReadFile(filepath.Join(w.paths.DataDir, "controller.db")); err != nil || string(b) != "controller database" {
		t.Errorf("the data directory changed: %q %v", b, err)
	}
	if _, err := os.Stat(filepath.Join(w.home, ".local", "share", "codeflock")); err == nil {
		t.Errorf("a new directory exists")
	}
}

func (w *oldWorker) insertLiveRun(t *testing.T) {
	t.Helper()
	db, err := state.Open(t.Context(), w.paths.WorkerStateFile())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	err = db.WithTx(t.Context(), func(tx *state.Tx) error {
		_, err := state.InsertAssigned(t.Context(), tx, state.Run{
			Key: state.Key{RunID: "run_1", LeaseEpoch: 1}, LeaseID: "lease_1", RunShort: "r1", TaskID: "task_1", ProjectID: "proj_1",
			CommandSeq: 1, AcceptBy: time.Now().Add(time.Hour), CPU: 1, MemoryMB: 1, Spec: []byte(`{}`), Harness: "command", Mode: "headless", CreatedAt: time.Now(),
		})
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestScenario_RenameCLI_WorkerConServicioDeUsuario(t *testing.T) {
	w := newOldWorker(t)

	res, err := daemon.InstallService(t.Context(), w.opts)
	if err != nil {
		t.Fatal(err)
	}

	if !res.Changed || !w.fleet.Active("codeflock-worker.service") || w.fleet.Active("cf-worker.service") {
		t.Errorf("%+v", res)
	}
	if _, err := os.Stat(w.unit("cf-worker")); err == nil {
		t.Error("the old unit is still there")
	}
	newPaths := config.Paths{ConfigDir: filepath.Join(w.home, ".config", "codeflock"), DataDir: filepath.Join(w.home, ".local", "share", "codeflock")}
	for path, content := range map[string]string{
		filepath.Join(newPaths.WorkerCredentialsDir(), "cert.pem"): "worker identity",
		filepath.Join(newPaths.DataDir, "controller.db"):           "controller database",
		filepath.Join(newPaths.PKIDir(), "ca.pem"):                 "pki",
		filepath.Join(newPaths.RelayDir(), "p.git", "HEAD"):        "relay",
	} {
		if b, err := os.ReadFile(path); err != nil || string(b) != content { //nolint:gosec // a file of the test
			t.Errorf("%s: %q %v", path, b, err)
		}
	}
	if _, err := os.Stat(newPaths.WorkerStateFile()); err != nil {
		t.Errorf("the Worker database did not follow: %v", err)
	}
	// The new unit: CFLK_*, and what the old one had.
	in, err := svcunit.Inspect(t.Context(), w.opts.Manager, svcunit.Spec{Name: "codeflock-worker", Label: "com.codeflock.worker"})
	if err != nil || !in.Found {
		t.Fatalf("%+v %v", in, err)
	}
	for key, want := range map[string]string{
		"CFLK_CONFIG_DIR": newPaths.ConfigDir, "CFLK_DATA_DIR": newPaths.DataDir, "TMUX_TMPDIR": "/run/user/1000/tmux", "PATH": "/old/bin:/usr/bin", "FOO": "bar",
	} {
		if in.Env[key] != want {
			t.Errorf("%s = %q, want %q", key, in.Env[key], want)
		}
	}
	if _, old := in.Env["CF_DATA_DIR"]; old {
		t.Error("the new unit carries CF_DATA_DIR")
	}
	if got := strings.Join(in.Argv[1:], " "); got != "worker run --ssh-target me@host --controller https://c:7443 --extra" {
		t.Errorf("arguments: %s", got)
	}
}

func TestScenario_RenameCLI_WorkerConRunsVivos_CeroCambios(t *testing.T) {
	t.Run("in the database", func(t *testing.T) {
		w := newOldWorker(t)
		w.insertLiveRun(t)
		before, _ := os.ReadFile(w.paths.WorkerStateFile())

		_, err := daemon.InstallService(t.Context(), w.opts)

		var de *domain.Error
		if !errors.As(err, &de) || de.Code != domain.CodeWorkerBusy || !strings.Contains(err.Error(), "cflk worker drain") || !strings.Contains(err.Error(), "1 Runs") {
			t.Fatalf("err = %v", err)
		}
		w.untouched(t)
		if after, _ := os.ReadFile(w.paths.WorkerStateFile()); !bytes.Equal(before, after) {
			t.Error("the database changed")
		}
		if slices.ContainsFunc(w.fleet.Calls(), func(c string) bool { return strings.Contains(c, " stop ") }) {
			t.Errorf("the service was stopped: %v", w.fleet.Calls())
		}
	})
	t.Run("in the old tmux, asked at the socket of the old directory and the TMUX_TMPDIR of the unit", func(t *testing.T) {
		w := newOldWorker(t)
		var asked tmux.Options
		w.opts.Sessions = func(_ context.Context, o tmux.Options) ([]string, error) {
			asked = o
			return []string{"cf-run_a-e1", "cf-run_b-e1"}, nil
		}
		_, err := daemon.InstallService(t.Context(), w.opts)
		if err == nil || !strings.Contains(err.Error(), "2 in tmux") {
			t.Fatalf("err = %v", err)
		}
		w.untouched(t)
		if asked.Socket != tmux.SocketName(w.paths.WorkerDataDir()) || asked.TmpDir != "/run/user/1000/tmux" || !asked.OwnTmpDir {
			t.Errorf("asked %+v", asked)
		}
	})
	t.Run("what cannot be checked counts as Runs", func(t *testing.T) {
		w := newOldWorker(t)
		w.opts.Sessions = func(context.Context, tmux.Options) ([]string, error) { return nil, errors.New("tmux is broken") }
		if _, err := daemon.InstallService(t.Context(), w.opts); err == nil || !strings.Contains(err.Error(), "that counts as there being some") {
			t.Fatalf("tmux: %v", err)
		}
		w.untouched(t)

		w = newOldWorker(t)
		if err := os.WriteFile(w.paths.WorkerStateFile(), []byte("not a database"), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := daemon.InstallService(t.Context(), w.opts); err == nil || !strings.Contains(err.Error(), "that counts as there being some") {
			t.Fatalf("database: %v", err)
		}
		w.untouched(t)
	})
}

func TestInstallService_AFailureAfterTheMoveLeavesTheOldWorkerRunningAsItWas(t *testing.T) {
	w := newOldWorker(t)
	w.fleet.Fail("systemctl --user start codeflock-worker", errors.New("boom"))
	if _, err := daemon.InstallService(t.Context(), w.opts); err == nil || !strings.Contains(err.Error(), "boom") {
		t.Fatalf("err = %v", err)
	}
	w.untouched(t)
	if _, err := os.Stat(w.paths.WorkerStateFile()); err != nil {
		t.Errorf("the database is not back: %v", err)
	}
}

func TestInstallService_AnUnreadableOldUnitChangesNothing(t *testing.T) {
	w := newOldWorker(t)
	if err := os.Remove(w.unit("cf-worker")); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(w.unit("cf-worker"), 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := daemon.InstallService(t.Context(), w.opts); err == nil {
		t.Fatal("installed over an old unit it could not read")
	}
	if _, err := os.Stat(w.unit("codeflock-worker")); err == nil {
		t.Error("the new unit was written")
	}
}

func TestInstallService_ADirectoryOfTheOldNameWithNoOldServiceMovesBeforeTheNewOneRuns(t *testing.T) {
	w := newOldWorker(t)
	if err := os.Remove(w.unit("cf-worker")); err != nil {
		t.Fatal(err)
	}
	res, err := daemon.InstallService(t.Context(), w.opts)
	if err != nil {
		t.Fatal(err)
	}
	got, _, _ := svcunit.InstalledEnv(w.opts.Manager, svcunit.Spec{Name: "codeflock-worker"})
	if got["CFLK_DATA_DIR"] != filepath.Join(w.home, ".local", "share", "codeflock") || !res.Changed {
		t.Errorf("%v %+v", got, res)
	}
	if _, err := os.Stat(filepath.Join(w.home, ".local", "share", "codeflock", "worker", "state.db")); err != nil {
		t.Error(err)
	}
}

func TestScenario_RenameCLI_LaunchDaemon_ImprimeLosComandosYNoCambiaNada(t *testing.T) {
	daemons := t.TempDir()
	home := t.TempDir()
	plist := `<plist><dict><key>Label</key><string>com.codefabric.worker</string><key>UserName</key><string>ana</string>
<key>ProgramArguments</key><array><string>/usr/local/bin/cf</string><string>worker</string><string>run</string></array>
<key>EnvironmentVariables</key><dict><key>HOME</key><string>/Users/ana</string><key>CF_CONFIG_DIR</key><string>/Users/ana/.config/codefabric</string>
<key>CF_DATA_DIR</key><string>/Users/ana/.local/share/codefabric</string></dict></dict></plist>`
	path := filepath.Join(daemons, "com.codefabric.worker.plist")
	if err := os.WriteFile(path, []byte(plist), 0o600); err != nil {
		t.Fatal(err)
	}
	fake := svcunittest.NewLaunchd(filepath.Join(home, "Library", "LaunchAgents", "com.codeflock.worker.plist"), "com.codeflock.worker", 501)
	o := daemon.ServiceOptions{
		Paths: config.Paths{ConfigDir: filepath.Join(home, "c"), DataDir: filepath.Join(home, "d")}, Binary: svcunittest.Program(t),
		PlanBinary: func(binary string, _ bool) (string, error) { return binary, nil }, // a program of the test, not of root
		Manager:    svcunit.Env{Runner: fake, Home: home, UID: 501, GOOS: "darwin"}, LaunchDaemons: daemons,
	}

	_, err := daemon.InstallService(t.Context(), o)

	if err == nil {
		t.Fatal("a LaunchDaemon was handled without root")
	}
	for _, want := range []string{
		"sudo sh -eu <<'CFLK_PLAN'",
		"launchctl bootout system/'com.codefabric.worker'",
		"sudo -u 'ana' mv '/Users/ana/.local/share/codefabric' '/Users/ana/.local/share/codeflock'",
		"mv -n \"$tmp\" '" + filepath.Join(daemons, "com.codeflock.worker.plist") + "'",
		"launchctl bootstrap system",
		"# launchctl bootstrap system '" + path + "'", // the way back, commented out
	} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("missing %q in:\n%s", want, err)
		}
	}
	if len(fake.Calls()) != 0 {
		t.Errorf("launchctl was run: %v", fake.Calls())
	}
	if got, _ := os.ReadFile(path); string(got) != plist { //nolint:gosec // a file of the test
		t.Error("the old property list changed")
	}
	if _, err := os.Stat(filepath.Join(daemons, "com.codeflock.worker.plist")); err == nil {
		t.Error("a new property list was written")
	}
}

// withController is a Controller of the old name on the same root as the Worker.
func (w *oldWorker) withController(t *testing.T, active bool) {
	t.Helper()
	spec := svcunit.Spec{
		Name: "cf-controller", Label: "com.codefabric.controller", Installer: "cf controller install-service",
		Argv: []string{"/opt/cf/bin/cf", "controller", "run"},
		Env:  []svcunit.KV{{Key: "CF_CONFIG_DIR", Value: w.paths.ConfigDir}, {Key: "CF_DATA_DIR", Value: w.paths.DataDir}, {Key: "PATH", Value: "/usr/bin"}},
	}
	if _, err := svcunit.Install(t.Context(), w.opts.Manager, spec); err != nil {
		t.Fatal(err)
	}
	w.fleet.Set("cf-controller.service", true, active)
}

func TestInstallService_AControllerOnTheSameRootIsMovedInTheSameRun(t *testing.T) {
	w := newOldWorker(t)
	w.withController(t, true)

	if _, err := daemon.InstallService(t.Context(), w.opts); err != nil {
		t.Fatal(err)
	}

	for _, name := range []string{"cf-controller", "cf-worker"} {
		if _, err := os.Stat(w.unit(name)); err == nil {
			t.Errorf("%s is still there", name)
		}
	}
	if !w.fleet.Active("codeflock-controller.service") || !w.fleet.Active("codeflock-worker.service") {
		t.Errorf("not running: controller %v, worker %v", w.fleet.Active("codeflock-controller.service"), w.fleet.Active("codeflock-worker.service"))
	}
	got, _, _ := svcunit.InstalledEnv(w.opts.Manager, svcunit.Spec{Name: "codeflock-controller", Label: "com.codeflock.controller"})
	if got["CFLK_DATA_DIR"] != filepath.Join(w.home, ".local", "share", "codeflock") {
		t.Errorf("env = %v", got)
	}
}

func TestInstallService_ALiveRunStopsTheMoveOfTheWorkerAndOfTheControllerWithIt(t *testing.T) {
	w := newOldWorker(t)
	w.withController(t, true)
	w.insertLiveRun(t)

	if _, err := daemon.InstallService(t.Context(), w.opts); err == nil || !strings.Contains(err.Error(), "1 Runs are live") {
		t.Fatalf("err = %v", err)
	}
	w.untouched(t)
	if _, err := os.Stat(w.unit("cf-controller")); err != nil || !w.fleet.Active("cf-controller.service") {
		t.Errorf("the Controller was touched: %v", err)
	}
}

// With no old service the directories move all the same, so the request must
// be good, and the Worker idle, before anything does.
func TestInstallService_NothingMovesUnlessTheRequestAndTheWorkerAreGood(t *testing.T) {
	for _, tc := range []struct {
		name  string
		setup func(t *testing.T, w *oldWorker)
	}{
		{"a live Run", func(t *testing.T, w *oldWorker) { w.insertLiveRun(t) }},
		{"a bad flag", func(_ *testing.T, w *oldWorker) { w.opts.SSHTarget = "not a target" }},
		{"a machine that has not joined", func(t *testing.T, w *oldWorker) {
			if err := os.RemoveAll(w.paths.WorkerCredentialsDir()); err != nil {
				t.Fatal(err)
			}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := newOldWorker(t)
			if err := os.Remove(w.unit("cf-worker")); err != nil {
				t.Fatal(err)
			}
			tc.setup(t, w)
			if _, err := daemon.InstallService(t.Context(), w.opts); err == nil {
				t.Fatal("installed")
			}
			if b, err := os.ReadFile(filepath.Join(w.paths.DataDir, "controller.db")); err != nil || string(b) != "controller database" {
				t.Errorf("the data directory moved: %q %v", b, err)
			}
			if _, err := os.Stat(filepath.Join(w.home, ".local", "share", "codeflock")); err == nil {
				t.Error("a new directory exists")
			}
		})
	}
}

func TestInstallService_ALaunchDaemonPlanRunsThisCflkNotTheTransitionCf(t *testing.T) {
	daemons, home := t.TempDir(), t.TempDir()
	plist := `<plist><dict><key>Label</key><string>com.codefabric.worker</string>
<key>ProgramArguments</key><array><string>/usr/local/bin/cf</string><string>worker</string><string>run</string></array>
<key>EnvironmentVariables</key><dict><key>CF_CONFIG_DIR</key><string>/srv/c/codefabric</string><key>CF_DATA_DIR</key><string>/srv/d/codefabric</string></dict></dict></plist>`
	if err := os.WriteFile(filepath.Join(daemons, "com.codefabric.worker.plist"), []byte(plist), 0o600); err != nil {
		t.Fatal(err)
	}
	fake := svcunittest.NewLaunchd(filepath.Join(home, "Library", "LaunchAgents", "com.codeflock.worker.plist"), "com.codeflock.worker", 501)
	o := daemon.ServiceOptions{
		Paths:      config.Paths{ConfigDir: filepath.Join(home, "c"), DataDir: filepath.Join(home, "d")},
		PlanBinary: func(binary string, _ bool) (string, error) { return binary, nil }, // the test binary is not root's
		Manager:    svcunit.Env{Runner: fake, Home: home, UID: 501, GOOS: "darwin"}, LaunchDaemons: daemons,
	}
	o.Binary, _ = os.Executable()
	_, err := daemon.InstallService(t.Context(), o)
	exe, _ := os.Executable()
	if err == nil || !strings.Contains(err.Error(), "<string>"+exe+"</string>") || strings.Contains(err.Error(), "/usr/local/bin/cf<") {
		t.Fatalf("err = %v", err)
	}
}

// A LaunchDaemon runs as root: a cflk that its caller owns is refused, and the
// message says where to put it.
func TestInstallService_ALaunchDaemonPlanRefusesACflkThatNotOnlyRootCanReplace(t *testing.T) {
	daemons, home := t.TempDir(), t.TempDir()
	plist := `<plist><dict><key>Label</key><string>com.codefabric.worker</string>
<key>ProgramArguments</key><array><string>/usr/local/bin/cf</string><string>worker</string><string>run</string></array>
<key>EnvironmentVariables</key><dict><key>CF_CONFIG_DIR</key><string>/srv/c/codefabric</string><key>CF_DATA_DIR</key><string>/srv/d/codefabric</string></dict></dict></plist>`
	if err := os.WriteFile(filepath.Join(daemons, "com.codefabric.worker.plist"), []byte(plist), 0o600); err != nil {
		t.Fatal(err)
	}
	fake := svcunittest.NewLaunchd(filepath.Join(home, "Library", "LaunchAgents", "com.codeflock.worker.plist"), "com.codeflock.worker", 501)
	o := daemon.ServiceOptions{
		Paths: config.Paths{ConfigDir: filepath.Join(home, "c"), DataDir: filepath.Join(home, "d")}, Binary: svcunittest.Program(t),
		Manager: svcunit.Env{Runner: fake, Home: home, UID: 501, GOOS: "darwin"}, LaunchDaemons: daemons,
	}
	_, err := daemon.InstallService(t.Context(), o)
	if os.Getuid() == 0 {
		t.Skip("root owns what it creates")
	}
	if err == nil || !strings.Contains(err.Error(), "/usr/local/bin") || !strings.Contains(err.Error(), "--binary") || strings.Contains(err.Error(), "<<'CFLK_PLAN'") {
		t.Fatalf("err = %v", err)
	}
	if len(fake.Calls()) != 0 {
		t.Errorf("launchctl was run: %v", fake.Calls())
	}
}
