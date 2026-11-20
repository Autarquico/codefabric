package config

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// oldHome is a user whose directories still have the old name, with an old
// service installed if unit is not empty.
func oldHome(t *testing.T, unit string) (home string, resolve func(env map[string]string) Paths) {
	t.Helper()
	home = t.TempDir()
	for _, dir := range []string{".config/codefabric/credentials", ".local/share/codefabric/worker"} {
		if err := os.MkdirAll(filepath.Join(home, dir), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	if unit != "" {
		dir := filepath.Join(home, ".config", "systemd", "user")
		if runtime.GOOS == "darwin" {
			dir = filepath.Join(home, "Library", "LaunchAgents")
		}
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, unit), nil, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return home, func(env map[string]string) Paths {
		p, err := Resolve(func(k string) string { return env[k] }, func() (string, error) { return home, nil })
		if err != nil {
			t.Fatal(err)
		}
		return p
	}
}

func oldWorkerUnit() string {
	if runtime.GOOS == "darwin" {
		return "com.codefabric.worker.plist"
	}
	return "cf-worker.service"
}

func TestResolveKeepsTheOldDirectoriesWhileTheNewOnesDoNotExist(t *testing.T) {
	warned := capture(t)
	home, resolve := oldHome(t, oldWorkerUnit())

	p := resolve(nil)

	if p.ConfigDir != filepath.Join(home, ".config", "codefabric") || p.DataDir != filepath.Join(home, ".local", "share", "codefabric") {
		t.Errorf("%+v", p)
	}
	// What a `worker run` with no variables opens: the database of the old service.
	if p.WorkerStateFile() != filepath.Join(home, ".local", "share", "codefabric", "worker", "state.db") {
		t.Errorf("state file: %s", p.WorkerStateFile())
	}
	resolve(nil)
	resolve(nil)
	if n := strings.Count(warned.String(), "install-service"); n != 1 || !strings.Contains(warned.String(), strings.TrimSuffix(strings.TrimSuffix(oldWorkerUnit(), ".service"), ".plist")) ||
		!strings.Contains(warned.String(), "cflk worker install-service") {
		t.Errorf("one line naming the unit and its install-service per process, got:\n%s", warned)
	}
}

func TestResolveGoesToTheNewDirectoryOnceItExists(t *testing.T) {
	capture(t)
	home, resolve := oldHome(t, "")
	if err := os.MkdirAll(filepath.Join(home, ".config", "codeflock"), 0o700); err != nil {
		t.Fatal(err)
	}
	p := resolve(nil)
	if p.ConfigDir != filepath.Join(home, ".config", "codeflock") || p.DataDir != filepath.Join(home, ".local", "share", "codefabric") {
		t.Errorf("each directory on its own: %+v", p)
	}
}

func TestResolveKeepsWhatAVariableSaysEvenIfTheOldDirectoryExists(t *testing.T) {
	warned := capture(t)
	home, resolve := oldHome(t, oldWorkerUnit())
	p := resolve(map[string]string{"CFLK_CONFIG_DIR": filepath.Join(home, "elsewhere"), "CFLK_DATA_DIR": filepath.Join(home, "data")})
	if p.ConfigDir != filepath.Join(home, "elsewhere") || p.DataDir != filepath.Join(home, "data") || warned.Len() != 0 {
		t.Errorf("%+v %q", p, warned)
	}
}

func TestOldUnitsFindsSuffixedAndStoppedOnes(t *testing.T) {
	home, _ := oldHome(t, "")
	dir := filepath.Join(home, ".config", "systemd", "user")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	for _, f := range []string{"cf-worker-t1.service", "cf-controller.service", "other.service"} {
		if err := os.WriteFile(filepath.Join(dir, f), nil, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	var got []string
	for _, u := range OldUnits(home, "linux") {
		got = append(got, u.Kind+":"+u.Name)
	}
	if strings.Join(got, ",") != "worker:cf-worker-t1,controller:cf-controller" {
		t.Errorf("%v", got)
	}
}
