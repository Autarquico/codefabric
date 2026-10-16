package harnesstest_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/Autarquico/codefabric/internal/adapters/harness"
	"github.com/Autarquico/codefabric/internal/adapters/harness/harnesstest"
	"github.com/Autarquico/codefabric/internal/domain"
	"github.com/Autarquico/codefabric/internal/testutil/fakeharness"
)

func TestMain(m *testing.M) {
	fakeharness.RunIfRequested()
	os.Exit(m.Run())
}

// everyCap is every capability the fakeharness has: all but Install and
// GracefulStop, which it never supports.
var everyCap = harness.Capabilities{
	Modes:  []domain.Mode{domain.ModeHeadless, domain.ModeInteractive},
	Resume: true, NativeCheckpoint: true, NeedsInputSignal: true, StructuredInput: true,
	UsageEvents: true, ModelSelection: true, ConfigIsolation: true,
}

// noCap is no capability at all, but the one mode a harness must have.
var noCap = harness.Capabilities{Modes: []domain.Mode{domain.ModeHeadless}}

const (
	credentialSentinel = "fake-credential-sentinel-7f3a" //nolint:gosec // a sentinel, not a credential
	session            = "s-1"
	model              = "m1"
	releaseFile        = "release"
)

// The secret the Launch of the fakeharness names; the smoke Run prints its
// value, so it fails unless it got it.
//
//nolint:gosec // sentinels, not credentials
const (
	secretRef   = "fake/run-key"
	secretValue = "fake-secret-value-91c2"
	secretName  = "FAKE_RUN_KEY"
)

func ptr(v int64) *int64 { return &v }

// The lines the fakeharness emits, in the smoke Run and in the recording.
var lines = []fakeharness.Line{
	{Type: harness.EventSessionStarted, SessionID: session, Model: model},
	{Type: harness.EventUsage, Usage: &fakeharness.Usage{Provider: "fake", Model: model, InputTokens: ptr(120), OutputTokens: ptr(0)}},
	{Type: harness.EventInputRequired, Message: "proceed?"},
	{Type: harness.EventResult, Result: &fakeharness.Result{Summary: "done"}},
}

var usage = harness.UsageEvent{Provider: "fake", Model: model, InputTokens: ptr(120), OutputTokens: ptr(0)}

func script(t *testing.T) []byte {
	t.Helper()
	var s fakeharness.Script
	s.Steps = append(s.Steps, fakeharness.Step{EmitEnv: &fakeharness.EmitEnv{Name: secretName}})
	for i := range lines {
		s.Steps = append(s.Steps, fakeharness.Step{Emit: &lines[i]})
	}
	s.Steps = append(s.Steps, fakeharness.Step{WaitFile: &fakeharness.WaitFile{Path: releaseFile, TimeoutMS: 60_000}})
	return mustJSON(t, s)
}

func mustJSON(t *testing.T, v any) []byte {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func write(t *testing.T, path string, content []byte, perm os.FileMode) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, content, perm); err != nil { //nolint:gosec // a path of the test
		t.Fatal(err)
	}
}

// fakeFixture is the fixture of a fakeharness with caps, whose binary is
// this test binary behind a shell script.
func fakeFixture(caps harness.Capabilities) harnesstest.Fixture {
	expect := []harness.Event{{Kind: harness.EventSessionStarted, SessionID: session, Model: model}}
	if caps.UsageEvents {
		expect = append(expect, harness.Event{Kind: harness.EventUsage, Usage: &usage})
	}
	if caps.NeedsInputSignal {
		expect = append(expect, harness.Event{Kind: harness.EventInputRequired, Message: "proceed?"})
	}
	expect = append(expect, harness.Event{Kind: harness.EventResult, Result: &harness.Result{Summary: "done"}})
	return harnesstest.Fixture{
		New: func(binDir string) harness.Adapter {
			return fakeharness.Adapter{
				Caps: caps, Binary: filepath.Join(binDir, "fakeharness"),
				SecretEnv: []harness.SecretEnv{{Name: secretName, Ref: secretRef}},
			}
		},
		Install: func(t *testing.T, binDir string) {
			self, err := filepath.Abs(os.Args[0])
			if err != nil {
				t.Fatal(err)
			}
			wrapper := "#!/bin/sh\n" + fakeharness.AsFakeharnessEnv + "=1 exec '" + self + "' \"$@\"\n"
			write(t, filepath.Join(binDir, "fakeharness"), []byte(wrapper), 0o755)
		},
		SeedHome: func(t *testing.T, home string) {
			dir := filepath.Join(home, fakeharness.ConfigDirName)
			settings := `{"model":"m1","theme":"dark","api_key":"` + harness.SecretMarker("fake/api-key") + `"}`
			write(t, filepath.Join(dir, fakeharness.SettingsFile), []byte(settings), 0o600)
			write(t, filepath.Join(dir, fakeharness.ScriptFile), script(t), 0o600)
			write(t, filepath.Join(dir, "skills", "review", "SKILL.md"), []byte("# Review\n"), 0o644)
			write(t, filepath.Join(dir, fakeharness.AuthFile), []byte(`{"token":"`+credentialSentinel+`"}`), 0o600)
		},
		Secrets:         []string{credentialSentinel},
		SecretValues:    map[string]string{secretRef: secretValue},
		CredentialFiles: []string{filepath.Join(fakeharness.ConfigDirName, fakeharness.AuthFile)},
		Mutate: func(t *testing.T, home string) string {
			write(t, filepath.Join(home, fakeharness.ConfigDirName, "skills", "review", "SKILL.md"), []byte("# Review, edited\n"), 0o644)
			return fakeharness.SkillsPrefix + "review"
		},
		PrepareRun: func(t *testing.T, run *harness.RunSpec, hold bool) {
			run.Prompt = "smoke"
			if !hold {
				write(t, filepath.Join(run.Worktree, releaseFile), nil, 0o600)
			}
		},
		ExpectExit:   0,
		ExpectOutput: `"summary":"done"`,
		Recorded: func(t *testing.T, h harness.Handle) {
			out := []byte("starting up\n")
			for _, l := range lines {
				out = append(append(out, mustJSON(t, l)...), '\n')
				out = append(out, "{half a line\n"...)
			}
			write(t, h.OutputPath, out, 0o600)
		},
		ExpectEvents: expect,
		ExpectUsage:  []harness.UsageEvent{usage},
		ResumeFrom:   []harness.Checkpoint{{Harness: fakeharness.DefaultID, Format: fakeharness.CheckpointFormat, Data: []byte("another-session")}},
	}
}

func TestScenario_HarnessAdapterFramework_Contrato(t *testing.T) {
	t.Run("every capability", func(t *testing.T) { harnesstest.Run(t, fakeFixture(everyCap)) })
	t.Run("no capability", func(t *testing.T) { harnesstest.Run(t, fakeFixture(noCap)) })
}
