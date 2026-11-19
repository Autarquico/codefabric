package tmux

import (
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	cfruntime "github.com/Autarquico/codeflock/internal/adapters/runtime"
	"github.com/Autarquico/codeflock/internal/domain"
)

const (
	runID    = "run_01J8ZQ4M5N6P7R8S9T0V1W2X3Y"
	otherRun = "run_01J8ZQ4M5N6P7R8S9T0V1W2X4Z"
	bin      = "/fake/bin/tmux"
)

func handleOf(socket, session string) cfruntime.Handle {
	return cfruntime.Handle{Runtime: Name, Ref: ref(socket, session)}
}

func TestAttachArgv_IsTheSessionOfThatRunAndEpoch(t *testing.T) {
	h := handleOf("cf-0123456789ab", domain.SessionName(runID, 3))
	got, err := AttachArgv(h, bin, runID, 3, false)
	want := []string{bin, "-L", "cf-0123456789ab", "-f", "/dev/null", "attach-session", "-t", "=" + domain.SessionName(runID, 3)}
	if err != nil || !slices.Equal(got, want) {
		t.Fatalf("argv = %q, %v; want %q", got, err, want)
	}
	if ro, err := AttachArgv(h, bin, runID, 3, true); err != nil || ro[len(ro)-1] != "-r" || len(ro) != len(want)+1 {
		t.Fatalf("read-only = %q, %v", ro, err)
	}
}

func TestAttachArgv_RefusesWhatIsNotTheSessionOfTheRun(t *testing.T) {
	good := handleOf("cf-0123456789ab", domain.SessionName(runID, 3))
	for name, tc := range map[string]struct {
		h      cfruntime.Handle
		bin    string
		run    string
		epoch  uint64
		reason string
	}{
		"another Run":                            {good, bin, otherRun, 3, ""},
		"another epoch":                          {good, bin, runID, 4, ""},
		"another runtime":                        {cfruntime.Handle{Runtime: "zellij", Ref: good.Ref}, bin, runID, 3, ""},
		"a session of the Worker's, not a Run's": {handleOf("cf-0123456789ab", "work"), bin, runID, 3, ""},
		"a session with a prefix of the name":    {handleOf("cf-0123456789ab", domain.SessionName(runID, 3)+"x"), bin, runID, 3, ""},
		"ref without a socket":                   {cfruntime.Handle{Runtime: Name, Ref: domain.SessionName(runID, 3)}, bin, runID, 3, ""},
		"ref with an option as socket":           {handleOf("-S", domain.SessionName(runID, 3)), bin, runID, 3, ""},
		"ref with a path":                        {cfruntime.Handle{Runtime: Name, Ref: "/tmp/x/" + domain.SessionName(runID, 3)}, bin, runID, 3, ""},
		"relative binary":                        {good, "tmux", runID, 3, ""},
		"binary with a space":                    {good, "/usr/bin/tm ux", runID, 3, ""},
		"binary with .. ":                        {good, "/usr/../bin/tmux", runID, 3, ""},
		"binary with a quote":                    {good, "/usr/bin/tmux'", runID, 3, ""},
		"a binary not called tmux":               {good, "/bin/sh", runID, 3, ""},
		"a binary that ends like tmux":           {good, "/usr/bin/mytmux", runID, 3, ""},
		"no binary":                              {good, "", runID, 3, ""},
	} {
		if argv, err := AttachArgv(tc.h, tc.bin, tc.run, tc.epoch, false); !errors.Is(err, ErrNotAttachable) || argv != nil {
			t.Errorf("%s: %q, %v", name, argv, err)
		}
	}
}

func TestResolveBinary_OnlyAPlainExecutableRegularFileAtAnAcceptablePath(t *testing.T) {
	dir := t.TempDir()
	exe := filepath.Join(dir, "tmux")
	if err := os.WriteFile(exe, []byte("#!/bin/sh\n"), 0o700); err != nil { //nolint:gosec // a test script
		t.Fatal(err)
	}
	plain := filepath.Join(dir, "notexec")
	if err := os.WriteFile(plain, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	spaced := filepath.Join(dir, "with space")
	if err := os.WriteFile(spaced, []byte("#!/bin/sh\n"), 0o700); err != nil { //nolint:gosec // a test script
		t.Fatal(err)
	}
	for binary, want := range map[string]string{exe: exe, plain: "", spaced: "", "/nonexistent/tmux": "", dir: ""} {
		r, err := New(Options{Socket: "cf-test", Binary: binary})
		if err != nil {
			t.Fatal(err)
		}
		if name, _ := r.AttachBinary(); name != "" {
			t.Fatalf("%s announces a binary before Check", binary)
		}
		r.resolveBinary()
		name, path := r.AttachBinary()
		if path != want || (want != "" && name != Name) {
			t.Errorf("%s: %q %q, want %q", binary, name, path, want)
		}
		if strings.Contains(path, " ") {
			t.Errorf("path %q", path)
		}
	}
}

func TestAttachCommand_IsTheSameClientAsSSHRunsOnThisServer(t *testing.T) {
	r, err := New(Options{Socket: "cf-0123456789ab", TmpDir: "/srv/tmux", OwnTmpDir: true})
	if err != nil {
		t.Fatal(err)
	}
	h := r.HandleFor(domain.SessionName(runID, 3))
	if _, err := r.AttachCommand(h, false); !errors.Is(err, ErrNotAttachable) {
		t.Fatalf("before Check found tmux: %v", err)
	}
	r.attach = bin
	// The keys of the server are locked first; a tmux that does nothing stands for it.
	log := filepath.Join(t.TempDir(), "calls")
	fake := filepath.Join(t.TempDir(), "tmux")
	if err := os.WriteFile(fake, []byte("#!/bin/sh\necho \"$*\" >> "+log+"\n"), 0o700); err != nil { //nolint:gosec // a script of the test
		t.Fatal(err)
	}
	r.binary = fake
	cmd, err := r.AttachCommand(h, false)
	calls, _ := os.ReadFile(log) //nolint:gosec // a file of the test
	for _, want := range []string{"set-option -g prefix None", "set-option -g prefix2 None", "unbind-key -a -T root", "bind-key -n C-] detach-client"} {
		if !strings.Contains(string(calls), want) {
			t.Errorf("AttachCommand did not run %q; ran:\n%s", want, calls)
		}
	}
	want := []string{"-L", "cf-0123456789ab", "-f", "/dev/null", "attach-session", "-t", "=" + domain.SessionName(runID, 3)}
	if err != nil || cmd.Path != bin || !slices.Equal(cmd.Args, want) || !slices.Contains(cmd.Env, "TMUX_TMPDIR=/srv/tmux") {
		t.Fatalf("command = %+v, %v", cmd, err)
	}
	ro, _ := r.AttachCommand(h, true)
	if got := ro.Args[len(ro.Args)-1]; got != "-r" {
		t.Errorf("a read-only client ends in %q", got)
	}
	// The same arguments the ssh path builds.
	viaSSH, _ := AttachArgv(h, bin, runID, 3, true)
	if !slices.Equal(append([]string{ro.Path}, ro.Args...), viaSSH) {
		t.Errorf("relay %q, ssh %q", ro.Args, viaSSH)
	}
	// A handle of another server, or of another runtime, is not attached.
	for _, bad := range []cfruntime.Handle{handleOf("cf-other", domain.SessionName(runID, 3)), {Runtime: "docker", Ref: "x"}} {
		if _, err := r.AttachCommand(bad, false); !errors.Is(err, ErrNotAttachable) {
			t.Errorf("%+v: %v", bad, err)
		}
	}
}

func TestSnapshotArgs_ReadTheExactSessionKeepingItsAttributes(t *testing.T) {
	got := snapshotArgs(domain.SessionName(runID, 3))
	want := []string{"capture-pane", "-p", "-e", "-t", "=" + domain.SessionName(runID, 3) + ":"}
	if !slices.Equal(got, want) {
		t.Fatalf("args = %q, want %q", got, want)
	}
	// The command runs behind the runtime's base: the Worker's server, no config.
	r := &Runtime{socket: "cf-0123456789ab"}
	if full := append(r.base(), got...); !slices.Equal(full[:4], []string{"-L", "cf-0123456789ab", "-f", "/dev/null"}) {
		t.Fatalf("full = %q", full)
	}
}

func TestSnapshot_RefusesAHandleOfAnotherRuntime(t *testing.T) {
	r := &Runtime{socket: "cf-0123456789ab"}
	if _, err := r.Snapshot(t.Context(), cfruntime.Handle{Runtime: "other", Ref: "x"}); err == nil {
		t.Fatal("a handle of another runtime was captured")
	}
}

func TestViewIgnoresSize_IsKnownFromTheVersionCheckFound(t *testing.T) {
	for version, want := range map[string]bool{"3.0": false, "3.1c": false, "3.2": true, "3.6a": true, "4.0": true} {
		bin := filepath.Join(t.TempDir(), "tmux")
		if err := os.WriteFile(bin, []byte("#!/bin/sh\necho tmux "+version+"\n"), 0o755); err != nil { //nolint:gosec // a script of the test
			t.Fatal(err)
		}
		r, err := New(Options{Socket: "cf-0123456789ab", Binary: bin})
		if err != nil {
			t.Fatal(err)
		}
		if r.ViewIgnoresSize() {
			t.Errorf("tmux %s: a view ignores the size before Check", version)
		}
		if err := r.Check(t.Context()); err != nil {
			t.Fatalf("tmux %s: %v", version, err)
		}
		if got := r.ViewIgnoresSize(); got != want {
			t.Errorf("tmux %s: ViewIgnoresSize = %v, want %v", version, got, want)
		}
	}
}
