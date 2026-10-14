package svcunit_test

import (
	"errors"
	"os"
	"os/user"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"testing"

	"github.com/Autarquico/codefabric/internal/support/svcunit"
)

func TestServicePATH_ProgramsFirstThenTheUsualOnesOnce(t *testing.T) {
	if got := svcunit.ServicePATH("linux", "/opt/cf/bin", "/usr/bin", "relative", "/opt/cf/bin/"); got != "/opt/cf/bin:/usr/bin:/usr/local/bin:/bin" {
		t.Errorf("linux: %s", got)
	}
	if got := svcunit.ServicePATH("darwin"); got != "/opt/homebrew/bin:/usr/local/bin:/usr/bin:/bin" {
		t.Errorf("darwin: %s", got)
	}
}

func TestValidatePATH(t *testing.T) {
	for path, valid := range map[string]bool{
		"/usr/bin:/bin": true, "/a b/bin": true,
		"": false, "/usr/bin:": false, "bin:/usr/bin": false, "/usr/bin::/bin": false, "/usr/bin\n": false,
	} {
		if err := svcunit.ValidatePATH(path); (err == nil) != valid {
			t.Errorf("%q: %v", path, err)
		}
	}
}

func TestExecutableWarning(t *testing.T) {
	for program, warns := range map[string]bool{
		"/usr/local/bin/cf":                      false,
		filepath.Join(os.TempDir(), "x", "cf"):   true,
		"/Users/me/Library/Caches/go-build/a/cf": true,
		"/opt/homebrew/Cellar/cf/0.1/bin/cf":     true,
	} {
		if got := svcunit.ExecutableWarning(program); (got != "") != warns {
			t.Errorf("%s: %q", program, got)
		}
	}
}

// entry is a directory (a mode with os.ModeDir), a file of its mode, or, with
// no mode, a symbolic link to target.
type entry struct {
	path   string
	mode   os.FileMode
	target string
}

// fsTree builds the entries, in order, under a directory only this user can write.
func fsTree(t *testing.T, entries ...entry) string {
	t.Helper()
	// os.MkdirTemp makes 0700, and t.TempDir's parents are root's or this user's.
	// Resolved, so that the refusals name the directories they are about.
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		p := filepath.Join(root, e.path)
		switch {
		case e.mode == 0:
			err = os.Symlink(e.target, p)
		case e.mode.IsDir():
			err = os.Mkdir(p, 0o700)
		default:
			err = os.WriteFile(p, nil, 0o600)
		}
		if err == nil && e.mode != 0 {
			err = os.Chmod(p, e.mode&(os.ModePerm|os.ModeSticky))
		}
		if err != nil {
			t.Fatal(err)
		}
	}
	return root
}

func TestReplaceable(t *testing.T) {
	const dir, sticky, exe = os.ModeDir | 0o755, os.ModeDir | os.ModeSticky | 0o777, 0o755
	root := fsTree(t,
		entry{path: "safe", mode: dir},
		entry{path: "safe/cf", mode: exe},
		entry{path: "safe/plain", mode: 0o644},
		entry{path: "safe/group", mode: 0o775},
		entry{path: "safe/world", mode: 0o777},
		entry{path: "safe/real", mode: dir},
		entry{path: "safe/real/cf", mode: exe},
		entry{path: "open", mode: os.ModeDir | 0o777},
		entry{path: "open/cf", mode: exe},
		entry{path: "shared", mode: os.ModeDir | 0o775},
		entry{path: "shared/dir", mode: dir},
		entry{path: "tmp", mode: sticky},
		entry{path: "tmp/cf", mode: exe},
		// Codex's N1: back to a safe path, through a directory a foreign group can write.
		entry{path: "safe/link", target: "../shared/dir"},
		entry{path: "shared/dir/back", target: "../../safe/real"},
		entry{path: "safe/into-open", target: "../open/cf"},
		entry{path: "safe/dangling", target: "nothing"},
		entry{path: "safe/loop-a", target: "loop-b"},
		entry{path: "safe/loop-b", target: "loop-a"},
		// Like Homebrew: bin/cf -> ../Cellar/cf/1/bin/cf.
		entry{path: "brew", mode: dir},
		entry{path: "brew/bin", mode: dir},
		entry{path: "brew/Cellar", mode: dir},
		entry{path: "brew/Cellar/cf", mode: dir},
		entry{path: "brew/Cellar/cf/1", mode: dir},
		entry{path: "brew/Cellar/cf/1/bin", mode: dir},
		entry{path: "brew/Cellar/cf/1/bin/cf", mode: exe},
		entry{path: "brew/bin/cf", target: "../Cellar/cf/1/bin/cf"},
	)
	nobody := func(int) bool { return false }
	everyone := func(int) bool { return true }
	uid := os.Getuid()
	for _, c := range []struct {
		name, program string
		uid           int
		trusted       func(int) bool
		refusal       string // in the error; empty is accepted
	}{
		{"an executable of mine", "safe/cf", uid, nobody, ""},
		{"absent", "safe/later", uid, nobody, "no such file"},
		{"under an absent directory", "safe/later/cf", uid, nobody, "no such file"},
		{"not executable", "safe/plain", uid, nobody, "not a file you can run"},
		{"a directory", "safe/real", uid, nobody, "not a file you can run"},
		{"a dangling link", "safe/dangling", uid, nobody, "no such file"},
		{"a loop of links", "safe/loop-a", uid, nobody, "symbolic links"},
		{"another user's", "safe/cf", uid + 1, nobody, "belongs to another user"},
		{"in a world-writable directory", "open/cf", uid, nobody, "open, which anyone can write"},
		{"a link into a world-writable directory", "safe/into-open", uid, nobody, "open, which anyone can write"},
		{"world-writable", "safe/world", uid, nobody, "anyone can write"},
		{"writable by a foreign group", "safe/group", uid, nobody, "`chmod g-w " + filepath.Join(root, "safe/group") + "`"},
		{"writable by a trusted group", "safe/group", uid, everyone, ""},
		{
			"links back to a safe path through a foreign group's directory", "safe/link/back/cf", uid, nobody,
			"`chmod g-w " + filepath.Join(root, "shared") + "`",
		},
		{"links back to a safe path through a trusted group's directory", "safe/link/back/cf", uid, everyone, ""},
		{"mine in a sticky world-writable directory", "tmp/cf", uid, nobody, ""},
		{"like Homebrew", "brew/bin/cf", uid, nobody, ""},
	} {
		err := svcunit.ReplaceableTrusting(filepath.Join(root, c.program), c.uid, c.trusted)
		var input *svcunit.InputError
		switch {
		case c.refusal == "" && err != nil:
			t.Errorf("%s: %v", c.name, err)
		case c.refusal != "" && (!errors.As(err, &input) || !strings.Contains(err.Error(), c.refusal)):
			t.Errorf("%s: %v, want a refusal with %q", c.name, err, c.refusal)
		}
	}
	// The service runs the path given, not where it leads: a Homebrew upgrade moves the target.
	brew := filepath.Join(root, "brew/bin/cf")
	if program, _, err := svcunit.Program(brew); err != nil || program != brew {
		t.Errorf("Program(%s) = %q, %v", brew, program, err)
	}
}

func TestTrustedGroup(t *testing.T) {
	alex := &user.User{Username: "alex", Uid: "1000", Gid: "1000"}
	for _, c := range []struct {
		name  string
		goos  string
		gid   int
		group string
		owner *user.User
		want  bool
	}{
		{"root's", "linux", 0, "root", alex, true},
		{"wheel", "linux", 10, "wheel", alex, true},
		{"sudo", "linux", 27, "sudo", alex, true},
		{"admin on macOS", "darwin", 80, "admin", alex, true},
		{"admin elsewhere", "linux", 4, "admin", alex, false},
		{"the user's private group", "linux", 1000, "alex", alex, true},
		{"the user's primary group, shared", "darwin", 20, "staff", &user.User{Username: "alex", Gid: "20"}, false},
		{"a group of the user's name that is not their primary", "linux", 1001, "alex", alex, false},
		{"another group", "linux", 100, "users", alex, false},
		{"an unknown user", "linux", 1000, "alex", nil, false},
		{"an unknown group", "linux", 1000, "", alex, false},
	} {
		if got := svcunit.TrustedGroup(c.goos, c.gid, c.group, c.owner); got != c.want {
			t.Errorf("%s: %t", c.name, got)
		}
	}
}

// Homebrew's bin is the user's and group admin's, 0775: the tmux there is fine.
func TestReplaceable_HomebrewOnThisMac(t *testing.T) {
	const tmux = "/opt/homebrew/bin/tmux"
	info, err := os.Stat("/opt/homebrew/bin")
	if runtime.GOOS != "darwin" || err != nil {
		t.Skip("no Homebrew here")
	}
	if st, ok := info.Sys().(*syscall.Stat_t); !ok || int(st.Uid) != os.Getuid() {
		t.Skip("Homebrew belongs to another user")
	}
	if _, err := os.Stat(tmux); err != nil {
		t.Skip("no tmux in Homebrew")
	}
	if err := svcunit.Replaceable(tmux, os.Getuid()); err != nil {
		t.Error(err)
	}
	if program, _, err := svcunit.Program(tmux); err != nil || program != tmux {
		t.Errorf("Program(%s) = %q, %v", tmux, program, err)
	}
}

func TestProgram(t *testing.T) {
	if _, _, err := svcunit.Program("cf"); err == nil || !strings.Contains(err.Error(), "--binary") {
		t.Errorf("relative: %v", err)
	}
	program, _, err := svcunit.Program("")
	if exe, _ := os.Executable(); err != nil || program != exe {
		t.Errorf("this executable: %q %v", program, err)
	}
}

func TestResolvePATH(t *testing.T) {
	if got, err := svcunit.ResolvePATH("linux", "/usr/bin:/bin", "/opt/cf/cf"); err != nil || got != "/usr/bin:/bin" {
		t.Errorf("given: %q %v", got, err)
	}
	var input *svcunit.InputError
	if _, err := svcunit.ResolvePATH("linux", "bin", "/opt/cf/cf"); !errors.As(err, &input) || input.Flag != "--path" {
		t.Errorf("relative: %v", err)
	}
	if got, err := svcunit.ResolvePATH("linux", "", "/opt/cf/cf"); err != nil || !strings.HasPrefix(got, "/opt/cf:") {
		t.Errorf("calculated: %q %v", got, err)
	}
}
