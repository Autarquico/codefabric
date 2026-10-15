package fakeharness

import (
	"bufio"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/Autarquico/codefabric/internal/support/atomicfile"
	"github.com/Autarquico/codefabric/internal/support/treedigest"
)

// The exit codes of the program besides 0 and those of exit steps.
const (
	// ExitScriptError is a script, a flag or a step that is wrong.
	ExitScriptError = 2
	// ExitTimeout is a wait_file or read_line step that timed out, as
	// timeout(1) exits.
	ExitTimeout = 124
)

// The environment of the program.
const (
	// AsFakeharnessEnv set to 1 makes [RunIfRequested] run the program.
	AsFakeharnessEnv = "CF_TEST_AS_FAKEHARNESS"
	// ScriptEnv is the path of the script, after --script.
	ScriptEnv = "FAKEHARNESS_SCRIPT"
	// ConfigDirEnv is the configuration directory: the script is
	// fakeharness.json in it, last, and record digests it.
	ConfigDirEnv = "FAKEHARNESS_CONFIG_DIR"
)

// The files of a configuration directory the program knows.
const (
	// ScriptFile is the script in a configuration directory.
	ScriptFile = "fakeharness.json"
	// AuthFile is the auth, the only symlink a configuration may have.
	AuthFile = "credentials.json"
)

// Version is what --version prints after "fakeharness ".
const Version = "1.0.0"

// pollInterval is how often wait_file looks for its file.
const pollInterval = 10 * time.Millisecond

// RunIfRequested runs the program and exits when AsFakeharnessEnv is 1, and
// returns otherwise. A TestMain calls it first, so that its test binary is
// the program when a test starts it with that variable.
func RunIfRequested() {
	if os.Getenv(AsFakeharnessEnv) != "1" {
		return
	}
	_ = os.Unsetenv(AsFakeharnessEnv)
	os.Exit(Main(os.Args, os.Environ(), os.Stdin, os.Stdout, os.Stderr))
}

// Main is the program: argv with its name first, and env as os.Environ. It
// returns the exit code.
func Main(argv, env []string, stdin io.Reader, stdout, stderr io.Writer) int {
	name := "fakeharness"
	if len(argv) > 0 {
		name, argv = argv[0], argv[1:]
	}
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	fs.SetOutput(stderr)
	scriptPath := fs.String("script", "", "path of the script")
	version := fs.Bool("version", false, "print the version and exit")
	// What the Adapter passes: recorded through argv, not acted on.
	fs.String("prompt", "", "the prompt of the Run")
	fs.String("model", "", "the model of the Run")
	fs.String("mode", "", "the mode of the Run")
	fs.String("resume", "", "the session to resume")
	if err := fs.Parse(argv); err != nil {
		return ExitScriptError
	}
	if fs.NArg() > 0 {
		_, _ = fmt.Fprintf(stderr, "fakeharness: unexpected arguments %q\n", fs.Args())
		return ExitScriptError
	}
	if *version {
		_, _ = fmt.Fprintln(stdout, "fakeharness "+Version)
		return 0
	}
	script, err := loadScript(*scriptPath, env)
	if err != nil {
		_, _ = fmt.Fprintln(stderr, err)
		return ExitScriptError
	}
	r := &runner{argv: append([]string{name}, argv...), env: env, stdin: bufio.NewReader(stdin), stdout: stdout}
	for i, st := range script.Steps {
		if code, done := r.step(st); done {
			if code != 0 && st.Exit == nil {
				_, _ = fmt.Fprintf(stderr, "fakeharness: step %d: %v\n", i, r.err)
			}
			return code
		}
	}
	return 0
}

// loadScript finds the script by the precedence of the package doc.
func loadScript(flagPath string, env []string) (Script, error) {
	path := flagPath
	if path == "" {
		path = getenv(env, ScriptEnv)
	}
	if path == "" {
		if dir := getenv(env, ConfigDirEnv); dir != "" {
			path = filepath.Join(dir, ScriptFile)
		}
	}
	if path == "" {
		return Script{}, fmt.Errorf("fakeharness: no script: give --script, %s or %s", ScriptEnv, ConfigDirEnv)
	}
	b, err := os.ReadFile(path) //nolint:gosec // the script the test points to
	if err != nil {
		return Script{}, fmt.Errorf("fakeharness: %w", err)
	}
	return ParseScript(b)
}

// getenv is the value of key in env, the last one when it repeats.
func getenv(env []string, key string) string {
	v := ""
	for _, kv := range env {
		if k, val, ok := strings.Cut(kv, "="); ok && k == key {
			v = val
		}
	}
	return v
}

type runner struct {
	argv   []string
	env    []string
	stdin  *bufio.Reader
	stdout io.Writer
	err    error
}

var errTimeout = errors.New("timed out")

// step runs st, and says whether the program ends with code.
func (r *runner) step(st Step) (code int, done bool) {
	var err error
	switch {
	case st.Emit != nil:
		err = r.emit(*st.Emit)
	case st.Record != nil:
		err = r.record(*st.Record)
	case st.WaitFile != nil:
		err = waitFile(*st.WaitFile)
	case st.ReadLine != nil:
		err = r.readLine(*st.ReadLine)
	case st.IgnoreSignals:
		signal.Ignore(syscall.SIGTERM, syscall.SIGINT, syscall.SIGHUP)
	case st.Exit != nil:
		return *st.Exit, true
	}
	if err == nil {
		return 0, false
	}
	r.err = err
	if errors.Is(err, errTimeout) {
		return ExitTimeout, true
	}
	return ExitScriptError, true
}

func (r *runner) emit(l Line) error {
	b, err := json.Marshal(l)
	if err != nil {
		return err
	}
	_, err = r.stdout.Write(append(b, '\n'))
	return err
}

func (r *runner) record(rec Recorder) error {
	keep := rec.Env
	if len(keep) == 0 {
		keep = DefaultRecordEnv
	}
	out := Record{Argv: r.argv, Env: map[string]string{}}
	for _, kv := range r.env {
		if k, v, ok := strings.Cut(kv, "="); ok && matchesAny(k, keep) {
			out.Env[k] = v
		}
	}
	dir, err := os.Getwd()
	if err != nil {
		return err
	}
	out.Dir = dir
	if cfg := getenv(r.env, ConfigDirEnv); cfg != "" {
		if out.ConfigDigest, err = treedigest.Dir(cfg, treedigest.Options{ApprovedLinks: []string{AuthFile}}); err != nil {
			return fmt.Errorf("record: %w", err)
		}
	}
	b, err := json.MarshalIndent(out, "", "  ")
	if err != nil {
		return err
	}
	return atomicfile.Write(rec.Path, b, 0o600)
}

func matchesAny(name string, patterns []string) bool {
	for _, p := range patterns {
		prefix, isPrefix := strings.CutSuffix(p, "*")
		if isPrefix && strings.HasPrefix(name, prefix) || !isPrefix && p == name {
			return true
		}
	}
	return false
}

func waitFile(w WaitFile) error {
	deadline := time.Now().Add(time.Duration(w.TimeoutMS) * time.Millisecond)
	for {
		_, err := os.Stat(w.Path)
		if err == nil {
			return nil
		}
		if !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("wait_file: %w", err)
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("wait_file %s: %w after %dms", w.Path, errTimeout, w.TimeoutMS)
		}
		time.Sleep(pollInterval)
	}
}

func (r *runner) readLine(rl ReadLine) error {
	type read struct {
		line string
		err  error
	}
	got := make(chan read, 1)
	go func() {
		line, err := r.stdin.ReadString('\n')
		if err == io.EOF && line != "" {
			err = nil
		}
		got <- read{strings.TrimSuffix(strings.TrimSuffix(line, "\n"), "\r"), err}
	}()
	timer := time.NewTimer(time.Duration(rl.TimeoutMS) * time.Millisecond)
	defer timer.Stop()
	select {
	case rd := <-got:
		if rd.err != nil {
			return fmt.Errorf("read_line: %w", rd.err)
		}
		if rl.To == "" {
			return nil
		}
		return atomicfile.Write(rl.To, []byte(rd.line), 0o600)
	case <-timer.C:
		return fmt.Errorf("read_line: %w after %dms", errTimeout, rl.TimeoutMS)
	}
}
