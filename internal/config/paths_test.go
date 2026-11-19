package config

import (
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"slices"
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
	if p.ConfigDir != "/home/ana/.config/codeflock" || p.DataDir != "/home/ana/.local/share/codeflock" {
		t.Errorf("%+v", p)
	}
	if p.CredentialsDir() != "/home/ana/.config/codeflock/credentials" ||
		p.DatabaseFile() != "/home/ana/.local/share/codeflock/controller.db" ||
		p.PKIDir() != "/home/ana/.local/share/codeflock/pki" {
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
	if p.DataDir != filepath.Join("/xdg/d", "codeflock") {
		t.Errorf("XDG_DATA_HOME lost: %s", p.DataDir)
	}
}

func TestResolveIgnoresRelativeXDGAndRejectsRelativeOverrides(t *testing.T) {
	p, err := resolve(t, map[string]string{"XDG_CONFIG_HOME": "relative/dir"})
	if err != nil || p.ConfigDir != "/home/ana/.config/codeflock" {
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
	if got, err := LoadController(p); err != nil || !reflect.DeepEqual(got, want) {
		t.Fatalf("%+v %v", got, err)
	}
}

func TestControllerConfigCheckpointExclude(t *testing.T) {
	p := Paths{ConfigDir: t.TempDir(), DataDir: t.TempDir()}

	// Unset, it stays out of the file and the defaults apply.
	if err := SaveController(p, Controller{Listen: "a:1", Advertise: "b:2"}); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(p.ControllerConfigFile())
	if err != nil || strings.Contains(string(raw), "checkpoint") {
		t.Fatalf("file = %s (%v)", raw, err)
	}
	got, err := LoadController(p)
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"**/.env*", "**/*.pem", "**/*.key"}; !slices.Equal(got.Checkpoint.Excluded(), want) {
		t.Errorf("default = %v, want %v", got.Checkpoint.Excluded(), want)
	}

	// Set, it replaces the defaults and survives a round trip.
	want := Controller{Listen: "a:1", Advertise: "b:2", Checkpoint: Checkpoint{Exclude: []string{"**/*.p12"}}}
	if err := SaveController(p, want); err != nil {
		t.Fatal(err)
	}
	if got, err := LoadController(p); err != nil || !slices.Equal(got.Checkpoint.Excluded(), []string{"**/*.p12"}) {
		t.Fatalf("%+v %v", got, err)
	}
}

func TestCheckpointExcludedIsACopy(t *testing.T) {
	DefaultCheckpointExclude()[0] = "changed"
	if got := (Checkpoint{}).Excluded()[0]; got != "**/.env*" {
		t.Errorf("a caller changed the default: %q", got)
	}
	set := Checkpoint{Exclude: []string{"a"}}
	set.Excluded()[0] = "changed"
	if set.Exclude[0] != "a" {
		t.Error("a caller changed the configured list")
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

func TestWorkerSecretsLiveInTheWorkersPrivateConfigNextToItsCredentials(t *testing.T) {
	p := Paths{ConfigDir: "/cfg", DataDir: "/data"}
	if got, want := p.WorkerSecretsFile(), "/cfg/worker/secrets.json"; got != want {
		t.Errorf("WorkerSecretsFile = %q, want %q", got, want)
	}
	if filepath.Dir(p.WorkerSecretsFile()) != filepath.Dir(p.WorkerCredentialsDir()) {
		t.Error("the catalog is not in the private directory of the Worker")
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
	if got, err := LoadController(p); err != nil || !reflect.DeepEqual(got, want) {
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
		p.WorkerDataDir():               "/d/worker",
		p.WorkerStateFile():             "/d/worker/state.db",
		p.WorkerRunDir("run_X", 2):      "/d/worker/runs/run_X-e2",
		p.WorkerWorktreeDir("run_X", 2): "/d/worker/worktrees/run_X-e2",
		p.WorkerRepoDir("project_X"):    "/d/worker/repos/project_X.git",
		p.RelayDir():                    "/d/relay",
	} {
		if got != want {
			t.Errorf("got %s, want %s", got, want)
		}
	}
}

// capture reports the warnings of legacy variables to a buffer, afresh.
func capture(t *testing.T) *strings.Builder {
	t.Helper()
	var b strings.Builder
	was := legacyVarWarning
	legacyVarWarning = &b
	warnedVars.Clear()
	t.Cleanup(func() { legacyVarWarning = was; warnedVars.Clear() })
	return &b
}

func TestResolveReadsTheOldVariablesWithOneWarningPerProcess(t *testing.T) {
	warned := capture(t)
	env := map[string]string{"CF_CONFIG_DIR": "/old/c", "CF_DATA_DIR": "/old/d"}
	for range 3 {
		p, err := resolve(t, env)
		if err != nil || p.ConfigDir != "/old/c" || p.DataDir != "/old/d" {
			t.Fatalf("%+v %v", p, err)
		}
	}
	if n := strings.Count(warned.String(), "CF_CONFIG_DIR is deprecated"); n != 1 {
		t.Errorf("%d warnings of CF_CONFIG_DIR:\n%s", n, warned)
	}
	if !strings.Contains(warned.String(), "use CFLK_CONFIG_DIR") || !strings.Contains(warned.String(), "CF_DATA_DIR is deprecated") {
		t.Errorf("warnings:\n%s", warned)
	}
}

func TestResolveCFLKWinsOverCFAndSaysNothing(t *testing.T) {
	warned := capture(t)
	p, err := resolve(t, map[string]string{"CFLK_CONFIG_DIR": "/new/c", "CF_CONFIG_DIR": "/old/c", "CFLK_DATA_DIR": "/new/d"})
	if err != nil || p.ConfigDir != "/new/c" || p.DataDir != "/new/d" {
		t.Fatalf("%+v %v", p, err)
	}
	if warned.Len() != 0 {
		t.Errorf("warned: %s", warned)
	}
}

func TestResolveRejectsARelativeOldVariableByName(t *testing.T) {
	capture(t)
	if _, err := resolve(t, map[string]string{"CF_CONFIG_DIR": "rel"}); err == nil || !strings.Contains(err.Error(), "CF_CONFIG_DIR") {
		t.Errorf("err = %v", err)
	}
}

func TestLocateSaysWhichDirectoriesAreDefault(t *testing.T) {
	capture(t)
	l, err := Locate(func(k string) string { return map[string]string{"CFLK_DATA_DIR": "/mine"}[k] }, func() (string, error) { return "/home/ana", nil })
	if err != nil || !l.ConfigIsDefault || l.DataIsDefault {
		t.Fatalf("%+v %v", l, err)
	}
	if got := LegacyOf(l.ConfigDir); got != "/home/ana/.config/codefabric" {
		t.Errorf("legacy = %s", got)
	}
}
