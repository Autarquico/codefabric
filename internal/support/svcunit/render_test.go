package svcunit_test

import (
	"flag"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/Autarquico/codefabric/internal/support/svcunit"
)

var update = flag.Bool("update", false, "rewrite the golden files")

// awkward is a value with everything that needs quoting: spaces, specifiers,
// variables, quotes, backslashes and XML.
const awkward = `/Users/a b/100% "cf" $HOME\x &<>`

func workerSpec() svcunit.Spec {
	return svcunit.Spec{
		Name: "cf-worker", Label: "com.codefabric.worker", Description: "CodeFabric Worker",
		Installer: "cf worker install-service",
		Argv:      []string{"/opt/cf dir/bin/cf", "worker", "run", "--ssh-target", "me@host", awkward},
		Env: []svcunit.KV{
			{Key: "CF_CONFIG_DIR", Value: "/home/me/.config/codefabric"},
			{Key: "CF_DATA_DIR", Value: awkward},
			{Key: "PATH", Value: "/opt/cf dir/bin:/usr/bin:/bin"},
		},
		KeepProcesses: true,
	}
}

func controllerSpec() svcunit.Spec {
	return svcunit.Spec{
		Name: "cf-controller", Label: "com.codefabric.controller", Description: "CodeFabric Controller",
		Installer: "cf controller install-service",
		Argv:      []string{"/usr/local/bin/cf", "controller", "run"},
		Env:       []svcunit.KV{{Key: "CF_DATA_DIR", Value: "/srv/cf 50%"}, {Key: "PATH", Value: "/usr/bin:/bin"}},
	}
}

func env(goos, home string) svcunit.Env { return svcunit.Env{Home: home, UID: 501, GOOS: goos} }

func assertGolden(t *testing.T, name string, got []byte) {
	t.Helper()
	path := filepath.Join("testdata", name)
	if *update {
		if err := os.WriteFile(path, got, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	want, err := os.ReadFile(path) //nolint:gosec // fixed test path
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(want) {
		t.Errorf("%s differs:\n--- got\n%s--- want\n%s", name, got, want)
	}
}

func TestRender_Golden(t *testing.T) {
	cases := []struct {
		golden, goos string
		spec         svcunit.Spec
		path         string
	}{
		{"worker.service", "linux", workerSpec(), "/home/me/.config/systemd/user/cf-worker.service"},
		{"controller.service", "linux", controllerSpec(), "/home/me/.config/systemd/user/cf-controller.service"},
		{"worker.plist", "darwin", workerSpec(), "/home/me/Library/LaunchAgents/com.codefabric.worker.plist"},
	}
	for _, c := range cases {
		t.Run(c.golden, func(t *testing.T) {
			path, content, err := svcunit.Render(env(c.goos, "/home/me"), c.spec)
			if err != nil {
				t.Fatal(err)
			}
			if path != c.path {
				t.Errorf("path = %s", path)
			}
			assertGolden(t, c.golden, content)
		})
	}
}

// systemd reads a '#' after a directive as part of its value: comments go on lines of their own.
func TestRender_SystemdCommentsAreLinesOfTheirOwn(t *testing.T) {
	for _, spec := range []svcunit.Spec{workerSpec(), controllerSpec()} {
		_, content, err := svcunit.Render(env("linux", "/home/me"), spec)
		if err != nil {
			t.Fatal(err)
		}
		for line := range strings.Lines(string(content)) {
			if !strings.HasPrefix(line, "#") && strings.Contains(line, "#") {
				t.Errorf("a comment after a directive: %q", line)
			}
		}
	}
}

func TestRender_RefusesWhatAFileCannotHold(t *testing.T) {
	for name, mutate := range map[string]func(*svcunit.Spec){
		"newline in an argument": func(s *svcunit.Spec) { s.Argv = append(s.Argv, "a\nExecStartPre=/bin/evil") },
		"newline in a value":     func(s *svcunit.Spec) { s.Env[0].Value = "x\ny" },
		"relative program":       func(s *svcunit.Spec) { s.Argv[0] = "cf" },
		"bad variable name":      func(s *svcunit.Spec) { s.Env[0].Key = "A B" },
		"bad unit name":          func(s *svcunit.Spec) { s.Name = "cf/worker" },
		"dashes in the comment":  func(s *svcunit.Spec) { s.Installer = "cf -- x" },
	} {
		t.Run(name, func(t *testing.T) {
			spec := workerSpec()
			mutate(&spec)
			for _, goos := range []string{"linux", "darwin"} {
				if _, _, err := svcunit.Render(env(goos, "/home/me"), spec); err == nil {
					t.Errorf("%s: rendered", goos)
				}
			}
		})
	}
}

func TestRender_UnsupportedOS(t *testing.T) {
	if _, _, err := svcunit.Render(env("windows", "/home/me"), workerSpec()); err != svcunit.ErrUnsupported { //nolint:errorlint // the sentinel itself
		t.Errorf("err = %v", err)
	}
}

// InstalledEnv reads back, from either file, the values that went in, quoting and all.
func TestInstalledEnv_RoundTrips(t *testing.T) {
	for _, goos := range []string{"linux", "darwin"} {
		t.Run(goos, func(t *testing.T) {
			e := env(goos, t.TempDir())
			if _, found, err := svcunit.InstalledEnv(e, workerSpec()); found || err != nil {
				t.Fatalf("before install: %v %v", found, err)
			}
			path, content, err := svcunit.Render(e, workerSpec())
			if err != nil {
				t.Fatal(err)
			}
			if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(path, content, 0o600); err != nil {
				t.Fatal(err)
			}
			vars, found, err := svcunit.InstalledEnv(e, workerSpec())
			if err != nil || !found {
				t.Fatalf("%v %v", found, err)
			}
			for _, kv := range workerSpec().Env {
				if vars[kv.Key] != kv.Value {
					t.Errorf("%s = %q, want %q", kv.Key, vars[kv.Key], kv.Value)
				}
			}
		})
	}
}

func TestRender_PlistIsValid(t *testing.T) {
	plutil, err := exec.LookPath("plutil")
	if runtime.GOOS != "darwin" || err != nil {
		t.Skip("plutil is macOS's")
	}
	path := filepath.Join(t.TempDir(), "agent.plist")
	_, content, err := svcunit.Render(env("darwin", "/home/me"), workerSpec())
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, content, 0o600); err != nil {
		t.Fatal(err)
	}
	if out, err := exec.CommandContext(t.Context(), plutil, "-lint", path).CombinedOutput(); err != nil { //nolint:gosec // plutil on a file of the test
		t.Fatalf("plutil: %v\n%s", err, out)
	}
}

func TestRender_UnitIsValid(t *testing.T) {
	analyze, err := exec.LookPath("systemd-analyze")
	if err != nil {
		t.Skip("no systemd-analyze")
	}
	// verify wants the program to exist.
	spec := workerSpec()
	spec.Argv[0] = "/bin/sh"
	dir := t.TempDir()
	_, content, err := svcunit.Render(env("linux", dir), spec)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "cf-worker.service")
	if err := os.WriteFile(path, content, 0o600); err != nil {
		t.Fatal(err)
	}
	out, err := exec.CommandContext(t.Context(), analyze, "verify", "--user", path).CombinedOutput() //nolint:gosec // systemd-analyze on a file of the test
	switch {
	case strings.Contains(string(out), "cf-worker.service:"):
		t.Fatalf("systemd-analyze: %v\n%s", err, out)
	case err != nil:
		// A runner without a user manager cannot verify anything; it says nothing of the unit.
		t.Skipf("systemd-analyze cannot run here: %v\n%s", err, out)
	}
}
