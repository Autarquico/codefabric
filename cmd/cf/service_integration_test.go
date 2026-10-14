//go:build integration && (linux || darwin)

package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
	"strings"
	"testing"

	cfruntime "github.com/Autarquico/codefabric/internal/adapters/runtime"
	"github.com/Autarquico/codefabric/internal/adapters/runtime/tmux"
	"github.com/Autarquico/codefabric/internal/config"
	"github.com/Autarquico/codefabric/internal/controller/daemon"
	"github.com/Autarquico/codefabric/internal/domain"
	"github.com/Autarquico/codefabric/internal/support/svcunit"
	workerdaemon "github.com/Autarquico/codefabric/internal/worker/daemon"
)

// realManager is the service manager of the user that runs the tests. These
// tests install services of that user, so they run only when asked: with
// CF_REQUIRE_SYSTEMD=1 on Linux, CF_REQUIRE_LAUNCHD=1 on macOS. Asked, they
// fail if the manager does not work. Every service has a name of its own and
// goes when the test ends, whatever happens.
type realManager struct {
	env svcunit.Env
	// restart is the When of the scenario: the manager restarts the daemon.
	restart func(t *testing.T, unit string)
	// mainPID is the process of the daemon and since when it is there.
	mainPID func(t *testing.T, unit string) string
	// logs is what the daemon said.
	logs func(t *testing.T, res workerdaemon.ServiceResult) string
}

func requireSystemd(t *testing.T) realManager {
	t.Helper()
	requireManager(t, "linux", "CF_REQUIRE_SYSTEMD")
	if out, err := exec.CommandContext(t.Context(), "systemctl", "--user", "show-environment").CombinedOutput(); err != nil {
		t.Fatalf("CF_REQUIRE_SYSTEMD=1 and the user's systemd does not answer: %v\n%s", err, out)
	}
	env, err := svcunit.HostEnv()
	if err != nil {
		t.Fatal(err)
	}
	systemctl := func(t *testing.T, args ...string) string {
		t.Helper()
		out, err := exec.CommandContext(context.WithoutCancel(t.Context()), "systemctl", append([]string{"--user"}, args...)...).CombinedOutput() //nolint:gosec // systemctl with arguments of the test
		if err != nil {
			t.Fatalf("systemctl %v: %v\n%s", args, err, out)
		}
		return strings.TrimSpace(string(out))
	}
	return realManager{
		env:     env,
		restart: func(t *testing.T, unit string) { systemctl(t, "restart", unit) },
		mainPID: func(t *testing.T, unit string) string {
			return systemctl(t, "show", "-p", "MainPID", "-p", "ActiveEnterTimestamp", unit)
		},
		logs: func(t *testing.T, res workerdaemon.ServiceResult) string {
			out, _ := exec.CommandContext(context.WithoutCancel(t.Context()), "journalctl", "--user", "-u", res.Unit, "--no-pager", "-n", "200").CombinedOutput() //nolint:gosec // journalctl with arguments of the test
			return string(out)
		},
	}
}

var launchdPID = regexp.MustCompile(`(?m)^\tpid = (\d+)$`)

func requireLaunchd(t *testing.T) realManager {
	t.Helper()
	requireManager(t, "darwin", "CF_REQUIRE_LAUNCHD")
	env, err := svcunit.HostEnv()
	if err != nil {
		t.Fatal(err)
	}
	// An agent can be loaded from anywhere: its plist and its log stay in the test.
	env.Home = shortTempDir(t)
	launchctl := func(t *testing.T, args ...string) string {
		t.Helper()
		out, err := exec.CommandContext(context.WithoutCancel(t.Context()), "launchctl", args...).CombinedOutput() //nolint:gosec // launchctl with arguments of the test
		if err != nil {
			t.Fatalf("launchctl %v: %v\n%s", args, err, out)
		}
		return string(out)
	}
	return realManager{
		env:     env,
		restart: func(t *testing.T, unit string) { launchctl(t, "kickstart", "-k", unit) },
		mainPID: func(t *testing.T, unit string) string {
			if m := launchdPID.FindStringSubmatch(launchctl(t, "print", unit)); m != nil {
				return m[1]
			}
			return ""
		},
		logs: func(_ *testing.T, res workerdaemon.ServiceResult) string {
			label := res.Unit[strings.LastIndex(res.Unit, "/")+1:]
			out, _ := os.ReadFile(filepath.Join(env.Home, "Library", "Logs", "codefabric", label+".log")) //nolint:gosec // the log of the test's agent
			return string(out)
		},
	}
}

func requireManager(t *testing.T, goos, variable string) {
	t.Helper()
	if runtime.GOOS != goos {
		t.Skipf("the service manager of %s", goos)
	}
	if os.Getenv(variable) != "1" {
		t.Skipf("installs a service of this user: %s=1 runs it", variable)
	}
}

// shortTempDir is a temporary directory under /tmp: the paths under it stay
// short, and it goes with the test.
func shortTempDir(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("/tmp", "cfsvc-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	return dir
}

func randomSuffix(t *testing.T) string {
	t.Helper()
	b := make([]byte, 4)
	if _, err := rand.Read(b); err != nil {
		t.Fatal(err)
	}
	return "-test-" + hex.EncodeToString(b)
}

// installed installs the Worker of b as a service of m, under a name of its
// own, and removes it when the test ends.
func installed(t *testing.T, m realManager, b *reconcileBox) (workerdaemon.ServiceOptions, workerdaemon.ServiceResult) {
	t.Helper()
	o := workerdaemon.ServiceOptions{
		Paths: b.box, Manager: m.env, Suffix: randomSuffix(t),
		ExtraEnv: []svcunit.KV{
			{Key: asCF, Value: "1"},
			{Key: "GORACE", Value: "atexit_sleep_ms=0"},
			{Key: "GIT_CONFIG_GLOBAL", Value: "/dev/null"},
			{Key: "GIT_CONFIG_NOSYSTEM", Value: "1"},
		},
		ExtraArgs: []string{"--heartbeat-interval", heartbeatEvery},
	}
	res, err := workerdaemon.InstallService(t.Context(), o)
	t.Cleanup(func() {
		if t.Failed() {
			t.Logf("--- the service said:\n%s", m.logs(t, res))
		}
		ctx := context.WithoutCancel(t.Context())
		if _, err := workerdaemon.UninstallService(ctx, workerdaemon.ServiceOptions{Manager: m.env, Suffix: o.Suffix}); err != nil {
			t.Errorf("removing the service of the test: %v", err)
		}
	})
	if err != nil {
		t.Fatal(err)
	}
	if !res.Changed {
		t.Fatalf("installing changed nothing: %+v", res)
	}
	return o, res
}

func waitOnline(t *testing.T, b *reconcileBox, not string) string {
	t.Helper()
	var epoch string
	eventually(t, "the Worker of the service to be online", func() bool {
		w, err := b.admin.Worker(t.Context(), "alpha")
		epoch = w.Epoch
		return err == nil && w.State == domain.WorkerOnline && len(w.Capabilities) == 1 && w.Epoch != not
	})
	return epoch
}

func sessions(t *testing.T, box config.Paths) []string {
	t.Helper()
	rt, err := tmux.New(tmux.Options{Socket: tmux.SocketName(box.WorkerDataDir())})
	if err != nil {
		t.Fatal(err)
	}
	names, err := rt.List(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	return names
}

// serviceScenario is the scenario of a manager: the Worker runs as its
// service, a Run is going, the manager restarts the daemon, and the session of
// the Run survives; the new daemon adopts it and publishes its result.
// Installing again changes nothing, and uninstalling with a Run going leaves it
// running and says so.
func serviceScenario(t *testing.T, m realManager) {
	host, _, _ := initController(t)
	serve(t, host, daemon.Options{})
	b := newReconcileBox(t, host)
	newOrigin(t).clone(t)
	smallProject(t)
	o, res := installed(t, m, b)
	if runtime.GOOS == "darwin" && !strings.HasPrefix(res.Unit, "gui/") {
		t.Fatalf("the agent went into %s, not the graphical session", res.Unit)
	}
	epoch := waitOnline(t, b, "")

	created := b.running(waitScript)
	run := created.Run.ID
	session := cfruntime.NameFor(run, 1)
	pid := harnessPID(t, b.box, run)

	m.restart(t, res.Unit)
	waitOnline(t, b, epoch)
	if !slices.Contains(sessions(t, b.box), session) {
		t.Fatalf("the session %s did not survive the restart: %v", session, sessions(t, b.box))
	}
	if !processAlive(pid) {
		t.Fatal("the harness died with the restart")
	}
	if r := b.run(run); r.State != domain.RunRunning || r.Runtime != domain.RuntimeActive {
		t.Fatalf("after the restart: %+v", r)
	}
	b.release(run)
	if got := b.waitPublished(run); got.State != domain.RunCompleted {
		t.Errorf("Run = %+v", got)
	}

	t.Run("Idempotencia", func(t *testing.T) {
		before := m.mainPID(t, res.Unit)
		again, err := workerdaemon.InstallService(t.Context(), o)
		if err != nil || again.Changed {
			t.Fatalf("installing again: %+v %v", again, err)
		}
		if after := m.mainPID(t, res.Unit); after != before || after == "" {
			t.Errorf("the daemon was touched: %q, then %q", before, after)
		}
	})

	t.Run("Uninstall", func(t *testing.T) {
		second := b.running(waitScript)
		pid := harnessPID(t, b.box, second.Run.ID)
		gone, err := workerdaemon.UninstallService(t.Context(), workerdaemon.ServiceOptions{Manager: m.env, Suffix: o.Suffix})
		if err != nil {
			t.Fatal(err)
		}
		want := cfruntime.NameFor(second.Run.ID, 1)
		if !slices.Equal(gone.ActiveRuns, []string{want}) || len(gone.Warnings) != 1 || !strings.Contains(gone.Warnings[0], want) {
			t.Errorf("uninstall: %+v", gone)
		}
		if !slices.Contains(sessions(t, b.box), want) || !processAlive(pid) {
			t.Error("uninstalling ended the Run")
		}
		if _, err := os.Stat(gone.Path); !os.IsNotExist(err) {
			t.Errorf("the file is still there: %v", err)
		}
	})
}

// TestScenario_ServiceUnits_Systemd: `systemctl --user restart` of the Worker's
// unit leaves the tmux session of a Run alive.
func TestScenario_ServiceUnits_Systemd(t *testing.T) {
	serviceScenario(t, requireSystemd(t))
}

// TestScenario_ServiceUnits_Launchd: `launchctl kickstart -k` of the Worker's
// agent leaves the tmux session of a Run alive.
func TestScenario_ServiceUnits_Launchd(t *testing.T) {
	serviceScenario(t, requireLaunchd(t))
}

// The manager hands the daemon the arguments and the environment of the file
// as they were given, spaces and '%' included.
func TestServiceUnits_TheDaemonGetsItsArgvAndEnvironment(t *testing.T) {
	var m realManager
	if runtime.GOOS == "linux" {
		m = requireSystemd(t)
	} else {
		m = requireLaunchd(t)
	}
	root := shortTempDir(t)
	box := config.Paths{ConfigDir: filepath.Join(root, "my config 100%"), DataDir: filepath.Join(root, "data $HOME %h")}
	if err := os.MkdirAll(box.WorkerCredentialsDir(), 0o700); err != nil {
		t.Fatal(err)
	}
	out := filepath.Join(root, "echoed.json")
	o := workerdaemon.ServiceOptions{
		Paths: box, Manager: m.env, Suffix: randomSuffix(t), SSHTarget: "me@box",
		PATH: "/usr/bin:/bin:" + filepath.Join(root, "a dir"), TmuxTmpDir: filepath.Join(root, "tmux %t"),
		ExtraEnv: []svcunit.KV{{Key: echoTo, Value: out}},
	}
	res, err := workerdaemon.InstallService(t.Context(), o)
	t.Cleanup(func() {
		if _, err := workerdaemon.UninstallService(context.WithoutCancel(t.Context()), workerdaemon.ServiceOptions{Manager: m.env, Suffix: o.Suffix}); err != nil {
			t.Errorf("removing the service of the test: %v", err)
		}
	})
	if err != nil {
		t.Fatal(err)
	}
	eventually(t, "the service to say what it got", func() bool { return fileExists(out) })
	raw, err := os.ReadFile(out) //nolint:gosec // a file of the test
	if err != nil {
		t.Fatal(err)
	}
	var got echoed
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("%v: %s", err, raw)
	}
	wantArgs := []string{"worker", "run", "--ssh-target", "me@box"}
	wantEnv := map[string]string{"CF_CONFIG_DIR": box.ConfigDir, "CF_DATA_DIR": box.DataDir, "PATH": o.PATH, "TMUX_TMPDIR": o.TmuxTmpDir}
	if !slices.Equal(got.Args, wantArgs) || fmt.Sprint(got.Env) != fmt.Sprint(wantEnv) {
		t.Errorf("the daemon got %+v\nwant args %q and env %q\n(%s)", got, wantArgs, wantEnv, m.logs(t, res))
	}
}
