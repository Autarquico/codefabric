package harnesstest_test

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/Autarquico/codefabric/internal/adapters/harness"
	"github.com/Autarquico/codefabric/internal/adapters/harness/harnesstest"
	"github.com/Autarquico/codefabric/internal/adapters/harness/localhost"
	"github.com/Autarquico/codefabric/internal/domain"
	"github.com/Autarquico/codefabric/internal/testutil/fakeharness"
)

// brokenEnv names the broken adapter TestContract_BrokenAdapter runs the
// suite against, in the subprocess TestContract_FailsABrokenAdapter starts.
const brokenEnv = "HARNESSTEST_BROKEN"

// broken are adapters each broken in one way, and the check of the suite
// that must catch it.
var broken = []struct {
	name, check string
	fixture     func() harnesstest.Fixture
}{
	{"declares Install it does not support", "install", func() harnesstest.Fixture {
		caps := everyCap
		caps.Install = true
		return fakeFixture(caps)
	}},
	{"declares Resume it does not support", "resume", func() harnesstest.Fixture {
		return wrap(everyCap, func(a fakeharness.Adapter) harness.Adapter { return noResume{a} })
	}},
	{"resumes ignoring the checkpoint", "resume", func() harnesstest.Fixture {
		return wrap(everyCap, func(a fakeharness.Adapter) harness.Adapter { return constantResume{a} })
	}},
	{"declares GracefulStop it does not support, interactive only", "stop", func() harnesstest.Fixture {
		caps := noCap
		caps.Modes = []domain.Mode{domain.ModeInteractive}
		caps.GracefulStop = true
		return fakeFixture(caps)
	}},
	{"reads unknown usage as zero", "usage", func() harnesstest.Fixture {
		return wrap(everyCap, func(a fakeharness.Adapter) harness.Adapter { return zeroUsage{a} })
	}},
	{"leaks a credential into the snapshot", "snapshot", func() harnesstest.Fixture {
		return wrap(everyCap, func(a fakeharness.Adapter) harness.Adapter { return leakySnapshot{a} })
	}},
	{"builds a different Start each time", "start", func() harnesstest.Fixture {
		return wrap(noCap, func(a fakeharness.Adapter) harness.Adapter { return &nonce{Adapter: a} })
	}},
	{"writes in HOME on Start", "start", func() harnesstest.Fixture {
		return wrap(noCap, func(a fakeharness.Adapter) harness.Adapter { return homeWriter{a} })
	}},
	{"keeps sending after its ctx ends", "events-cancel", func() harnesstest.Fixture {
		return wrap(noCap, func(a fakeharness.Adapter) harness.Adapter { return deafEvents{a} })
	}},
	{"puts the value of a secret in the Launch", "start", func() harnesstest.Fixture {
		return wrap(noCap, func(a fakeharness.Adapter) harness.Adapter { return resolvedSecret{a} })
	}},
	{"gives a secret to a variable of the Worker", "start", func() harnesstest.Fixture {
		return wrap(noCap, func(a fakeharness.Adapter) harness.Adapter {
			a.SecretEnv = []harness.SecretEnv{{Name: "PATH", Ref: secretRef}}
			return a
		})
	}},
	{"names a secret the Run does not resolve", "run", func() harnesstest.Fixture {
		return wrap(noCap, func(a fakeharness.Adapter) harness.Adapter {
			a.SecretEnv = append(a.SecretEnv, harness.SecretEnv{Name: "OTHER_KEY", Ref: "fake/unknown"})
			return a
		})
	}},
	{"does not pass its secret to the harness", "run", func() harnesstest.Fixture {
		return wrap(noCap, func(a fakeharness.Adapter) harness.Adapter {
			a.SecretEnv = nil
			return a
		})
	}},
}

// resolvedSecret puts the value of its secret in the environment of the
// Launch, besides naming its ref.
type resolvedSecret struct{ fakeharness.Adapter }

func (a resolvedSecret) Start(ctx context.Context, host harness.Host, run harness.RunSpec) (harness.Launch, error) {
	l, err := a.Adapter.Start(ctx, host, run)
	l.Env = append(l.Env, secretName+"="+secretValue)
	return l, err
}

// wrap is the fixture of the fakeharness with caps, its adapter broken by b.
func wrap(caps harness.Capabilities, b func(fakeharness.Adapter) harness.Adapter) harnesstest.Fixture {
	f := fakeFixture(caps)
	newFake := f.New
	f.New = func(binDir string) harness.Adapter { return b(newFake(binDir).(fakeharness.Adapter)) }
	return f
}

type noResume struct{ fakeharness.Adapter }

func (noResume) Resume(context.Context, harness.Host, harness.RunSpec, harness.Checkpoint) (harness.Launch, error) {
	return harness.Launch{}, harness.NewUnsupportedError("resume")
}

// constantResume resumes with a fixed argument instead of the checkpoint.
type constantResume struct{ fakeharness.Adapter }

func (a constantResume) Resume(ctx context.Context, host harness.Host, run harness.RunSpec, _ harness.Checkpoint) (harness.Launch, error) {
	l, err := a.Start(ctx, host, run)
	l.Argv = append(l.Argv, "--resume", "last")
	return l, err
}

type zeroUsage struct{ fakeharness.Adapter }

func (a zeroUsage) CollectUsage(ctx context.Context, host harness.Host, h harness.Handle) ([]harness.UsageEvent, error) {
	us, err := a.Adapter.CollectUsage(ctx, host, h)
	for i := range us {
		for _, p := range []**int64{&us[i].InputTokens, &us[i].OutputTokens, &us[i].CacheReadTokens, &us[i].CacheWriteTokens, &us[i].RequestCount, &us[i].LatencyMS} {
			if *p == nil {
				*p = new(int64)
			}
		}
	}
	return us, err
}

// leakySnapshot copies the credentials into the first file of the snapshot,
// keeping its digest right so that only the leak gives it away.
type leakySnapshot struct{ fakeharness.Adapter }

func (a leakySnapshot) SnapshotConfig(ctx context.Context, host harness.Host, opts harness.SnapshotOptions) (harness.NativeSnapshot, error) {
	s, err := a.Adapter.SnapshotConfig(ctx, host, opts)
	if err != nil || len(s.Components) == 0 {
		return s, err
	}
	creds, err := os.ReadFile(filepath.Join(host.Home(), fakeharness.ConfigDirName, fakeharness.AuthFile))
	if err != nil {
		return harness.NativeSnapshot{}, err
	}
	c := s.Components[0]
	c.Files[0].Content = append(bytes.Clone(c.Files[0].Content), creds...)
	c.Digest, err = harness.FilesDigest(c.Files)
	s.Components[0] = c
	return s, err
}

type nonce struct {
	fakeharness.Adapter
	n atomic.Int64
}

func (a *nonce) Start(ctx context.Context, host harness.Host, run harness.RunSpec) (harness.Launch, error) {
	l, err := a.Adapter.Start(ctx, host, run)
	l.Env = append(l.Env, "FAKEHARNESS_NONCE="+strconv.FormatInt(a.n.Add(1), 10))
	return l, err
}

type homeWriter struct{ fakeharness.Adapter }

func (a homeWriter) Start(ctx context.Context, host harness.Host, run harness.RunSpec) (harness.Launch, error) {
	if err := os.WriteFile(filepath.Join(host.Home(), ".fakeharness-last-start"), []byte(run.RunID), 0o600); err != nil {
		return harness.Launch{}, err
	}
	return a.Adapter.Start(ctx, host, run)
}

// deafEvents forwards the events of the fakeharness without looking at ctx:
// a send nobody receives blocks it for ever.
type deafEvents struct{ fakeharness.Adapter }

func (a deafEvents) Events(ctx context.Context, h harness.Handle, src io.ReadCloser) (<-chan harness.Event, error) {
	in, err := a.Adapter.Events(context.WithoutCancel(ctx), h, src)
	if err != nil {
		return nil, err
	}
	out := make(chan harness.Event)
	go func() {
		defer close(out)
		for ev := range in {
			out <- ev
		}
	}()
	return out, nil
}

// TestContract_BrokenAdapter runs the suite against the adapter brokenEnv
// names; it is the subprocess of TestContract_FailsABrokenAdapter.
func TestContract_BrokenAdapter(t *testing.T) {
	name := os.Getenv(brokenEnv)
	if name == "" {
		t.Skip("run by TestContract_FailsABrokenAdapter")
	}
	for _, b := range broken {
		if b.name == name {
			harnesstest.Run(t, b.fixture())
			return
		}
	}
	t.Fatalf("no broken adapter %q", name)
}

func TestContract_FailsABrokenAdapter(t *testing.T) {
	for _, b := range broken {
		t.Run(b.name, func(t *testing.T) {
			t.Parallel()
			cmd := exec.CommandContext(t.Context(), os.Args[0], "-test.count=1", //nolint:gosec // this test binary
				"-test.run=^TestContract_BrokenAdapter$/^"+b.check+"$")
			cmd.Env = append(os.Environ(), brokenEnv+"="+b.name)
			out, err := cmd.CombinedOutput()
			var exit *exec.ExitError
			if !errors.As(err, &exit) {
				t.Fatalf("the suite passed an adapter that %s (%v):\n%s", b.name, err, out)
			}
			if want := "--- FAIL: TestContract_BrokenAdapter/" + b.check; !strings.Contains(string(out), want) {
				t.Errorf("the check %s did not catch an adapter that %s:\n%s", b.check, b.name, out)
			}
		})
	}
}

func TestScenario_HarnessAdapterFramework_CapacidadAusente(t *testing.T) {
	ctx := t.Context()
	host, err := localhost.New(localhost.Options{Home: t.TempDir(), Path: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		name string
		off  func(*harness.Capabilities)
		call func(a harness.Adapter, run harness.RunSpec) (any, error)
	}{
		{"Resume", func(c *harness.Capabilities) { c.Resume = false }, func(a harness.Adapter, run harness.RunSpec) (any, error) {
			cp := harness.Checkpoint{Harness: a.ID(), Format: fakeharness.CheckpointFormat, Data: []byte(session)}
			return a.Resume(ctx, host, run, cp)
		}},
		{"Checkpoint", func(c *harness.Capabilities) { c.NativeCheckpoint = false }, func(a harness.Adapter, run harness.RunSpec) (any, error) {
			return a.Checkpoint(ctx, host, handle(t, run))
		}},
		{"CollectUsage", func(c *harness.Capabilities) { c.UsageEvents = false }, func(a harness.Adapter, run harness.RunSpec) (any, error) {
			return a.CollectUsage(ctx, host, handle(t, run))
		}},
	} {
		name := c.name
		t.Run(name, func(t *testing.T) {
			caps := everyCap
			c.off(&caps)
			a := fakeharness.Adapter{Caps: caps}
			run := harness.RunSpec{RunID: "run-1", LeaseEpoch: 1, Mode: caps.Modes[0], Prompt: "p", RunDir: t.TempDir(), Worktree: t.TempDir()}

			got, err := c.call(a, run)

			if !errors.Is(err, harness.ErrUnsupported) {
				t.Errorf("%s with the capability off: err = %v, want ErrUnsupported", name, err)
			}
			if !reflect.ValueOf(got).IsZero() {
				t.Errorf("%s with the capability off returned %+v, want the zero value, never made-up data", name, got)
			}
			if entries, _ := os.ReadDir(run.RunDir); len(entries) > 0 {
				t.Errorf("%s with the capability off left %v in the RunDir", name, entries)
			}
		})
	}
}

// handle is the handle of run, whose output, outside its RunDir, has a
// session and its usage: real data an unsupported call must not return.
func handle(t *testing.T, run harness.RunSpec) harness.Handle {
	t.Helper()
	out := filepath.Join(t.TempDir(), "output.log")
	var b []byte
	for _, l := range lines {
		b = append(append(b, mustJSON(t, l)...), '\n')
	}
	write(t, out, b, 0o600)
	return harness.Handle{RunID: run.RunID, LeaseEpoch: run.LeaseEpoch, Mode: run.Mode, RunDir: run.RunDir, Worktree: run.Worktree, OutputPath: out}
}
