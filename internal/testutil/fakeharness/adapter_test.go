package fakeharness_test

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Autarquico/codefabric/internal/adapters/harness"
	"github.com/Autarquico/codefabric/internal/domain"
	"github.com/Autarquico/codefabric/internal/support/clock"
	"github.com/Autarquico/codefabric/internal/support/treedigest"
	"github.com/Autarquico/codefabric/internal/testutil/fakeharness"
)

// testHost is the Host of a test: its own HOME, the rest of this process.
type testHost struct{ home string }

func newHost(t *testing.T) testHost { return testHost{home: t.TempDir()} }

func (h testHost) Home() string                       { return h.home }
func (testHost) Environ() []string                    { return os.Environ() }
func (testHost) Getenv(key string) string             { return os.Getenv(key) }
func (testHost) LookPath(file string) (string, error) { return exec.LookPath(file) }
func (testHost) Clock() clock.Clock                   { return clock.Real{} }
func (h testHost) configDir() string                  { return filepath.Join(h.home, fakeharness.ConfigDirName) }
func (h testHost) path(rel string) string             { return filepath.Join(h.configDir(), rel) }

func (h testHost) Run(ctx context.Context, c harness.Cmd) (harness.CmdResult, error) {
	cmd := exec.CommandContext(ctx, c.Argv[0], c.Argv[1:]...) //nolint:gosec // the command of the test
	cmd.Env = append(os.Environ(), c.Env...)
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	err := cmd.Run()
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		return harness.CmdResult{ExitCode: ee.ExitCode(), Stdout: out.Bytes(), Stderr: errb.Bytes()}, nil
	}
	return harness.CmdResult{Stdout: out.Bytes(), Stderr: errb.Bytes()}, err
}

var allCaps = harness.Capabilities{
	Modes:  []domain.Mode{domain.ModeHeadless, domain.ModeInteractive},
	Resume: true, NativeCheckpoint: true, NeedsInputSignal: true, StructuredInput: true,
	UsageEvents: true, ModelSelection: true, ConfigIsolation: true,
}

// selfAdapter is an Adapter whose program is this test binary.
func selfAdapter(caps harness.Capabilities) fakeharness.Adapter {
	return fakeharness.Adapter{Caps: caps, Binary: os.Args[0], Env: []string{fakeharness.AsFakeharnessEnv + "=1"}}
}

func TestAdapter_WithEverythingFalseDelegatesToUnsupported(t *testing.T) {
	a := fakeharness.Adapter{}
	host := newHost(t)
	ctx := t.Context()
	h := harness.Handle{RunDir: t.TempDir(), OutputPath: filepath.Join(t.TempDir(), "output.log")}
	run := harness.RunSpec{Mode: domain.ModeHeadless, RunDir: t.TempDir()}

	for op, got := range map[string]struct {
		value any
		err   error
	}{
		"Start":        pair(a.Start(ctx, host, run)),
		"Resume":       pair(a.Resume(ctx, host, run, harness.Checkpoint{Harness: "fake", Format: fakeharness.CheckpointFormat, Data: []byte("s")})),
		"Install":      pair(a.Install(ctx, host, harness.InstallOptions{DryRun: true})),
		"Checkpoint":   pair(a.Checkpoint(ctx, host, h)),
		"Stop":         {nil, a.Stop(ctx, host, h)},
		"SendInput":    {nil, a.SendInput(ctx, host, h, harness.Input{Text: "x"})},
		"CollectUsage": pair(a.CollectUsage(ctx, host, h)),
		"ApplyConfig TargetRun": pair(a.ApplyConfig(ctx, host, harness.NativeSnapshot{Harness: "fake", SchemaVersion: fakeharness.SchemaVersion},
			harness.ApplyOptions{Target: harness.TargetRun{Dir: t.TempDir()}})),
	} {
		if !errors.Is(got.err, harness.ErrUnsupported) {
			t.Errorf("%s: err = %v, want ErrUnsupported", op, got.err)
		}
		if got.value != nil && !reflect.ValueOf(got.value).IsZero() {
			t.Errorf("%s: returned %#v with ErrUnsupported, want the zero value", op, got.value)
		}
	}
	if entries, _ := os.ReadDir(h.RunDir); len(entries) > 0 {
		t.Errorf("an unsupported SendInput wrote %v", entries)
	}
}

func pair[T any](v T, err error) struct {
	value any
	err   error
} {
	return struct {
		value any
		err   error
	}{v, err}
}

func TestAdapter_IDIsFakeUnlessNamed(t *testing.T) {
	if got := (fakeharness.Adapter{}).ID(); got != "fake" {
		t.Errorf("ID() = %q", got)
	}
	if got := (fakeharness.Adapter{Name: "claude"}).ID(); got != "claude" {
		t.Errorf("ID() = %q", got)
	}
	if _, err := harness.NewRegistry(fakeharness.Adapter{}, fakeharness.Adapter{Name: "fake-two"}); err != nil {
		t.Errorf("two fakes do not register: %v", err)
	}
}

func TestAdapter_StartBuildsTheLaunchWithoutWritingAndIsIdempotent(t *testing.T) {
	host := newHost(t)
	run := harness.RunSpec{
		RunID: "r1", Mode: domain.ModeHeadless, Prompt: "do it", Model: "m1",
		RunDir: t.TempDir(), ConfigDir: t.TempDir(), Worktree: t.TempDir(),
	}
	secretEnv := []harness.SecretEnv{{Name: "FAKE_API_KEY", Ref: "fake/api-key"}}
	a := fakeharness.Adapter{Caps: allCaps, Binary: "/bin/fake", Env: []string{"EXTRA=1"}, Script: "/s.json", SecretEnv: secretEnv}
	first, err := a.Start(t.Context(), host, run)
	if err != nil {
		t.Fatal(err)
	}
	second, err := a.Start(t.Context(), host, run)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(first, second) {
		t.Errorf("Start is not idempotent: %#v then %#v", first, second)
	}
	if !reflect.DeepEqual(first.SecretEnv, secretEnv) {
		t.Errorf("secret env = %+v, want %+v", first.SecretEnv, secretEnv)
	}
	wantArgv := []string{"/bin/fake", "--mode", "headless", "--prompt", "do it", "--script", "/s.json", "--model", "m1"}
	if !slices.Equal(first.Argv, wantArgv) {
		t.Errorf("argv = %q, want %q", first.Argv, wantArgv)
	}
	if !slices.Equal(first.Env, []string{fakeharness.ConfigDirEnv + "=" + run.ConfigDir, "EXTRA=1"}) {
		t.Errorf("env = %q", first.Env)
	}
	for _, dir := range []string{run.RunDir, run.ConfigDir, run.Worktree, host.home} {
		if entries, _ := os.ReadDir(dir); len(entries) > 0 {
			t.Errorf("Start wrote into %s: %v", dir, entries)
		}
	}

	plain := fakeharness.Adapter{Binary: "/bin/fake", Caps: harness.Capabilities{Modes: []domain.Mode{domain.ModeHeadless}}}
	l, err := plain.Start(t.Context(), host, run)
	if err != nil {
		t.Fatal(err)
	}
	if slices.Contains(l.Argv, "--model") {
		t.Errorf("argv = %q: --model without ModelSelection", l.Argv)
	}
	if want := fakeharness.ConfigDirEnv + "=" + host.configDir(); !slices.Contains(l.Env, want) {
		t.Errorf("env = %q: without ConfigIsolation it reads %s", l.Env, want)
	}
	run.Mode = domain.ModeInteractive
	if _, err := plain.Start(t.Context(), host, run); !errors.Is(err, harness.ErrUnsupportedMode) {
		t.Errorf("Start in an unsupported mode: %v, want ErrUnsupportedMode", err)
	}
}

func TestAdapter_StartWithoutABinaryLaunchesTheFakeharnessOfThePath(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("PATH", dir)
	run := harness.RunSpec{Mode: domain.ModeHeadless, Prompt: "hi", RunDir: t.TempDir()}
	a := fakeharness.Adapter{Caps: allCaps}
	if _, err := a.Start(t.Context(), newHost(t), run); err == nil {
		t.Error("Start without fakeharness in the PATH did not fail")
	}
	bin := filepath.Join(dir, "fakeharness")
	if err := os.WriteFile(bin, []byte("#!/bin/sh\n"), 0o700); err != nil { //nolint:gosec // an executable of the test
		t.Fatal(err)
	}
	l, err := a.Start(t.Context(), newHost(t), run)
	if err != nil || len(l.Argv) == 0 || l.Argv[0] != bin {
		t.Errorf("Start = %v, %v; want argv[0] %s", l.Argv, err, bin)
	}
}

func TestAdapter_ResumeRefusesACheckpointOfAnotherHarness(t *testing.T) {
	a := fakeharness.Adapter{Caps: allCaps}
	run := harness.RunSpec{Mode: domain.ModeHeadless}
	for _, cp := range []harness.Checkpoint{
		{Harness: "other", Format: fakeharness.CheckpointFormat, Data: []byte("s1")},
		{Harness: "fake", Format: "other", Data: []byte("s1")},
		{Harness: "fake", Format: fakeharness.CheckpointFormat},
	} {
		if _, err := a.Resume(t.Context(), newHost(t), run, cp); err == nil || errors.Is(err, harness.ErrUnsupported) {
			t.Errorf("Resume(%+v) = %v, want a plain error", cp, err)
		}
	}
}

func TestAdapter_DetectAndVersion(t *testing.T) {
	host := newHost(t)
	d, err := selfAdapter(allCaps).Detect(t.Context(), host)
	if err != nil || !d.Installed || d.Path != os.Args[0] {
		t.Errorf("Detect = %+v, %v", d, err)
	}
	v, err := selfAdapter(allCaps).Version(t.Context(), host)
	if err != nil || v != fakeharness.Version {
		t.Errorf("Version = %q, %v; want %q", v, err, fakeharness.Version)
	}
	missing := fakeharness.Adapter{Binary: filepath.Join(t.TempDir(), "fakeharness")}
	if d, err := missing.Detect(t.Context(), host); err != nil || d.Installed {
		t.Errorf("Detect of a missing binary = %+v, %v; want not installed and no error", d, err)
	}
}

// TestAdapter_RunsTheProgramItLaunches applies a configuration with a script
// to a TargetRun, runs the Launch of Start as the wrapper would, and reads
// back what the program did through the adapter.
func TestAdapter_RunsTheProgramItLaunches(t *testing.T) {
	host := newHost(t)
	a := selfAdapter(allCaps)
	if err := os.MkdirAll(host.configDir(), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(host.path(fakeharness.AuthFile), []byte("token"), 0o600); err != nil {
		t.Fatal(err)
	}
	runDir, cfg := t.TempDir(), t.TempDir()
	recordPath := filepath.Join(runDir, "record.json")
	scriptJSON := mustJSON(t, fakeharness.Script{Steps: []fakeharness.Step{
		{Emit: &fakeharness.Line{Type: harness.EventSessionStarted, SessionID: "s1", Model: "m1"}},
		{Emit: &fakeharness.Line{Type: harness.EventUsage, Usage: &fakeharness.Usage{InputTokens: ptr[int64](10), OutputTokens: ptr[int64](0)}}},
		{Record: &fakeharness.Recorder{Path: recordPath}},
		{Emit: &fakeharness.Line{Type: harness.EventResult, Result: &fakeharness.Result{Summary: "done"}}},
	}})
	snap := harness.NativeSnapshot{Harness: a.ID(), SchemaVersion: fakeharness.SchemaVersion, Components: []harness.Component{
		{Name: fakeharness.ScriptComponent, Kind: harness.KindFile, Ownership: harness.Managed, Files: []harness.File{{Path: fakeharness.ScriptFile, Mode: 0o600, Content: scriptJSON}}},
	}}
	applied, err := a.ApplyConfig(t.Context(), host, snap, harness.ApplyOptions{Target: harness.TargetRun{Dir: cfg}, LinkAuth: true})
	if err != nil {
		t.Fatal(err)
	}
	run := harness.RunSpec{RunID: "r1", Mode: domain.ModeHeadless, Prompt: "p", RunDir: runDir, ConfigDir: cfg, Worktree: t.TempDir(), OutputPath: filepath.Join(runDir, "output.log")}
	launch, err := a.Start(t.Context(), host, run)
	if err != nil {
		t.Fatal(err)
	}
	out, err := os.Create(run.OutputPath)
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.CommandContext(t.Context(), launch.Argv[0], launch.Argv[1:]...) //nolint:gosec // the Launch under test
	cmd.Env, cmd.Dir, cmd.Stdout, cmd.Stderr = append(os.Environ(), launch.Env...), run.Worktree, out, out
	if err := cmd.Run(); err != nil {
		t.Fatalf("the program failed: %v", err)
	}
	_ = out.Close()

	rec, err := fakeharness.ReadRecord(recordPath)
	if err != nil {
		t.Fatal(err)
	}
	want, err := treedigest.Dir(cfg, treedigest.Options{ApprovedLinks: applied.AuthLinks})
	if err != nil {
		t.Fatal(err)
	}
	if rec.ConfigDigest != want || rec.Env[fakeharness.ConfigDirEnv] != cfg {
		t.Errorf("record = %+v, want config %s with digest %s", rec, cfg, want)
	}
	if resolved, _ := filepath.EvalSymlinks(run.Worktree); rec.Dir != resolved && rec.Dir != run.Worktree {
		t.Errorf("record dir = %q, want the worktree %q", rec.Dir, run.Worktree)
	}

	h := harness.Handle{RunID: run.RunID, Mode: run.Mode, RunDir: runDir, ConfigDir: cfg, OutputPath: run.OutputPath}
	usage, err := a.CollectUsage(t.Context(), host, h)
	if err != nil {
		t.Fatal(err)
	}
	if len(usage) != 1 || *usage[0].InputTokens != 10 || usage[0].OutputTokens == nil || *usage[0].OutputTokens != 0 || usage[0].CacheReadTokens != nil {
		t.Errorf("usage = %+v, want input 10, output a known 0, cache read unknown", usage)
	}
	cp, err := a.Checkpoint(t.Context(), host, h)
	if err != nil || string(cp.Data) != "s1" || cp.Harness != "fake" {
		t.Fatalf("Checkpoint = %+v, %v", cp, err)
	}
	resumed, err := a.Resume(t.Context(), host, run, cp)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(resumed.Argv[len(resumed.Argv)-2:], []string{"--resume", "s1"}) {
		t.Errorf("resume argv = %q", resumed.Argv)
	}
}

func mustJSON(t *testing.T, s fakeharness.Script) []byte {
	t.Helper()
	p := writeScript(t, filepath.Join(t.TempDir(), "s.json"), s)
	b, err := os.ReadFile(p) //nolint:gosec // under the temp dir
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestAdapter_CheckpointWithoutASessionIsAnError(t *testing.T) {
	out := filepath.Join(t.TempDir(), "output.log")
	if err := os.WriteFile(out, []byte("no events here\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cp, err := fakeharness.Adapter{Caps: allCaps}.Checkpoint(t.Context(), newHost(t), harness.Handle{OutputPath: out})
	if err == nil || errors.Is(err, harness.ErrUnsupported) || cp.Data != nil {
		t.Errorf("Checkpoint = %+v, %v; want an error and no data", cp, err)
	}
}

func TestAdapter_CollectUsageReadsTheEventsPathAndNeverMakesUpUsage(t *testing.T) {
	a := fakeharness.Adapter{Caps: allCaps}
	dir := t.TempDir()
	if _, err := a.CollectUsage(t.Context(), newHost(t), harness.Handle{OutputPath: filepath.Join(dir, "missing")}); err == nil {
		t.Error("CollectUsage of a missing output = nil error")
	}
	events, output := filepath.Join(dir, "events.jsonl"), filepath.Join(dir, "output.log")
	if err := os.WriteFile(output, []byte(`{"type":"usage","usage":{"input_tokens":5}}`+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(events, []byte("plain text\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	usage, err := a.CollectUsage(t.Context(), newHost(t), harness.Handle{OutputPath: output, EventsPath: events})
	if err != nil || usage == nil || len(usage) != 0 {
		t.Errorf("usage = %#v, %v; want none, from the events path, and no error", usage, err)
	}
}

func TestAdapter_SendInputAppendsALineToTheInputFile(t *testing.T) {
	a := fakeharness.Adapter{Caps: allCaps}
	h := harness.Handle{RunDir: t.TempDir()}
	for _, text := range []string{"yes", "go on"} {
		if err := a.SendInput(t.Context(), newHost(t), h, harness.Input{Text: text}); err != nil {
			t.Fatal(err)
		}
	}
	got, _ := os.ReadFile(filepath.Join(h.RunDir, fakeharness.InputFile)) //nolint:gosec // under the temp dir
	if string(got) != "yes\ngo on\n" {
		t.Errorf("input file = %q", got)
	}
}

const eventsJSONL = `{"type":"session_started","session_id":"s1","model":"m1","at":"2026-10-15T12:00:00Z"}
not json at all
{"type":"thinking"}
{"type":"usage","model":"m1","usage":{"provider":"p","input_tokens":3,"cache_read_tokens":0}}
{"type":"input_required","message":"continue?"}
{"type":"error","message":"boom"}
{"type":"result","result":{"failed":true,"summary":"no"}}
`

func collect(t *testing.T, a fakeharness.Adapter, src string) []harness.Event {
	t.Helper()
	ch, err := a.Events(t.Context(), harness.Handle{}, io.NopCloser(strings.NewReader(src)))
	if err != nil {
		t.Fatal(err)
	}
	var got []harness.Event
	timeout := time.After(bound)
	for {
		select {
		case ev, ok := <-ch:
			if !ok {
				return got
			}
			got = append(got, ev)
		case <-timeout:
			t.Fatal("Events never closed its channel at the end of the source")
		}
	}
}

func TestAdapter_EventsParsesTheLinesOfTheProgramAndSkipsTheRest(t *testing.T) {
	got := collect(t, fakeharness.Adapter{Caps: allCaps}, eventsJSONL)
	var kinds []harness.EventKind
	for _, ev := range got {
		kinds = append(kinds, ev.Kind)
	}
	want := []harness.EventKind{harness.EventSessionStarted, harness.EventUsage, harness.EventInputRequired, harness.EventError, harness.EventResult}
	if !slices.Equal(kinds, want) {
		t.Fatalf("kinds = %v, want %v", kinds, want)
	}
	if got[0].SessionID != "s1" || !got[0].At.Equal(time.Date(2026, 10, 15, 12, 0, 0, 0, time.UTC)) {
		t.Errorf("session_started = %+v", got[0])
	}
	u := got[1].Usage
	if u == nil || u.Model != "m1" || u.Provider != "p" || *u.InputTokens != 3 || u.CacheReadTokens == nil || *u.CacheReadTokens != 0 || u.OutputTokens != nil {
		t.Errorf("usage = %+v: want input 3, a known zero cache read and an unknown output", u)
	}
	if got[2].Message != "continue?" || got[3].Message != "boom" {
		t.Errorf("messages = %q, %q", got[2].Message, got[3].Message)
	}
	if r := got[4].Result; r == nil || !r.Failed || r.Summary != "no" {
		t.Errorf("result = %+v", r)
	}
}

func TestAdapter_EventsLeavesOutWhatItsCapabilitiesDoNotReport(t *testing.T) {
	caps := allCaps
	caps.UsageEvents, caps.NeedsInputSignal = false, false
	for _, ev := range collect(t, fakeharness.Adapter{Caps: caps}, eventsJSONL) {
		if ev.Kind == harness.EventUsage || ev.Kind == harness.EventInputRequired {
			t.Errorf("event %s without its capability", ev.Kind)
		}
	}
}

// blockedSource is a source whose Read blocks until Close.
type blockedSource struct {
	closed   chan struct{}
	returned chan struct{}
	once     sync.Once
}

func newBlockedSource() *blockedSource {
	return &blockedSource{closed: make(chan struct{}), returned: make(chan struct{}, 1)}
}

func (s *blockedSource) Read([]byte) (int, error) {
	<-s.closed
	s.returned <- struct{}{}
	return 0, io.ErrClosedPipe
}

func (s *blockedSource) Close() error {
	s.once.Do(func() { close(s.closed) })
	return nil
}

// waitClosed receives from ch until it is closed, and fails when that
// takes more than most events or longer than bound.
func waitClosed(t *testing.T, ch <-chan harness.Event, most int) {
	t.Helper()
	timeout := time.After(bound)
	for n := 0; ; n++ {
		select {
		case _, ok := <-ch:
			if !ok {
				return
			}
			if n >= most {
				t.Fatalf("Events sent more than %d events after ctx was done", most)
			}
		case <-timeout:
			t.Fatal("Events did not close its channel once ctx was done")
		}
	}
}

func TestAdapter_EventsReturnsWhenCtxIsDoneWhileTheReadBlocks(t *testing.T) {
	src := newBlockedSource()
	ctx, cancel := context.WithCancel(t.Context())
	ch, err := fakeharness.Adapter{Caps: allCaps}.Events(ctx, harness.Handle{}, src)
	if err != nil {
		t.Fatal(err)
	}
	cancel()
	waitClosed(t, ch, 0)

	_ = src.Close()
	select {
	case <-src.returned:
	case <-time.After(bound):
		t.Fatal("the read in progress did not end when the caller closed the source")
	}
}

func TestAdapter_EventsReturnsWhenCtxIsDoneWhileTheSendBlocks(t *testing.T) {
	src := strings.Repeat(`{"type":"session_started","session_id":"s"}`+"\n", 1000)
	ctx, cancel := context.WithCancel(t.Context())
	ch, err := fakeharness.Adapter{Caps: allCaps}.Events(ctx, harness.Handle{}, io.NopCloser(strings.NewReader(src)))
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-ch:
	case <-time.After(bound):
		t.Fatal("no first event")
	}
	// Nobody receives now: the adapter is blocked sending the second, and
	// must give up on it, not wait for a receiver.
	time.Sleep(20 * time.Millisecond)
	cancel()
	time.Sleep(100 * time.Millisecond)
	waitClosed(t, ch, 0)
}

func TestAdapter_EventsWithoutASourceIsAnError(t *testing.T) {
	if _, err := (fakeharness.Adapter{}).Events(t.Context(), harness.Handle{}, nil); err == nil {
		t.Error("Events(nil) = nil error")
	}
}
