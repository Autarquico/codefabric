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

func TestReplaceable(t *testing.T) {
	// A directory of this user under one only they can write: os.MkdirTemp
	// makes 0700, and t.TempDir's parents are root's or this user's.
	dir := t.TempDir()
	mkdir := func(name string, mode os.FileMode) string {
		t.Helper()
		p := filepath.Join(dir, name)
		if err := os.Mkdir(p, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(p, mode); err != nil {
			t.Fatal(err)
		}
		return p
	}
	open, sticky, own := mkdir("open", 0o777), mkdir("sticky", 0o777|os.ModeSticky), mkdir("own", 0o755)
	if err := os.Symlink(filepath.Join(open, "cf"), filepath.Join(own, "dangling")); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(open, "real"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(open, "real"), filepath.Join(own, "link")); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(own, "shared"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(filepath.Join(own, "shared"), 0o777); err != nil { //nolint:gosec // the unsafe file the test is about
		t.Fatal(err)
	}
	uid := os.Getuid()
	for _, c := range []struct {
		name, program string
		uid           int
		refused       bool
	}{
		{"own directory", filepath.Join(own, "cf"), uid, false},
		{"not there yet", filepath.Join(own, "later", "cf"), uid, false},
		{"sticky world-writable", filepath.Join(sticky, "cf"), uid, false},
		{"world-writable", filepath.Join(open, "cf"), uid, true},
		{"under a world-writable one", filepath.Join(open, "bin", "cf"), uid, true},
		{"a link into a world-writable one", filepath.Join(own, "link"), uid, true},
		{"a link to nothing in a world-writable one", filepath.Join(own, "dangling"), uid, true},
		{"a world-writable file", filepath.Join(own, "shared"), uid, true},
		{"another user's", filepath.Join(own, "cf"), uid + 1, true},
	} {
		err := svcunit.Replaceable(c.program, c.uid)
		var input *svcunit.InputError
		if (err != nil) != c.refused || err != nil && !errors.As(err, &input) {
			t.Errorf("%s: %v", c.name, err)
		}
	}
}

// Writing by a group is refused unless the group is trusted: the directory, or
// the program itself.
func TestReplaceable_GroupWritable(t *testing.T) {
	dir := t.TempDir()
	shared := filepath.Join(dir, "shared")
	if err := os.Mkdir(shared, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(shared, 0o775); err != nil { //nolint:gosec // the group-writable directory the test is about
		t.Fatal(err)
	}
	own := filepath.Join(dir, "own")
	if err := os.Mkdir(own, 0o755); err != nil { //nolint:gosec // a directory of the test
		t.Fatal(err)
	}
	program := filepath.Join(own, "cf")
	if err := os.WriteFile(program, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(program, 0o775); err != nil { //nolint:gosec // the group-writable program the test is about
		t.Fatal(err)
	}
	nobody := func(int) bool { return false }
	everyone := func(int) bool { return true }
	uid := os.Getuid()
	for _, c := range []struct {
		name, program, refusedAt string
		trusted                  func(int) bool
	}{
		{"a directory a foreign group can write", filepath.Join(shared, "cf"), shared, nobody},
		{"a program a foreign group can write", program, program, nobody},
		{"a directory a trusted group can write", filepath.Join(shared, "cf"), "", everyone},
		{"a program a trusted group can write", program, "", everyone},
	} {
		err := svcunit.ReplaceableTrusting(c.program, uid, c.trusted)
		switch {
		case c.refusedAt == "" && err != nil:
			t.Errorf("%s: %v", c.name, err)
		case c.refusedAt != "" && (err == nil || !strings.Contains(err.Error(), "`chmod g-w "+c.refusedAt+"`")):
			t.Errorf("%s: %v", c.name, err)
		}
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
