package migrate

import (
	"bytes"
	"errors"
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

	if err := Client(clientEnv(home, nil), &out); err != nil {
		t.Fatal(err)
	}
	if !has(t, filepath.Join(home, ".config", "codeflock", "f")) || !has(t, filepath.Join(home, ".local", "share", "codeflock", "f")) {
		t.Error("not moved")
	}
	if !strings.Contains(out.String(), oldCfg) || !strings.Contains(out.String(), oldData) {
		t.Errorf("not said: %q", out.String())
	}
	out.Reset()
	if err := Client(clientEnv(home, nil), &out); err != nil || out.Len() != 0 {
		t.Errorf("the second time: %q %v", out.String(), err)
	}
}

func TestClientMovesTheLaunchdLogsOnMacOS(t *testing.T) {
	home := t.TempDir()
	oldClient(t, home)
	tree(t, filepath.Join(home, "Library", "Logs"), "codefabric", "l")
	e := clientEnv(home, nil)
	e.GOOS = "darwin"

	if err := Client(e, &bytes.Buffer{}); err != nil {
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

	if err := Client(e, &out); err != nil {
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

	if err := Client(clientEnv(home, nil), &out); err != nil {
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
		if err := Client(clientEnv(home, vars), &out); err != nil || out.Len() != 0 {
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

	if err := Client(clientEnv(home, map[string]string{"CF_CONFIG_DIR": oldCfg}), &bytes.Buffer{}); err != nil {
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

	if err := Client(clientEnv(home, nil), &out); err != nil {
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
	if err := Client(clientEnv(home, nil), &out); err != nil {
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
	if err := Client(clientEnv(home, nil), &out); err != nil || out.Len() != 0 {
		t.Errorf("%q %v", out.String(), err)
	}
}

func TestClientMoveFailureIsAnErrorNamingThePair(t *testing.T) {
	home := t.TempDir()
	oldCfg, _ := oldClient(t, home)
	e := clientEnv(home, nil)
	e.Mover = Mover{Rename: func(string, string) error { return errors.New("injected") }}

	err := Client(e, &bytes.Buffer{})
	if err == nil || !strings.Contains(err.Error(), oldCfg) || !strings.Contains(err.Error(), filepath.Join(home, ".config", "codeflock")) {
		t.Fatalf("err = %v", err)
	}
}
