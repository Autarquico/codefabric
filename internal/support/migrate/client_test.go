package migrate

import (
	"bytes"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// clientEnv is a Linux user with this home and these variables.
func clientEnv(home string, vars map[string]string) ClientEnv {
	return ClientEnv{
		Getenv: func(k string) string { return vars[k] },
		Home:   func() (string, error) { return home, nil },
		GOOS:   "linux",
	}
}

func oldClient(t *testing.T, home string) (config, data string) {
	t.Helper()
	tree(t, filepath.Join(home, ".config"), "codefabric", "c")
	tree(t, filepath.Join(home, ".local", "share"), "codefabric", "d")
	return filepath.Join(home, ".config", "codefabric"), filepath.Join(home, ".local", "share", "codefabric")
}

func TestClientMovesTheDefaultDirectoriesAndSaysSo(t *testing.T) {
	home := t.TempDir()
	oldCfg, oldData := oldClient(t, home)
	var out bytes.Buffer

	if err := runClient(t, clientEnv(home, nil), &out); err != nil {
		t.Fatal(err)
	}
	if !has(t, filepath.Join(home, ".config", "codeflock", "f")) || !has(t, filepath.Join(home, ".local", "share", "codeflock", "f")) {
		t.Error("not moved")
	}
	if !strings.Contains(out.String(), oldCfg) || !strings.Contains(out.String(), oldData) {
		t.Errorf("not said: %q", out.String())
	}
	out.Reset()
	if err := runClient(t, clientEnv(home, nil), &out); err != nil || out.Len() != 0 {
		t.Errorf("the second time: %q %v", out.String(), err)
	}
}

func TestClientMovesTheLaunchdLogsOnMacOS(t *testing.T) {
	home := t.TempDir()
	oldClient(t, home)
	tree(t, filepath.Join(home, "Library", "Logs"), "codefabric", "l")
	e := clientEnv(home, nil)
	e.GOOS = "darwin"

	if err := runClient(t, e, &bytes.Buffer{}); err != nil {
		t.Fatal(err)
	}
	if !has(t, filepath.Join(home, "Library", "Logs", "codeflock", "f")) || has(t, filepath.Join(home, "Library", "Logs", "codefabric")) {
		t.Error("the logs were not moved")
	}
}

func TestClientPreflightCoversTheLogs(t *testing.T) {
	home := t.TempDir()
	oldCfg, _ := oldClient(t, home)
	tree(t, filepath.Join(home, "Library", "Logs"), "codefabric", "l")
	tree(t, filepath.Join(home, "Library", "Logs"), "codeflock", "other") // clash in the third pair
	e := clientEnv(home, nil)
	e.GOOS = "darwin"
	var out bytes.Buffer

	if err := runClient(t, e, &out); err != nil {
		t.Fatal(err)
	}
	if !has(t, oldCfg) || has(t, filepath.Join(home, ".config", "codeflock")) {
		t.Error("the config moved although the logs clash")
	}
	if !strings.Contains(out.String(), filepath.Join(home, "Library", "Logs", "codeflock")) {
		t.Errorf("the clash is not named: %q", out.String())
	}
}

func TestClientClashIsAWarningNotAnError(t *testing.T) {
	home := t.TempDir()
	oldCfg, _ := oldClient(t, home)
	tree(t, filepath.Join(home, ".config"), "codeflock", "other")
	var out bytes.Buffer

	if err := runClient(t, clientEnv(home, nil), &out); err != nil {
		t.Fatalf("a clash stopped the command: %v", err)
	}
	if !strings.Contains(out.String(), oldCfg) || !strings.Contains(out.String(), filepath.Join(home, ".config", "codeflock")) {
		t.Errorf("both paths must be named: %q", out.String())
	}
}

func TestClientLeavesAPathGivenByHandAlone(t *testing.T) {
	home := t.TempDir()
	oldCfg, oldData := oldClient(t, home)
	for _, vars := range []map[string]string{
		{"CF_CONFIG_DIR": oldCfg, "CF_DATA_DIR": oldData},
		{"CFLK_CONFIG_DIR": oldCfg, "CFLK_DATA_DIR": oldData},
	} {
		var out bytes.Buffer
		if err := runClient(t, clientEnv(home, vars), &out); err != nil || out.Len() != 0 {
			t.Fatalf("%v: %q %v", vars, out.String(), err)
		}
		if !has(t, filepath.Join(oldCfg, "f")) || !has(t, filepath.Join(oldData, "f")) || has(t, filepath.Join(home, ".config", "codeflock")) {
			t.Errorf("%v: a directory given by hand was moved", vars)
		}
	}
}

func TestClientMovesOnlyTheDefaultOfTheTwo(t *testing.T) {
	home := t.TempDir()
	oldCfg, oldData := oldClient(t, home)

	if err := runClient(t, clientEnv(home, map[string]string{"CF_CONFIG_DIR": oldCfg}), &bytes.Buffer{}); err != nil {
		t.Fatal(err)
	}
	if !has(t, oldCfg) || has(t, oldData) || !has(t, filepath.Join(home, ".local", "share", "codeflock", "f")) {
		t.Error("only the data directory should have moved")
	}
}

func TestClientWithAnOldUnitChangesNothing(t *testing.T) {
	home := t.TempDir()
	oldCfg, oldData := oldClient(t, home)
	units := filepath.Join(home, ".config", "systemd", "user")
	for _, name := range []string{"cf-worker.service", "cf-controller.service"} {
		if err := os.MkdirAll(units, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(units, name), []byte("[Unit]\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	var out bytes.Buffer

	if err := runClient(t, clientEnv(home, nil), &out); err != nil {
		t.Fatal(err)
	}
	if !has(t, filepath.Join(oldCfg, "f")) || !has(t, filepath.Join(oldData, "f")) ||
		has(t, filepath.Join(home, ".config", "codeflock")) || has(t, filepath.Join(home, ".local", "share", "codeflock")) {
		t.Error("something moved with a service installed")
	}
	if out.Len() != 0 {
		t.Errorf("the warning is config.Resolve's, once per process, not said here: %q", out.String())
	}
}

func TestClientWithAStoppedSuffixedUnitChangesNothing(t *testing.T) {
	home := t.TempDir()
	oldClient(t, home)
	units := filepath.Join(home, ".config", "systemd", "user")
	if err := os.MkdirAll(units, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(units, "cf-worker-t1.service"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if err := runClient(t, clientEnv(home, nil), &out); err != nil {
		t.Fatal(err)
	}
	if has(t, filepath.Join(home, ".config", "codeflock")) {
		t.Error("moved")
	}
}

func TestClientAnOldUnitWithNothingToMoveIsSilent(t *testing.T) {
	home := t.TempDir()
	units := filepath.Join(home, ".config", "systemd", "user")
	if err := os.MkdirAll(units, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(units, "cf-worker.service"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if err := runClient(t, clientEnv(home, nil), &out); err != nil || out.Len() != 0 {
		t.Errorf("%q %v", out.String(), err)
	}
}

func TestClientMoveFailureIsAnErrorNamingThePair(t *testing.T) {
	home := t.TempDir()
	oldCfg, _ := oldClient(t, home)
	e := clientEnv(home, nil)
	e.Mover = Mover{Rename: func(string, string) error { return errors.New("injected") }}

	err := runClient(t, e, &bytes.Buffer{})
	if err == nil || !strings.Contains(err.Error(), oldCfg) || !strings.Contains(err.Error(), filepath.Join(home, ".config", "codeflock")) {
		t.Fatalf("err = %v", err)
	}
}

func TestClientWithADaemonStateMovesOnlyForTheRunOfThatDaemon(t *testing.T) {
	for _, tc := range []struct {
		name  string
		state string // what the old data directory holds
		args  []string
		moved bool
	}{
		{"version with a Worker", "worker", []string{"version"}, false},
		{"no arguments with a Worker", "worker", nil, false},
		{"worker run", "worker", []string{"worker", "run"}, true},
		{"worker run behind global flags", "worker", []string{"--json", "--controller", "x:1", "worker", "run", "--ssh-target", "a"}, true},
		{"controller run with a Worker", "worker", []string{"controller", "run"}, false},
		{"controller run", "controller.db", []string{"controller", "run"}, true},
		{"status with a Controller", "controller.db", []string{"status"}, false},
		{"a pure client", "", []string{"version"}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			home := t.TempDir()
			_, oldData := oldClient(t, home)
			switch tc.state {
			case "worker":
				tree(t, oldData, "worker", "w")
			case "controller.db":
				if err := os.WriteFile(filepath.Join(oldData, "controller.db"), nil, 0o600); err != nil {
					t.Fatal(err)
				}
			}
			e := clientEnv(home, nil)
			e.Args = tc.args
			var out bytes.Buffer
			if err := runClient(t, e, &out); err != nil {
				t.Fatal(err)
			}
			if got := has(t, filepath.Join(home, ".local", "share", "codeflock")); got != tc.moved {
				t.Errorf("moved = %v, want %v (%q)", got, tc.moved, out.String())
			}
			if !tc.moved && tc.state != "" && !strings.Contains(out.String(), "install-service") {
				t.Errorf("not said: %q", out.String())
			}
		})
	}
}

// runClient is Client with the hold of the data directory released when the test ends.
func runClient(t *testing.T, e ClientEnv, out io.Writer) error {
	t.Helper()
	release, err := Client(e, out)
	t.Cleanup(release)
	return err
}

func TestClientDoesNotMoveForHelp(t *testing.T) {
	for _, args := range [][]string{{"worker", "run", "--help"}, {"controller", "run", "-h"}, {"help", "worker", "run"}, {"--help"}, {"worker", "run", "--help=true"}, {"worker", "run", "--help=false"}} {
		home := t.TempDir()
		oldClient(t, home)
		e := clientEnv(home, nil)
		e.Args = args
		if err := runClient(t, e, &bytes.Buffer{}); err != nil {
			t.Fatal(err)
		}
		if has(t, filepath.Join(home, ".config", "codeflock")) || has(t, filepath.Join(home, ".local", "share", "codeflock")) {
			t.Errorf("%v moved the directories", args)
		}
	}
}

func TestClientDoesNotMoveWhileAnotherMigrationHoldsItsLock(t *testing.T) {
	home := t.TempDir()
	oldClient(t, home)
	release, err := lockMigration(home)
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	var out bytes.Buffer
	if err := runClient(t, clientEnv(home, nil), &out); err != nil {
		t.Fatal(err)
	}
	if has(t, filepath.Join(home, ".local", "share", "codeflock")) || !strings.Contains(out.String(), "not moving") {
		t.Errorf("moved, or silent: %q", out.String())
	}
}

// The `run` that moves keeps the data directory held while it lives: a second
// command that finds it held leaves the directory where it is, and one that
// finds it moved has nothing to move.
func TestClientRunHoldsTheDataDirectoryWhileItLives(t *testing.T) {
	home := t.TempDir()
	_, oldData := oldClient(t, home)
	tree(t, oldData, "worker", "w")
	e := clientEnv(home, nil)
	e.Args = []string{"worker", "run"}
	release, err := Client(e, &bytes.Buffer{})
	if err != nil {
		t.Fatal(err)
	}
	newData := filepath.Join(home, ".local", "share", "codeflock")
	if !has(t, filepath.Join(newData, "worker", "f")) {
		t.Fatal("did not move")
	}
	if _, err := holdData(newData); err == nil {
		t.Error("the directory of a running daemon can be taken")
	}
	release()
	free, err := holdData(newData)
	if err != nil {
		t.Fatalf("not released: %v", err)
	}
	free()
}

func TestClientDoesNotMoveADataDirectoryAnotherProcessHolds(t *testing.T) {
	home := t.TempDir()
	_, oldData := oldClient(t, home)
	held, err := holdData(oldData)
	if err != nil {
		t.Fatal(err)
	}
	defer held()
	var out bytes.Buffer
	if err := runClient(t, clientEnv(home, nil), &out); err != nil {
		t.Fatal(err)
	}
	if has(t, filepath.Join(home, ".local", "share", "codeflock")) || !strings.Contains(out.String(), "has") {
		t.Errorf("moved, or silent: %q", out.String())
	}
}

// A daemon that runs on the old root because CFLK_DATA_DIR says so holds it as
// well: a second `run`, on the default directories, does not move it.
func TestClientRunHoldsTheDataDirectoryItIsGiven(t *testing.T) {
	home := t.TempDir()
	_, oldData := oldClient(t, home)
	tree(t, oldData, "worker", "w")
	first := clientEnv(home, map[string]string{"CFLK_DATA_DIR": oldData})
	first.Args = []string{"worker", "run"}
	release, err := Client(first, &bytes.Buffer{})
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	second := clientEnv(home, nil)
	second.Args = []string{"worker", "run"}
	var out bytes.Buffer
	if err := runClient(t, second, &out); err != nil {
		t.Fatal(err)
	}
	if has(t, filepath.Join(home, ".local", "share", "codeflock")) || !has(t, filepath.Join(oldData, "worker", "f")) {
		t.Errorf("moved the directory of a live daemon: %q", out.String())
	}
}
