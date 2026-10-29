//go:build linux || darwin

package execwrap_test

import (
	"bytes"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	cfruntime "github.com/Autarquico/codefabric/internal/adapters/runtime"
	"github.com/Autarquico/codefabric/internal/support/redact"
	"github.com/Autarquico/codefabric/internal/testutil/fakeharness"
	"github.com/Autarquico/codefabric/internal/worker/execwrap"
	"github.com/Autarquico/codefabric/internal/worker/secretjit"
)

const (
	apiRef   = "team/api-key"
	apiValue = "s3cr3t-sentinel-4b1d" //nolint:gosec // a sentinel, not a credential
	marker   = "[REDACTED:" + apiRef + "]"
)

// delivered is a run directory with secrets delivered to it, as the executor
// leaves it before the session starts.
func delivered(t *testing.T, secrets ...redact.Needle) string {
	t.Helper()
	dir := t.TempDir()
	if err := secretjit.New().Deliver(dir, 1, secrets, func() error { return nil }); err != nil {
		t.Fatal(err)
	}
	return dir
}

// encodings are the forms of value a sweep looks for: as it is, and encoded.
func encodings(value string) []string {
	return []string{value, base64.StdEncoding.EncodeToString([]byte(value)), hex.EncodeToString([]byte(value))}
}

// noValueIn fails t if any file under the dirs, but the delivery itself, holds
// value in any of its encodings: launch.json, the log, the markers, and the
// temporaries any of them left.
func noValueIn(t *testing.T, value string, dirs ...string) {
	t.Helper()
	for _, dir := range dirs {
		err := filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.IsDir() && d.Name() == "secrets" && filepath.Dir(path) == dir {
				return filepath.SkipDir // where the value lives until it is released
			}
			if !d.Type().IsRegular() {
				return nil
			}
			raw, err := os.ReadFile(path) //nolint:gosec // a file of the test
			if err != nil {
				return err
			}
			for _, enc := range encodings(value) {
				if bytes.Contains(raw, []byte(enc)) {
					t.Errorf("%s holds the secret (%q): %q", path, enc, raw)
				}
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
}

// fakeScript is the script of a fakeharness, in a file of the test.
func fakeScript(t *testing.T, steps ...fakeharness.Step) string {
	t.Helper()
	raw, err := json.Marshal(fakeharness.Script{Steps: steps})
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "script.json")
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// fakeLaunch runs the fakeharness with the script, and API_KEY as a secret.
func fakeLaunch(script, mode string) execwrap.Launch {
	return execwrap.Launch{
		Argv: []string{os.Args[0], "--script", script}, Env: []string{fakeharness.AsFakeharnessEnv + "=1"},
		SecretEnv: []execwrap.SecretEnv{{Name: "API_KEY", Ref: apiRef}}, Mode: mode, Grace: time.Second,
	}
}

func TestScenario_SecretsLocal_HeadlessOutputIsRedactedEvenInTwoGoes(t *testing.T) {
	tmp := t.TempDir()
	release := filepath.Join(t.TempDir(), "release")
	script := fakeScript(t,
		fakeharness.Step{EmitEnv: &fakeharness.EmitEnv{Name: "API_KEY", SplitAt: 7, PauseMS: 300}},
		fakeharness.Step{WaitFile: &fakeharness.WaitFile{Path: release, TimeoutMS: 20_000}},
		fakeharness.Step{EmitEnv: &fakeharness.EmitEnv{Name: "API_KEY"}},
	)
	dir := delivered(t, redact.Needle{Ref: apiRef, Value: []byte(apiValue)})
	s := launchIn(t, dir, fakeLaunch(script, execwrap.ModeHeadless), true, withDelivery+"=1", "TMPDIR="+tmp)

	// While the harness runs: the first print is out, and nothing holds the value.
	logPath := filepath.Join(dir, execwrap.OutputLog)
	for deadline := time.Now().Add(20 * time.Second); ; time.Sleep(20 * time.Millisecond) {
		if raw, _ := os.ReadFile(logPath); bytes.Contains(raw, []byte(marker)) { //nolint:gosec // a file of the test
			break
		}
		if time.Now().After(deadline) {
			raw, _ := os.ReadFile(logPath) //nolint:gosec // a file of the test
			t.Fatalf("the first print never reached the log: %q; pane %q", raw, s.pane.String())
		}
	}
	noValueIn(t, apiValue, dir, tmp)
	if err := os.WriteFile(release, nil, 0o600); err != nil {
		t.Fatal(err)
	}

	exit := s.waitExit()
	if exit.ExitCode == nil || *exit.ExitCode != 0 || exit.Error != "" {
		t.Fatalf("exit.json = %+v; pane %q", exit, s.pane.String())
	}
	log, _ := os.ReadFile(logPath) //nolint:gosec // a file of the test
	for what, out := range map[string]string{"log": string(log), "pane": s.pane.String()} {
		if strings.Count(out, marker) != 2 || strings.Contains(out, apiValue[:7]) {
			t.Errorf("the %s is %q: want the secret redacted twice and no piece of it", what, out)
		}
	}
	noValueIn(t, apiValue, dir, tmp)
}

func TestARunWithoutSecretsNeedsNoRetention(t *testing.T) {
	s := launch(t, execwrap.Launch{Argv: shell("echo plain; echo " + apiValue), Grace: time.Second}, true)
	exit := s.waitExit()
	if exit.ExitCode == nil || *exit.ExitCode != 0 || exit.Error != "" {
		t.Fatalf("exit.json = %+v", exit)
	}
	if log, _ := os.ReadFile(filepath.Join(s.dir, execwrap.OutputLog)); string(log) != "plain\n"+apiValue+"\n" { //nolint:gosec // a file of the test
		t.Errorf("without secrets the log is the output as it is, got %q", log)
	}
}

// mustNotRun is a launch whose harness leaves a file if it ever runs.
func mustNotRun(mode string) execwrap.Launch {
	return execwrap.Launch{
		Argv: shell("touch must-not-run"), Mode: mode, Grace: time.Second,
		SecretEnv: []execwrap.SecretEnv{{Name: "API_KEY", Ref: apiRef}},
	}
}

func assertNothingRan(t *testing.T, s *spawned, why string) {
	t.Helper()
	exit := s.waitExit()
	if !strings.HasPrefix(exit.Error, "CF_SECRET_UNAVAILABLE: ") || !strings.Contains(exit.Error, why) || exit.ExitCode != nil {
		t.Errorf("exit.json = %+v, want %q", exit, why)
	}
	for _, name := range []string{execwrap.StartedFile, "must-not-run"} {
		if _, err := os.Stat(filepath.Join(s.dir, name)); err == nil {
			t.Errorf("%s exists: the harness ran", name)
		}
	}
	noValueIn(t, apiValue, s.dir)
}

func TestNothingRunsWithoutTheSecrets(t *testing.T) {
	for name, tc := range map[string]struct {
		prepare  func(t *testing.T, dir string)
		delivery string
		why      string
	}{
		"revoked from the harness": {
			prepare: func(t *testing.T, dir string) {
				if err := secretjit.New().Revoke(dir, []string{apiRef}); err != nil {
					t.Fatal(err)
				}
			},
			delivery: "1", why: "not available to the harness",
		},
		"not delivered":                {delivery: "1", why: "no secrets delivered"},
		"retention unreadable":         {delivery: "broken", why: "the output cannot be redacted"},
		"only a longer value retained": {delivery: "longer", why: "would not be redacted"},
		"no secret store":              {why: "the output cannot be redacted"},
	} {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			if name != "not delivered" {
				dir = delivered(t, redact.Needle{Ref: apiRef, Value: []byte(apiValue)})
			}
			if tc.prepare != nil {
				tc.prepare(t, dir)
			}
			env := []string{}
			if tc.delivery != "" {
				env = append(env, withDelivery+"="+tc.delivery)
			}
			assertNothingRan(t, launchIn(t, dir, mustNotRun(execwrap.ModeHeadless), true, env...), tc.why)
		})
	}
}

func TestAnInteractiveRunWithSecretsNeedsTheAcknowledgementOfTheFilter(t *testing.T) {
	for name, ack := range map[string]func(path string) error{
		"none":    func(string) error { return nil },
		"refused": func(path string) error { return cfruntime.AckCapture(path, errors.New("cannot")) },
		"garbled": func(path string) error { return os.WriteFile(path, []byte("readyish"), 0o600) },
	} {
		t.Run(name, func(t *testing.T) {
			dir := delivered(t, redact.Needle{Ref: apiRef, Value: []byte(apiValue)})
			if err := ack(filepath.Join(dir, execwrap.CaptureAckFile)); err != nil {
				t.Fatal(err)
			}
			assertNothingRan(t, launchIn(t, dir, mustNotRun(execwrap.ModeInteractive), true, withDelivery+"=1"), "does not redact")
		})
	}
	t.Run("acknowledged", func(t *testing.T) {
		dir := delivered(t, redact.Needle{Ref: apiRef, Value: []byte(apiValue)})
		if err := cfruntime.AckCapture(filepath.Join(dir, execwrap.CaptureAckFile), nil); err != nil {
			t.Fatal(err)
		}
		l := mustNotRun(execwrap.ModeInteractive)
		l.Argv = shell(`[ "$API_KEY" = ` + apiValue + ` ] && touch ran`)
		s := launchIn(t, dir, l, true, withDelivery+"=1")
		if exit := s.waitExit(); exit.ExitCode == nil || *exit.ExitCode != 0 || exit.Error != "" {
			t.Fatalf("exit.json = %+v", exit)
		}
		if _, err := os.Stat(filepath.Join(dir, "ran")); err != nil {
			t.Errorf("the harness did not get its secret: %v", err)
		}
	})
}

// chunked writes each piece in a write of its own, as a terminal hands them over.
func chunked(pieces ...string) io.Reader {
	r, w := io.Pipe()
	go func() {
		for _, p := range pieces {
			if _, err := w.Write([]byte(p)); err != nil {
				return
			}
		}
		_ = w.Close()
	}()
	return r
}

func retained(secrets ...redact.Needle) execwrap.LoadSecrets {
	return func() ([]redact.Needle, error) { return secrets, nil }
}

func TestTheFilterRedactsWhatATerminalMakesOfASecret(t *testing.T) {
	const multi = "first-line-secret\nsecond-line-secret"
	dir := t.TempDir()
	var out bytes.Buffer
	// A PTY turns "\n" into "\r\n"; the pieces split the secret where they please.
	in := chunked("$ run\r\nfirst-li", "ne-secret\r\nsecond-line-s", "ecret\r\n", "token=", apiValue[:3], apiValue[3:], " done\r\n")
	err := execwrap.RedactPipe(execwrap.PipeOptions{
		RunDir: dir, In: in, Out: &out,
		Retention: retained(redact.Needle{Ref: "team/pem", Value: []byte(multi)}, redact.Needle{Ref: apiRef, Value: []byte(apiValue)}),
	})
	if err != nil {
		t.Fatal(err)
	}
	got := out.String()
	for _, leak := range []string{"first-line-secret", "second-line-secret", apiValue} {
		if strings.Contains(got, leak) {
			t.Errorf("the log has %q: %q", leak, got)
		}
	}
	if !strings.HasPrefix(got, "$ run\r\n") || !strings.Contains(got, "[REDACTED:team/pem]") || !strings.HasSuffix(got, "token="+marker+" done\r\n") {
		t.Errorf("the log is %q", got)
	}
	if acked, err := cfruntime.CaptureAcked(filepath.Join(dir, execwrap.CaptureAckFile)); !acked || err != nil {
		t.Errorf("acknowledged = %v, %v", acked, err)
	}
}

func TestAFilterThatCannotRedactSaysSoAndDrainsTheTerminal(t *testing.T) {
	for name, retention := range map[string]execwrap.LoadSecrets{
		"unreadable": func() ([]redact.Needle, error) { return nil, secretjit.ErrCorrupt },
		"too short":  retained(redact.Needle{Ref: apiRef, Value: []byte("short")}),
		"over the budget": retained(
			redact.Needle{Ref: "a", Value: bytes.Repeat([]byte("a"), redact.MaxNeedleBytes/2+1)},
			redact.Needle{Ref: "b", Value: bytes.Repeat([]byte("b"), redact.MaxNeedleBytes/2+1)},
		),
		"none": nil,
	} {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			var out bytes.Buffer
			big := strings.Repeat(apiValue+"\n", 1<<14) // more than a pipe holds
			err := execwrap.RedactPipe(execwrap.PipeOptions{RunDir: dir, Retention: retention, In: chunked(big), Out: &out})
			if err == nil || !strings.HasPrefix(err.Error(), "CF_SECRET_UNAVAILABLE: ") {
				t.Errorf("RedactPipe = %v", err)
			}
			if out.String() != execwrap.CaptureDisabledNote {
				t.Errorf("the log is %d bytes, want only the note", out.Len())
			}
			if acked, err := cfruntime.CaptureAcked(filepath.Join(dir, execwrap.CaptureAckFile)); acked || !errors.Is(err, cfruntime.ErrCaptureRefused) {
				t.Errorf("acknowledged = %v, %v; want a refusal", acked, err)
			}
		})
	}
}

type failingWriter struct{}

func (failingWriter) Write([]byte) (int, error) { return 0, errors.New("disk full") }

func TestAFilterThatCannotWriteTheLogStops(t *testing.T) {
	err := execwrap.RedactPipe(execwrap.PipeOptions{
		RunDir: t.TempDir(), Retention: retained(redact.Needle{Ref: apiRef, Value: []byte(apiValue)}),
		In: chunked("hello\n", apiValue), Out: failingWriter{},
	})
	if err == nil || !strings.Contains(err.Error(), "disk full") {
		t.Errorf("RedactPipe = %v", err)
	}
}

// The secrets of a configuration are in files, not in the environment: a launch
// with RedactOutput and no SecretEnv still redacts what the delivery retains,
// and does not run if it cannot.
func TestRedactOutputRedactsWithoutAnySecretEnv(t *testing.T) {
	dir := delivered(t, redact.Needle{Ref: apiRef, Value: []byte(apiValue)})
	l := execwrap.Launch{Argv: shell("echo " + apiValue), Grace: time.Second, RedactOutput: true}
	s := launchIn(t, dir, l, true, withDelivery+"=1")
	if exit := s.waitExit(); exit.ExitCode == nil || *exit.ExitCode != 0 || exit.Error != "" {
		t.Fatalf("exit.json = %+v", exit)
	}
	log, _ := os.ReadFile(filepath.Join(dir, execwrap.OutputLog)) //nolint:gosec // a file of the test
	if string(log) != marker+"\n" {
		t.Errorf("the log is %q, want the secret redacted", log)
	}
}

func TestRedactOutputWithoutRetentionRunsNothing(t *testing.T) {
	l := execwrap.Launch{Argv: shell("touch must-not-run"), Grace: time.Second, RedactOutput: true}
	assertNothingRan(t, launchIn(t, t.TempDir(), l, true), "the output cannot be redacted")
}
