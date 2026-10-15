package fakeharness_test

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Autarquico/codefabric/internal/adapters/harness"
	"github.com/Autarquico/codefabric/internal/support/treedigest"
	"github.com/Autarquico/codefabric/internal/testutil/fakeharness"
)

func TestMain(m *testing.M) {
	fakeharness.RunIfRequested()
	os.Exit(m.Run())
}

// bound is how long a test waits for what must happen before it fails.
const bound = 5 * time.Second

func ptr[T any](v T) *T { return &v }

func writeScript(t *testing.T, path string, s fakeharness.Script) string {
	t.Helper()
	b, err := json.Marshal(s)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, b, 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

type ran struct {
	code   int
	stdout string
	stderr string
}

// run runs Main in this process with the script and env.
func run(t *testing.T, stdin io.Reader, env []string, steps ...fakeharness.Step) ran {
	t.Helper()
	p := writeScript(t, filepath.Join(t.TempDir(), "script.json"), fakeharness.Script{Steps: steps})
	if stdin == nil {
		stdin = strings.NewReader("")
	}
	var out, errb bytes.Buffer
	code := fakeharness.Main([]string{"fakeharness", "--script", p}, env, stdin, &out, &errb)
	return ran{code, out.String(), errb.String()}
}

// self is the test binary started as the program.
func self(t *testing.T, args ...string) *exec.Cmd {
	t.Helper()
	cmd := exec.CommandContext(t.Context(), os.Args[0], args...) //nolint:gosec // the test binary itself
	cmd.Env = append(os.Environ(), fakeharness.AsFakeharnessEnv+"=1")
	return cmd
}

func exitCode(t *testing.T, err error) int {
	t.Helper()
	var ee *exec.ExitError
	if err == nil {
		return 0
	}
	if !errors.As(err, &ee) {
		t.Fatalf("running the program: %v", err)
	}
	return ee.ExitCode()
}

func TestRunIfRequested_TheProcessExitsWithTheCodeOfItsExitStep(t *testing.T) {
	out := filepath.Join(t.TempDir(), "after-exit")
	p := writeScript(t, filepath.Join(t.TempDir(), "s.json"), fakeharness.Script{Steps: []fakeharness.Step{
		{Exit: ptr(7)},
		{Record: &fakeharness.Recorder{Path: out}},
	}})
	if code := exitCode(t, self(t, "--script", p).Run()); code != 7 {
		t.Errorf("exit code = %d, want 7", code)
	}
	if _, err := os.Stat(out); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("a step after exit ran: %v", err)
	}
}

func TestRunIfRequested_RecordsTheEnvironmentWithoutItsOwnSwitch(t *testing.T) {
	out := filepath.Join(t.TempDir(), "record.json")
	p := writeScript(t, filepath.Join(t.TempDir(), "s.json"), fakeharness.Script{Steps: []fakeharness.Step{
		{Record: &fakeharness.Recorder{Path: out, Env: []string{"CF_*", "FAKEHARNESS_PROBE"}}},
	}})
	cmd := self(t, "--script", p, "--prompt", "do it")
	cmd.Env = append(cmd.Env, "FAKEHARNESS_PROBE=yes")
	if code := exitCode(t, cmd.Run()); code != 0 {
		t.Fatalf("exit code = %d", code)
	}
	rec, err := fakeharness.ReadRecord(out)
	if err != nil {
		t.Fatal(err)
	}
	if rec.Env["FAKEHARNESS_PROBE"] != "yes" {
		t.Errorf("env = %v, want FAKEHARNESS_PROBE=yes", rec.Env)
	}
	if _, ok := rec.Env[fakeharness.AsFakeharnessEnv]; ok {
		t.Errorf("env = %v, has %s", rec.Env, fakeharness.AsFakeharnessEnv)
	}
	if got := rec.Argv[1:]; strings.Join(got, " ") != "--script "+p+" --prompt do it" {
		t.Errorf("argv = %q", rec.Argv)
	}
}

func TestMain_EndsWith0WhenTheStepsRunOut(t *testing.T) {
	if r := run(t, nil, nil, fakeharness.Step{Emit: &fakeharness.Line{Type: harness.EventSessionStarted}}); r.code != 0 {
		t.Errorf("exit code = %d, stderr %q", r.code, r.stderr)
	}
}

func TestMain_EmitWritesEachLineAsOneJSONObject(t *testing.T) {
	at := time.Date(2026, 10, 15, 12, 0, 0, 0, time.UTC)
	r := run(t, nil, nil,
		fakeharness.Step{Emit: &fakeharness.Line{Type: harness.EventSessionStarted, SessionID: "s1", Model: "m1", At: &at}},
		fakeharness.Step{Emit: &fakeharness.Line{Type: harness.EventUsage, Usage: &fakeharness.Usage{InputTokens: ptr[int64](0)}}},
		fakeharness.Step{Emit: &fakeharness.Line{Type: harness.EventResult, Result: &fakeharness.Result{Summary: "done"}}},
	)
	if r.code != 0 {
		t.Fatalf("exit code = %d, stderr %q", r.code, r.stderr)
	}
	lines := strings.Split(strings.TrimSuffix(r.stdout, "\n"), "\n")
	if len(lines) != 3 {
		t.Fatalf("stdout = %q, want 3 lines", r.stdout)
	}
	var types []string
	for _, l := range lines {
		var obj map[string]json.RawMessage
		if err := json.Unmarshal([]byte(l), &obj); err != nil {
			t.Fatalf("line %q is not a JSON object: %v", l, err)
		}
		var typ string
		_ = json.Unmarshal(obj["type"], &typ)
		types = append(types, typ)
	}
	if got := strings.Join(types, ","); got != "session_started,usage,result" {
		t.Errorf("types = %s", got)
	}
	if !strings.Contains(lines[1], `"input_tokens":0`) || strings.Contains(lines[1], "output_tokens") {
		t.Errorf("usage line %q: a known zero must be written and an unknown left out", lines[1])
	}
}

func TestMain_RecordCapturesArgvFilteredEnvAndConfigDigest(t *testing.T) {
	cfg := t.TempDir()
	if err := os.WriteFile(filepath.Join(cfg, "settings.json"), []byte(`{"a":1}`), 0o600); err != nil {
		t.Fatal(err)
	}
	auth := filepath.Join(t.TempDir(), "credentials.json")
	if err := os.WriteFile(auth, []byte("token-1"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(auth, filepath.Join(cfg, fakeharness.AuthFile)); err != nil {
		t.Fatal(err)
	}
	env := []string{
		fakeharness.ConfigDirEnv + "=" + cfg, "CF_RUN=r1", "TMUX=/tmp/x", "HOME=/h",
		"SECRET_TOKEN=s3cr3t", "PATH=/bin",
	}
	record := func(keep ...string) fakeharness.Record {
		t.Helper()
		out := filepath.Join(t.TempDir(), "record.json")
		if r := run(t, nil, env, fakeharness.Step{Record: &fakeharness.Recorder{Path: out, Env: keep}}); r.code != 0 {
			t.Fatalf("exit code = %d, stderr %q", r.code, r.stderr)
		}
		rec, err := fakeharness.ReadRecord(out)
		if err != nil {
			t.Fatal(err)
		}
		return rec
	}

	rec := record()
	want := map[string]string{fakeharness.ConfigDirEnv: cfg, "CF_RUN": "r1", "TMUX": "/tmp/x", "HOME": "/h"}
	if len(rec.Env) != len(want) {
		t.Errorf("env = %v, want %v", rec.Env, want)
	}
	for k, v := range want {
		if rec.Env[k] != v {
			t.Errorf("env[%s] = %q, want %q", k, rec.Env[k], v)
		}
	}
	if rec.Argv[0] != "fakeharness" || rec.Argv[1] != "--script" {
		t.Errorf("argv = %q", rec.Argv)
	}
	if wd, _ := os.Getwd(); rec.Dir != wd {
		t.Errorf("dir = %q, want %q", rec.Dir, wd)
	}
	digest, err := treedigest.Dir(cfg, treedigest.Options{ApprovedLinks: []string{fakeharness.AuthFile}})
	if err != nil {
		t.Fatal(err)
	}
	if rec.ConfigDigest != digest {
		t.Errorf("config digest = %q, want %q", rec.ConfigDigest, digest)
	}

	if err := os.WriteFile(auth, []byte("token-2"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := record().ConfigDigest; got != digest {
		t.Errorf("rotating the auth changed the config digest: %q, want %q", got, digest)
	}
	if err := os.WriteFile(filepath.Join(cfg, "settings.json"), []byte(`{"a":2}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := record().ConfigDigest; got == digest {
		t.Error("changing the settings did not change the config digest")
	}
	if got := record("SECRET_*").Env; len(got) != 1 || got["SECRET_TOKEN"] != "s3cr3t" {
		t.Errorf("env with SECRET_* = %v", got)
	}
}

func TestMain_RecordFailsOnASymlinkThatIsNotTheAuth(t *testing.T) {
	cfg := t.TempDir()
	if err := os.Symlink("/etc/hosts", filepath.Join(cfg, "settings.json")); err != nil {
		t.Fatal(err)
	}
	out := filepath.Join(t.TempDir(), "record.json")
	r := run(t, nil, []string{fakeharness.ConfigDirEnv + "=" + cfg}, fakeharness.Step{Record: &fakeharness.Recorder{Path: out}})
	if r.code != fakeharness.ExitScriptError {
		t.Errorf("exit code = %d, want %d", r.code, fakeharness.ExitScriptError)
	}
}

func TestMain_WaitFileGoesOnOnceTheFileExists(t *testing.T) {
	gate := filepath.Join(t.TempDir(), "gate")
	go func() {
		time.Sleep(50 * time.Millisecond)
		_ = os.WriteFile(gate, nil, 0o600)
	}()
	r := run(t, nil, nil,
		fakeharness.Step{WaitFile: &fakeharness.WaitFile{Path: gate, TimeoutMS: bound.Milliseconds()}},
		fakeharness.Step{Exit: ptr(3)},
	)
	if r.code != 3 {
		t.Errorf("exit code = %d, want 3; stderr %q", r.code, r.stderr)
	}
}

func TestMain_WaitFileTimesOutAtItsBound(t *testing.T) {
	start := time.Now()
	r := run(t, nil, nil, fakeharness.Step{WaitFile: &fakeharness.WaitFile{Path: filepath.Join(t.TempDir(), "never"), TimeoutMS: 50}})
	if r.code != fakeharness.ExitTimeout {
		t.Errorf("exit code = %d, want %d", r.code, fakeharness.ExitTimeout)
	}
	if took := time.Since(start); took < 50*time.Millisecond || took > bound {
		t.Errorf("it waited %v for a 50ms timeout", took)
	}
	if !strings.Contains(r.stderr, "timed out") {
		t.Errorf("stderr = %q", r.stderr)
	}
}

func TestMain_ReadLineReadsOneLineOfStdinEach(t *testing.T) {
	dir := t.TempDir()
	first, second := filepath.Join(dir, "first"), filepath.Join(dir, "second")
	r := run(t, strings.NewReader("hello\r\nworld"), nil,
		fakeharness.Step{ReadLine: &fakeharness.ReadLine{TimeoutMS: bound.Milliseconds(), To: first}},
		fakeharness.Step{ReadLine: &fakeharness.ReadLine{TimeoutMS: bound.Milliseconds(), To: second}},
	)
	if r.code != 0 {
		t.Fatalf("exit code = %d, stderr %q", r.code, r.stderr)
	}
	for path, want := range map[string]string{first: "hello", second: "world"} {
		if got, _ := os.ReadFile(path); string(got) != want { //nolint:gosec // under the temp dir
			t.Errorf("%s = %q, want %q", filepath.Base(path), got, want)
		}
	}
}

func TestMain_ReadLineTimesOutAtItsBound(t *testing.T) {
	stdin, w := io.Pipe()
	t.Cleanup(func() { _ = w.Close() })
	r := run(t, stdin, nil, fakeharness.Step{ReadLine: &fakeharness.ReadLine{TimeoutMS: 50}})
	if r.code != fakeharness.ExitTimeout {
		t.Errorf("exit code = %d, want %d", r.code, fakeharness.ExitTimeout)
	}
}

func TestMain_ReadLineAtTheEndOfStdinFails(t *testing.T) {
	r := run(t, strings.NewReader(""), nil, fakeharness.Step{ReadLine: &fakeharness.ReadLine{TimeoutMS: bound.Milliseconds()}})
	if r.code != fakeharness.ExitScriptError {
		t.Errorf("exit code = %d, want %d", r.code, fakeharness.ExitScriptError)
	}
}

// TestMain_ScriptPrecedence checks the order of the package doc: --script,
// then FAKEHARNESS_SCRIPT, then fakeharness.json in FAKEHARNESS_CONFIG_DIR,
// and that one given but unreadable is an error, not a fall through.
func TestMain_ScriptPrecedence(t *testing.T) {
	dir := t.TempDir()
	script := func(name string, code int) string {
		return writeScript(t, filepath.Join(dir, name), fakeharness.Script{Steps: []fakeharness.Step{{Exit: ptr(code)}}})
	}
	flagScript, envScript := script("flag.json", 11), script("env.json", 12)
	cfg := filepath.Join(dir, "config")
	if err := os.Mkdir(cfg, 0o700); err != nil {
		t.Fatal(err)
	}
	script(filepath.Join("config", fakeharness.ScriptFile), 13)
	missing := filepath.Join(dir, "missing.json")

	for _, tc := range []struct {
		name string
		flag string
		env  []string
		want int
	}{
		{"flag over env and config dir", flagScript, []string{fakeharness.ScriptEnv + "=" + envScript, fakeharness.ConfigDirEnv + "=" + cfg}, 11},
		{"env over config dir", "", []string{fakeharness.ScriptEnv + "=" + envScript, fakeharness.ConfigDirEnv + "=" + cfg}, 12},
		{"config dir alone", "", []string{fakeharness.ConfigDirEnv + "=" + cfg}, 13},
		{"none", "", nil, fakeharness.ExitScriptError},
		{"flag unreadable", missing, []string{fakeharness.ScriptEnv + "=" + envScript}, fakeharness.ExitScriptError},
		{"env unreadable", "", []string{fakeharness.ScriptEnv + "=" + missing, fakeharness.ConfigDirEnv + "=" + cfg}, fakeharness.ExitScriptError},
	} {
		t.Run(tc.name, func(t *testing.T) {
			argv := []string{"fakeharness"}
			if tc.flag != "" {
				argv = append(argv, "--script", tc.flag)
			}
			var errb bytes.Buffer
			if got := fakeharness.Main(argv, tc.env, strings.NewReader(""), io.Discard, &errb); got != tc.want {
				t.Errorf("exit code = %d, want %d; stderr %q", got, tc.want, errb.String())
			}
		})
	}
}

func TestMain_VersionPrintsTheVersion(t *testing.T) {
	var out bytes.Buffer
	if code := fakeharness.Main([]string{"fakeharness", "--version"}, nil, nil, &out, io.Discard); code != 0 {
		t.Fatalf("exit code = %d", code)
	}
	if got := out.String(); got != "fakeharness "+fakeharness.Version+"\n" {
		t.Errorf("stdout = %q", got)
	}
}

func TestMain_UnknownFlagIsAScriptError(t *testing.T) {
	if code := fakeharness.Main([]string{"fakeharness", "--nope"}, nil, nil, io.Discard, io.Discard); code != fakeharness.ExitScriptError {
		t.Errorf("exit code = %d, want %d", code, fakeharness.ExitScriptError)
	}
}

func TestParseScript_RejectsWhatItCannotRunSafely(t *testing.T) {
	for name, src := range map[string]string{
		"two actions":           `{"steps":[{"exit":0,"ignore_signals":true}]}`,
		"no action":             `{"steps":[{}]}`,
		"unknown step":          `{"steps":[{"spawn_child":{}}]}`,
		"wait_file unbounded":   `{"steps":[{"wait_file":{"path":"x"}}]}`,
		"wait_file over max":    `{"steps":[{"wait_file":{"path":"x","timeout_ms":300001}}]}`,
		"read_line unbounded":   `{"steps":[{"read_line":{}}]}`,
		"unknown event":         `{"steps":[{"emit":{"type":"thinking"}}]}`,
		"usage without usage":   `{"steps":[{"emit":{"type":"usage"}}]}`,
		"result without result": `{"steps":[{"emit":{"type":"result"}}]}`,
		"exit out of range":     `{"steps":[{"exit":256}]}`,
		"record without path":   `{"steps":[{"record":{}}]}`,
	} {
		if _, err := fakeharness.ParseScript([]byte(src)); err == nil {
			t.Errorf("%s: ParseScript(%s) = nil error", name, src)
		}
	}
	if _, err := fakeharness.ParseScript([]byte(`{"steps":[{"wait_file":{"path":"x","timeout_ms":300000}},{"read_line":{"timeout_ms":1}},{"exit":255}]}`)); err != nil {
		t.Errorf("a script at its bounds: %v", err)
	}
}
