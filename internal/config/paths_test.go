package config

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func resolve(t *testing.T, env map[string]string) (Paths, error) {
	t.Helper()
	return Resolve(func(k string) string { return env[k] }, func() (string, error) { return "/home/ana", nil })
}

func TestResolveUsesXDGDefaultsOnEveryOS(t *testing.T) {
	p, err := resolve(t, nil)
	if err != nil {
		t.Fatal(err)
	}
	if p.ConfigDir != "/home/ana/.config/codefabric" || p.DataDir != "/home/ana/.local/share/codefabric" {
		t.Errorf("%+v", p)
	}
	if p.CredentialsDir() != "/home/ana/.config/codefabric/credentials" ||
		p.DatabaseFile() != "/home/ana/.local/share/codefabric/controller.db" ||
		p.PKIDir() != "/home/ana/.local/share/codefabric/pki" {
		t.Errorf("derived paths: %s %s %s", p.CredentialsDir(), p.DatabaseFile(), p.PKIDir())
	}
}

func TestResolvePrecedence(t *testing.T) {
	p, err := resolve(t, map[string]string{
		"XDG_CONFIG_HOME": "/xdg/c", "XDG_DATA_HOME": "/xdg/d",
		"CF_CONFIG_DIR": "/override/c",
	})
	if err != nil {
		t.Fatal(err)
	}
	if p.ConfigDir != "/override/c" {
		t.Errorf("CF_CONFIG_DIR lost: %s", p.ConfigDir)
	}
	if p.DataDir != filepath.Join("/xdg/d", "codefabric") {
		t.Errorf("XDG_DATA_HOME lost: %s", p.DataDir)
	}
}

func TestResolveIgnoresRelativeXDGAndRejectsRelativeOverrides(t *testing.T) {
	p, err := resolve(t, map[string]string{"XDG_CONFIG_HOME": "relative/dir"})
	if err != nil || p.ConfigDir != "/home/ana/.config/codefabric" {
		t.Errorf("%+v %v", p, err)
	}
	if _, err := resolve(t, map[string]string{"CF_DATA_DIR": "relative"}); err == nil {
		t.Error("a relative CF_DATA_DIR was accepted")
	}
}

func TestResolveReportsAMissingHome(t *testing.T) {
	_, err := Resolve(func(string) string { return "" }, func() (string, error) { return "", errors.New("no home") })
	if err == nil {
		t.Fatal("expected an error")
	}
}

func TestControllerConfigRoundTripAndMissing(t *testing.T) {
	p := Paths{ConfigDir: t.TempDir(), DataDir: t.TempDir()}
	if _, err := LoadController(p); !errors.Is(err, ErrNotInitialized) {
		t.Fatalf("err = %v", err)
	}
	want := Controller{Listen: "0.0.0.0:7443", Advertise: "ctl.example:7443"}
	if err := SaveController(p, want); err != nil {
		t.Fatal(err)
	}
	if got, err := LoadController(p); err != nil || got != want {
		t.Fatalf("%+v %v", got, err)
	}
}

func TestWorkerCredentialsAreApartFromTheClients(t *testing.T) {
	p := Paths{ConfigDir: "/cfg", DataDir: "/data"}
	if got, want := p.WorkerCredentialsDir(), "/cfg/worker/credentials"; got != want {
		t.Errorf("WorkerCredentialsDir = %q, want %q", got, want)
	}
	if p.WorkerCredentialsDir() == p.CredentialsDir() {
		t.Error("a Worker and a client on one machine would share a bundle")
	}
}

func TestControllerConfigWorkerOfflineTTL(t *testing.T) {
	p := Paths{ConfigDir: t.TempDir(), DataDir: t.TempDir()}
	want := Controller{Listen: "0.0.0.0:7443", Advertise: "ctl:7443", WorkerOfflineTTL: Duration(90 * time.Second)}
	if err := SaveController(p, want); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(p.ControllerConfigFile())
	if err != nil || !strings.Contains(string(raw), `"worker_offline_ttl": "1m30s"`) {
		t.Fatalf("file = %s (%v)", raw, err)
	}
	if got, err := LoadController(p); err != nil || got != want {
		t.Fatalf("%+v %v", got, err)
	}

	// Unset, it stays out of the file.
	if err := SaveController(p, Controller{Listen: "a:1", Advertise: "b:2"}); err != nil {
		t.Fatal(err)
	}
	if raw, _ := os.ReadFile(p.ControllerConfigFile()); strings.Contains(string(raw), "ttl") {
		t.Errorf("an unset TTL was written: %s", raw)
	}

	for _, bad := range []string{`"soon"`, `"0s"`, `"-5s"`, `"999ms"`, `45`, `""`} {
		if err := os.WriteFile(p.ControllerConfigFile(), []byte(`{"listen":"a:1","advertise":"b:2","worker_offline_ttl":`+bad+`}`), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := LoadController(p); err == nil {
			t.Errorf("worker_offline_ttl %s was accepted", bad)
		}
	}
}

func TestWorkerPathsLiveUnderTheirOwnDirectory(t *testing.T) {
	p := Paths{ConfigDir: "/c", DataDir: "/d"}
	for got, want := range map[string]string{
		p.WorkerDataDir():            "/d/worker",
		p.WorkerStateFile():          "/d/worker/state.db",
		p.WorkerRunDir("run_X"):      "/d/worker/runs/run_X",
		p.WorkerWorktreeDir("run_X"): "/d/worker/worktrees/run_X",
	} {
		if got != want {
			t.Errorf("got %s, want %s", got, want)
		}
	}
}
