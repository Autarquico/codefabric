package fakeharness

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/Autarquico/codefabric/internal/adapters/harness"
)

// DefaultID is the ID of an Adapter with no Name.
const DefaultID = "fake"

// CheckpointFormat is the Format of the checkpoints of the Adapter: Data is
// the session ID to resume.
const CheckpointFormat = "fakeharness/session-v1"

// InputFile is the file in the RunDir that SendInput appends to; a script
// waits for it with wait_file.
const InputFile = "fakeharness.input"

// maxLine is the longest line Events and CollectUsage read; a longer one
// ends the stream.
const maxLine = 1 << 20

// Adapter is the harness port over the program of this package. What Caps
// says false is unsupported, by [harness.Unsupported]:
//
//   - Modes: Start and Resume with any other mode.
//   - Resume: Resume. NativeCheckpoint: Checkpoint.
//   - StructuredInput: SendInput. NeedsInputSignal: input_required events.
//   - UsageEvents: CollectUsage and usage events.
//   - ConfigIsolation: ApplyConfig to a TargetRun, and RunSpec.ConfigDir.
//   - ModelSelection: --model.
//
// Install and Stop are always unsupported: there is nothing to install, and
// the process group is the only way to stop it.
type Adapter struct {
	harness.Unsupported
	// Name is the ID; empty is DefaultID.
	Name string
	Caps harness.Capabilities
	// Binary is the program; empty is "fakeharness" in the PATH of the Host.
	Binary string
	// Env is added to every Launch: CF_TEST_AS_FAKEHARNESS=1 when Binary is
	// a test binary.
	Env []string
	// Script, when set, is passed as --script, over what the configuration
	// directory has.
	Script string
}

var _ harness.Adapter = Adapter{}

// ID is Name, or DefaultID.
func (a Adapter) ID() string {
	if a.Name == "" {
		return DefaultID
	}
	return a.Name
}

// Capabilities is Caps.
func (a Adapter) Capabilities() harness.Capabilities { return a.Caps }

func (a Adapter) unsupported(op string) error {
	return &harness.UnsupportedError{Harness: a.ID(), Op: op}
}

// Detect finds Binary, or fakeharness in the PATH.
func (a Adapter) Detect(_ context.Context, host harness.Host) (harness.Detection, error) {
	if a.Binary == "" {
		path, err := host.LookPath("fakeharness")
		if err != nil {
			return harness.Detection{}, nil //nolint:nilerr // not installed is not an error of detection
		}
		return harness.Detection{Installed: true, Path: path}, nil
	}
	fi, err := os.Stat(a.Binary)
	switch {
	case errors.Is(err, os.ErrNotExist):
		return harness.Detection{}, nil
	case err != nil:
		return harness.Detection{}, fmt.Errorf("fakeharness: detect: %w", err)
	case fi.IsDir() || fi.Mode().Perm()&0o111 == 0:
		return harness.Detection{}, nil
	}
	return harness.Detection{Installed: true, Path: a.Binary}, nil
}

// Version runs the program with --version.
func (a Adapter) Version(ctx context.Context, host harness.Host) (string, error) {
	d, err := a.Detect(ctx, host)
	if err != nil {
		return "", err
	}
	if !d.Installed {
		return "", errors.New("fakeharness: not installed")
	}
	res, err := host.Run(ctx, harness.Cmd{Argv: []string{d.Path, "--version"}, Env: a.Env})
	if err != nil {
		return "", fmt.Errorf("fakeharness: version: %w", err)
	}
	v, ok := strings.CutPrefix(strings.TrimSpace(string(res.Stdout)), "fakeharness ")
	if res.ExitCode != 0 || !ok {
		return "", fmt.Errorf("fakeharness: version: exit %d, %q", res.ExitCode, res.Stdout)
	}
	return v, nil
}

// Start builds the command of run; it writes nothing.
func (a Adapter) Start(_ context.Context, host harness.Host, run harness.RunSpec) (harness.Launch, error) {
	return a.launch(host, run, "")
}

// Resume is Start with --resume and the session of cp.
func (a Adapter) Resume(_ context.Context, host harness.Host, run harness.RunSpec, cp harness.Checkpoint) (harness.Launch, error) {
	if !a.Caps.Resume {
		return harness.Launch{}, a.unsupported("resume")
	}
	if cp.Harness != a.ID() || cp.Format != CheckpointFormat || len(cp.Data) == 0 {
		return harness.Launch{}, fmt.Errorf("fakeharness: resume: not a checkpoint of %s: %s %q", a.ID(), cp.Harness, cp.Format)
	}
	return a.launch(host, run, string(cp.Data))
}

func (a Adapter) launch(host harness.Host, run harness.RunSpec, session string) (harness.Launch, error) {
	if err := a.Caps.CheckMode(run.Mode); err != nil {
		return harness.Launch{}, err
	}
	bin := a.Binary
	if bin == "" {
		path, err := host.LookPath("fakeharness")
		if err != nil {
			return harness.Launch{}, fmt.Errorf("fakeharness: start: %w", err)
		}
		bin = path
	}
	argv := []string{bin, "--mode", string(run.Mode), "--prompt", run.Prompt}
	if a.Script != "" {
		argv = append(argv, "--script", a.Script)
	}
	if a.Caps.ModelSelection && run.Model != "" {
		argv = append(argv, "--model", run.Model)
	}
	if session != "" {
		argv = append(argv, "--resume", session)
	}
	configDir := globalDir(host)
	if a.Caps.ConfigIsolation && run.ConfigDir != "" {
		configDir = run.ConfigDir
	}
	env := append([]string{ConfigDirEnv + "=" + configDir}, a.Env...)
	return harness.Launch{Argv: argv, Env: env}, nil
}

// Checkpoint is the last session the Run started, from its events.
func (a Adapter) Checkpoint(_ context.Context, _ harness.Host, h harness.Handle) (harness.Checkpoint, error) {
	if !a.Caps.NativeCheckpoint {
		return harness.Checkpoint{}, a.unsupported("checkpoint")
	}
	session := ""
	err := eachEvent(eventsPath(h), func(ev harness.Event) {
		if ev.Kind == harness.EventSessionStarted && ev.SessionID != "" {
			session = ev.SessionID
		}
	})
	if err != nil {
		return harness.Checkpoint{}, err
	}
	if session == "" {
		return harness.Checkpoint{}, errors.New("fakeharness: checkpoint: the Run started no session")
	}
	return harness.Checkpoint{Harness: a.ID(), Format: CheckpointFormat, Data: []byte(session)}, nil
}

// SendInput appends the text and a newline to InputFile in the RunDir.
func (a Adapter) SendInput(_ context.Context, _ harness.Host, h harness.Handle, in harness.Input) error {
	if !a.Caps.StructuredInput {
		return a.unsupported("send input")
	}
	if h.RunDir == "" {
		return errors.New("fakeharness: send input: no RunDir")
	}
	f, err := os.OpenFile(filepath.Join(h.RunDir, InputFile), os.O_WRONLY|os.O_APPEND|os.O_CREATE, 0o600) //nolint:gosec // the RunDir of the handle
	if err != nil {
		return fmt.Errorf("fakeharness: send input: %w", err)
	}
	_, err = f.WriteString(in.Text + "\n")
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	return err
}

// Events parses the lines the program emits from src. When ctx is done it
// closes the channel without waiting for a read in progress, which ends when
// the caller closes src.
func (a Adapter) Events(ctx context.Context, _ harness.Handle, src io.ReadCloser) (<-chan harness.Event, error) {
	if src == nil {
		return nil, errors.New("fakeharness: events: no source")
	}
	lines := make(chan []byte)
	go func() {
		defer close(lines)
		sc := bufio.NewScanner(src)
		sc.Buffer(nil, maxLine)
		for sc.Scan() {
			select {
			case lines <- bytes.Clone(sc.Bytes()):
			case <-ctx.Done():
				return
			}
		}
	}()
	out := make(chan harness.Event)
	go func() {
		defer close(out)
		for {
			var line []byte
			select {
			case <-ctx.Done():
				return
			case l, ok := <-lines:
				if !ok {
					return
				}
				line = l
			}
			ev, ok := event(line)
			if !ok || !a.reports(ev.Kind) {
				continue
			}
			select {
			case out <- ev:
			case <-ctx.Done():
				return
			}
		}
	}()
	return out, nil
}

// reports tells whether the capabilities let an event of kind through.
func (a Adapter) reports(kind harness.EventKind) bool {
	switch kind {
	case harness.EventUsage:
		return a.Caps.UsageEvents
	case harness.EventInputRequired:
		return a.Caps.NeedsInputSignal
	}
	return true
}

// CollectUsage is the usage lines of the events of h, in order: an empty,
// non-nil slice when there are none.
func (a Adapter) CollectUsage(_ context.Context, _ harness.Host, h harness.Handle) ([]harness.UsageEvent, error) {
	if !a.Caps.UsageEvents {
		return nil, a.unsupported("collect usage")
	}
	usage := []harness.UsageEvent{}
	err := eachEvent(eventsPath(h), func(ev harness.Event) {
		if ev.Kind == harness.EventUsage {
			usage = append(usage, *ev.Usage)
		}
	})
	if err != nil {
		return nil, err
	}
	return usage, nil
}

// eventsPath is where the events of h are: its EventsPath, or its output.
func eventsPath(h harness.Handle) string {
	if h.EventsPath != "" {
		return h.EventsPath
	}
	return h.OutputPath
}

func eachEvent(path string, fn func(harness.Event)) error {
	if path == "" {
		return errors.New("fakeharness: the handle has no output")
	}
	f, err := os.Open(path) //nolint:gosec // the output of the handle
	if err != nil {
		return fmt.Errorf("fakeharness: %w", err)
	}
	defer func() { _ = f.Close() }()
	sc := bufio.NewScanner(f)
	sc.Buffer(nil, maxLine)
	for sc.Scan() {
		if ev, ok := event(sc.Bytes()); ok {
			fn(ev)
		}
	}
	if err := sc.Err(); err != nil {
		return fmt.Errorf("fakeharness: reading %s: %w", path, err)
	}
	return nil
}
