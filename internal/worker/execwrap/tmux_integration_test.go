//go:build integration && (linux || darwin)

package execwrap_test

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	cfruntime "github.com/Autarquico/codefabric/internal/adapters/runtime"
	"github.com/Autarquico/codefabric/internal/adapters/runtime/tmux"
	"github.com/Autarquico/codefabric/internal/support/redact"
	"github.com/Autarquico/codefabric/internal/testutil/fakeharness"
	"github.com/Autarquico/codefabric/internal/worker/execwrap"
)

// newTmux is a Runtime with a tmux server of its own. Without tmux the test is
// skipped, unless CF_REQUIRE_TMUX=1.
func newTmux(t *testing.T) *tmux.Runtime {
	t.Helper()
	if _, err := exec.LookPath("tmux"); err != nil {
		if os.Getenv("CF_REQUIRE_TMUX") == "1" {
			t.Fatal("CF_REQUIRE_TMUX=1 and tmux is not installed")
		}
		t.Skip("tmux is not installed")
	}
	r, err := tmux.New(tmux.Options{Socket: tmux.SocketName(t.TempDir())})
	if err != nil {
		t.Fatal(err)
	}
	if err := r.Check(t.Context()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = r.KillServer(context.WithoutCancel(t.Context())) })
	return r
}

// asSelf is the argv that runs this test binary in a role, as the executor
// runs `cf worker exec` and `cf worker redact-pipe`.
func asSelf(role, dir, delivery string) []string {
	return []string{"/usr/bin/env", role + "=" + dir, withDelivery + "=" + delivery, os.Args[0]}
}

// session is the Spec of the session of the run in dir, as the executor makes
// it: in interactive mode, with the filter of the Worker as its capture.
func session(name, dir, mode, filterDelivery string) cfruntime.Spec {
	spec := cfruntime.Spec{Name: cfruntime.NameFor(name, 1), Dir: dir, Command: asSelf(asWrapper, dir, "1")}
	if mode == execwrap.ModeInteractive {
		spec.Capture, spec.Ready = filepath.Join(dir, execwrap.OutputLog), filepath.Join(dir, "pane.ready")
		spec.CaptureFilter, spec.CaptureAck = asSelf(asRedactPipe, dir, filterDelivery), filepath.Join(dir, execwrap.CaptureAckFile)
	}
	return spec
}

func waitExitIn(t *testing.T, dir string, bound time.Duration) execwrap.Exit {
	t.Helper()
	var exit execwrap.Exit
	for deadline := time.Now().Add(bound); time.Now().Before(deadline); time.Sleep(20 * time.Millisecond) {
		if ok, err := execwrap.Read(dir, execwrap.ExitFile, &exit); err != nil {
			t.Fatal(err)
		} else if ok {
			return exit
		}
	}
	log, _ := os.ReadFile(filepath.Join(dir, execwrap.OutputLog)) //nolint:gosec // a file of the test
	t.Fatalf("no exit.json; the log has %q", log)
	return exit
}

func TestScenario_SecretsLocal_RedactionUnderTmux(t *testing.T) {
	const multi = "first-line-secret\nsecond-line-secret"
	for _, mode := range []string{execwrap.ModeHeadless, execwrap.ModeInteractive} {
		t.Run(mode, func(t *testing.T) {
			r := newTmux(t)
			script := fakeScript(t,
				fakeharness.Step{EmitEnv: &fakeharness.EmitEnv{Name: "API_KEY", SplitAt: 9, PauseMS: 300}},
				fakeharness.Step{EmitEnv: &fakeharness.EmitEnv{Name: "PEM", SplitAt: 20, PauseMS: 300}},
			)
			dir := delivered(t, redact.Needle{Ref: apiRef, Value: []byte(apiValue)}, redact.Needle{Ref: "team/pem", Value: []byte(multi)})
			spec := session("run_sec", dir, mode, "1")
			l := fakeLaunch(script, mode)
			l.Dir, l.Gate = dir, spec.Ready
			l.SecretEnv = append(l.SecretEnv, execwrap.SecretEnv{Name: "PEM", Ref: "team/pem"})
			if err := execwrap.Write(dir, execwrap.LaunchFile, l); err != nil {
				t.Fatal(err)
			}
			if _, err := r.Start(t.Context(), spec); err != nil {
				t.Fatal(err)
			}
			exit := waitExitIn(t, dir, 30*time.Second)
			if exit.ExitCode == nil || *exit.ExitCode != 0 || exit.Error != "" {
				t.Fatalf("exit.json = %+v", exit)
			}
			want := []string{marker, "[REDACTED:team/pem]"}
			var log []byte
			for deadline := time.Now().Add(10 * time.Second); time.Now().Before(deadline); time.Sleep(20 * time.Millisecond) {
				// The filter of an interactive Run may still be draining the terminal.
				if log, _ = os.ReadFile(filepath.Join(dir, execwrap.OutputLog)); strings.Contains(string(log), want[1]) { //nolint:gosec // a file of the test
					break
				}
			}
			for _, w := range want {
				if !strings.Contains(string(log), w) {
					t.Errorf("the log lacks %s: %q", w, log)
				}
			}
			for _, leak := range []string{apiValue, apiValue[:9], "first-line-secret", "second-line-secret"} {
				if strings.Contains(string(log), leak) {
					t.Errorf("the log has %q: %q", leak, log)
				}
			}
			noValueIn(t, apiValue, dir)
			noValueIn(t, "first-line-secret", dir)
		})
	}
}

func TestScenario_SecretsLocal_AFilterThatCannotLoadKeepsTheHarnessFromRunning(t *testing.T) {
	r := newTmux(t)
	dir := delivered(t, redact.Needle{Ref: apiRef, Value: []byte(apiValue)})
	spec := session("run_nofilter", dir, execwrap.ModeInteractive, "broken")
	l := mustNotRun(execwrap.ModeInteractive)
	l.Dir, l.Gate = dir, spec.Ready
	if err := execwrap.Write(dir, execwrap.LaunchFile, l); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Start(t.Context(), spec); !errors.Is(err, cfruntime.ErrCaptureRefused) {
		t.Fatalf("Start = %v, want the refusal of the filter", err)
	}
	if _, err := os.Stat(spec.Ready); err == nil {
		t.Fatal("the gate is open")
	}
	log, _ := os.ReadFile(spec.Capture)
	if !strings.Contains(string(log), strings.TrimSpace(execwrap.CaptureDisabledNote)) {
		t.Errorf("the log is %q, want the note", log)
	}
	// The wrapper gives up at the closed gate, having run nothing.
	exit := waitExitIn(t, dir, execwrap.GateTimeout+15*time.Second)
	if exit.ExitCode != nil || !strings.Contains(exit.Error, "not ready") {
		t.Errorf("exit.json = %+v", exit)
	}
	for _, name := range []string{execwrap.StartedFile, "must-not-run"} {
		if _, err := os.Stat(filepath.Join(dir, name)); err == nil {
			t.Errorf("%s exists: the harness ran", name)
		}
	}
	noValueIn(t, apiValue, dir)
}
