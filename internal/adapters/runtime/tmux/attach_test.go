package tmux

import (
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	cfruntime "github.com/Autarquico/codefabric/internal/adapters/runtime"
	"github.com/Autarquico/codefabric/internal/domain"
)

const (
	runID    = "run_01J8ZQ4M5N6P7R8S9T0V1W2X3Y"
	otherRun = "run_01J8ZQ4M5N6P7R8S9T0V1W2X4Z"
	bin      = "/opt/homebrew/bin/tmux"
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
