package attachwindow

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/Autarquico/codeflock/internal/domain"
)

// recorder is a Runner that keeps what it was asked and does what the window would.
type recorder struct {
	name   string
	args   []string
	err    error
	launch bool // run the command line the window was given, as its shell would
}

func (r *recorder) Run(ctx context.Context, name string, args ...string) error {
	r.name, r.args = name, args
	if r.err != nil || !r.launch {
		return r.err
	}
	var line []string
	if name == "tmux" {
		line = args[len(args)-2:] // /bin/sh <script>, as argv
	} else {
		line = []string{"/bin/sh", "-c", args[len(args)-1]} // the command line a terminal types into a shell
	}
	return exec.CommandContext(ctx, line[0], line[1:]...).Run() //nolint:gosec // the script the code under test wrote
}

func env(vars map[string]string) func(string) string {
	return func(k string) string { return vars[k] }
}

func TestDetect(t *testing.T) {
	for name, tc := range map[string]struct {
		vars map[string]string
		goos string
		want Launcher
		ok   bool
	}{
		"tmux wins over the terminal": {map[string]string{"TMUX": "/tmp/x,1,0", "TERM_PROGRAM": "iTerm.app"}, "darwin", Tmux, true},
		"tmux on linux":               {map[string]string{"TMUX": "/tmp/x,1,0"}, "linux", Tmux, true},
		"iTerm2":                      {map[string]string{"TERM_PROGRAM": "iTerm.app"}, "darwin", ITerm2, true},
		"Terminal.app":                {map[string]string{"TERM_PROGRAM": "Apple_Terminal"}, "darwin", Terminal, true},
		"iTerm2 variable off macOS":   {map[string]string{"TERM_PROGRAM": "iTerm.app"}, "linux", "", false},
		"another terminal":            {map[string]string{"TERM_PROGRAM": "ghostty"}, "darwin", "", false},
		"nothing":                     {nil, "darwin", "", false},
	} {
		got, ok := Detect(env(tc.vars), tc.goos)
		if got != tc.want || ok != tc.ok {
			t.Errorf("%s: Detect = %q, %v", name, got, ok)
		}
	}
}

// hostile are values a shell would act on.
var hostile = []string{"it's", "a;b", "$(touch pwned)", "`touch pwned`", "two\nlines", "trailing;", "", "-n", `back\slash "q"`, "*", "a b"}

// fakeCflk is a program that writes its arguments, NUL-terminated, to $OUT.
func fakeCflk(t *testing.T) (exe, out string) {
	t.Helper()
	dir := t.TempDir()
	exe, out = filepath.Join(dir, "cflk"), filepath.Join(dir, "argv")
	script := "#!/bin/sh\nprintf '%s\\0' \"$@\" > \"$OUT\"\nprintf '%s\\0' \"$CFLK_CONFIG_DIR\" > \"$OUT.env\"\n"
	if err := os.WriteFile(exe, []byte(script), 0o700); err != nil { //nolint:gosec // a test script
		t.Fatal(err)
	}
	t.Setenv("OUT", out)
	return exe, out
}

func recorded(t *testing.T, path string) []string {
	t.Helper()
	raw, err := os.ReadFile(path) //nolint:gosec // a file of the test
	if err != nil {
		t.Fatalf("the program did not run: %v", err)
	}
	return strings.Split(strings.TrimSuffix(string(raw), "\x00"), "\x00")
}

func TestScenario_AttachWindow_NadaSeInterpreta(t *testing.T) {
	for _, l := range []Launcher{Tmux, ITerm2, Terminal} {
		t.Run(string(l), func(t *testing.T) {
			exe, out := fakeCflk(t)
			work := t.TempDir()
			t.Chdir(work)
			args := append([]string{"attach"}, hostile...)
			argv := append([]string{"env", "CFLK_CONFIG_DIR=/cfg dir/it's", exe}, args...)
			run := &recorder{launch: true}
			if err := l.Open(t.Context(), run, argv, "run-1"); err != nil {
				t.Fatal(err)
			}
			if got := recorded(t, out); !slices.Equal(got, args) {
				t.Errorf("argv = %q, want %q", got, args)
			}
			if got := recorded(t, out+".env"); got[0] != "/cfg dir/it's" {
				t.Errorf("CFLK_CONFIG_DIR = %q", got)
			}
			if entries, _ := os.ReadDir(work); len(entries) != 0 {
				t.Errorf("something else ran and left %v", entries)
			}
			script := scriptOf(l, run)
			if _, err := os.Stat(script); !errors.Is(err, os.ErrNotExist) {
				t.Errorf("the script is still there: %v", err)
			}
		})
	}
}

// scriptOf is the script the launcher was told to run.
func scriptOf(l Launcher, run *recorder) string {
	if l == Tmux {
		return run.args[len(run.args)-1]
	}
	line := run.args[len(run.args)-1]
	return strings.Trim(line[strings.LastIndex(line, " ")+1:], "'")
}

func TestScenario_AttachWindow_ITerm2OTerminalApp(t *testing.T) {
	argv := []string{"/bin/cflk", "attach", "--", "run_1"}
	t.Run("tmux gets argv, no shell", func(t *testing.T) {
		run := &recorder{}
		if err := Tmux.Open(t.Context(), run, argv, "run_1"); err != nil {
			t.Fatal(err)
		}
		script := run.args[len(run.args)-1]
		t.Cleanup(func() { _ = os.RemoveAll(filepath.Dir(script)) })
		if want := []string{"new-window", "-n", "run_1", "/bin/sh", script}; run.name != "tmux" || !slices.Equal(run.args, want) {
			t.Errorf("%s %q", run.name, run.args)
		}
	})
	for l, app := range map[Launcher]string{ITerm2: "iTerm2", Terminal: "Terminal"} {
		t.Run(app+" gets the command as an argument of the script", func(t *testing.T) {
			run := &recorder{}
			if err := l.Open(t.Context(), run, argv, "run_1"); err != nil {
				t.Fatal(err)
			}
			script := scriptOf(l, run)
			t.Cleanup(func() { _ = os.RemoveAll(filepath.Dir(script)) })
			sep := slices.Index(run.args, "--")
			if run.name != "osascript" || sep != len(run.args)-2 {
				t.Fatalf("%s %q", run.name, run.args)
			}
			if want := "'/bin/sh' '" + script + "'"; run.args[sep+1] != want {
				t.Errorf("argument = %q, want %q", run.args[sep+1], want)
			}
			source := strings.Join(run.args[:sep], "\n")
			if !strings.Contains(source, `tell application "`+app+`"`) || strings.Contains(source, script) || strings.Contains(source, "run_1") {
				t.Errorf("source = %s", source)
			}
		})
	}
}

func TestOpen_ALauncherThatFailsLeavesNothingBehind(t *testing.T) {
	boom := errors.New("boom")
	tmp := t.TempDir()
	t.Setenv("TMPDIR", tmp)
	run := &recorder{err: boom}
	if err := Tmux.Open(t.Context(), run, []string{"/bin/cflk"}, "r"); !errors.Is(err, boom) {
		t.Fatalf("err = %v", err)
	}
	if entries, _ := os.ReadDir(tmp); len(entries) != 0 {
		t.Errorf("left %v", entries)
	}
}

func TestOpen_ANulIsRefusedBeforeAnythingIsWritten(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("TMPDIR", tmp)
	run := &recorder{}
	err := Tmux.Open(t.Context(), run, []string{"/bin/cflk", "a\x00b"}, "r")
	var coded *domain.Error
	if !errors.As(err, &coded) || coded.Code != domain.CodeInvalidRequest || run.name != "" {
		t.Fatalf("err = %v, launched %q", err, run.name)
	}
	if entries, _ := os.ReadDir(tmp); len(entries) != 0 {
		t.Errorf("left %v", entries)
	}
}

func TestLine(t *testing.T) {
	got, err := line([]string{"a b", "it's", "x\ny"})
	if want := "'a b' 'it'\\''s' 'x\ny'"; err != nil || got != want {
		t.Errorf("line = %q, %v, want %q", got, err, want)
	}
}

func TestOpen_ATmuxTitleIsNeverAFormat(t *testing.T) {
	run := &recorder{}
	if err := Tmux.Open(t.Context(), run, []string{"/bin/cflk"}, "#(touch pwned)#{pid}"); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(filepath.Dir(run.args[len(run.args)-1])) })
	if got, want := run.args[2], "##(touch pwned)##{pid}"; got != want {
		t.Errorf("title = %q, want %q", got, want)
	}
}

func TestExecRunner_AFailureCarriesTheStderrOfTheProgram(t *testing.T) {
	err := ExecRunner{}.Run(t.Context(), "/bin/sh", "-c", "echo 'Not authorized to send Apple events' >&2; exit 1")
	if err == nil || !strings.Contains(err.Error(), "Not authorized to send Apple events") || !strings.Contains(err.Error(), "exit status 1") {
		t.Errorf("err = %v", err)
	}
	err = ExecRunner{}.Run(t.Context(), "/bin/sh", "-c", "head -c 100000 /dev/zero | tr '\\0' x >&2; exit 1")
	if err == nil || len(err.Error()) > maxStderr+100 {
		t.Errorf("the error is not bounded: %d bytes", len(err.Error()))
	}
}
