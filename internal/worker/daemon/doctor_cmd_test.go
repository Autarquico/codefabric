package daemon_test

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Autarquico/codefabric/internal/cli"
	"github.com/Autarquico/codefabric/internal/config"
	"github.com/Autarquico/codefabric/internal/domain"
	"github.com/Autarquico/codefabric/internal/worker/daemon"
	"github.com/Autarquico/codefabric/internal/worker/doctor"
)

// doctorMachine is a machine whose PATH holds only the stubs given: no real
// tmux, git or harness can make the test depend on where it runs.
func doctorMachine(t *testing.T, stubs ...string) {
	t.Helper()
	bin := t.TempDir()
	for _, name := range stubs {
		script := "#!/bin/sh\necho " + name + " version 3.4\n"
		if err := os.WriteFile(filepath.Join(bin, name), []byte(script), 0o700); err != nil { //nolint:gosec // a stub the test runs
			t.Fatal(err)
		}
	}
	root := t.TempDir()
	t.Setenv("PATH", bin)
	t.Setenv("HOME", filepath.Join(root, "home"))
	t.Setenv("CF_CONFIG_DIR", filepath.Join(root, "config"))
	t.Setenv("CF_DATA_DIR", filepath.Join(root, "data"))
	if err := os.MkdirAll(filepath.Join(root, "home"), 0o700); err != nil {
		t.Fatal(err)
	}
}

func cf(t *testing.T, args ...string) (code int, stdout, stderr string) {
	t.Helper()
	var out, errOut bytes.Buffer
	code = cli.RunContextWith(t.Context(), cli.Options{}, args, strings.NewReader(""), &out, &errOut, daemon.Command())
	return code, out.String(), errOut.String()
}

func TestWorkerDoctor_AGreenMachineExitsZero(t *testing.T) {
	doctorMachine(t, "tmux", "git")
	code, out, errOut := cf(t, "worker", "doctor", "--disk-min-free", "1%")
	if code != 0 {
		t.Fatalf("exit %d, stderr %q, stdout %q", code, errOut, out)
	}
	for _, want := range []string{"COMPONENT", "runtime", "git", "disk"} {
		if !strings.Contains(out, want) {
			t.Errorf("table lacks %q:\n%s", want, out)
		}
	}
}

func TestWorkerDoctor_JSONIsOneValueWithTheComponents(t *testing.T) {
	doctorMachine(t, "tmux", "git")
	code, out, errOut := cf(t, "worker", "doctor", "--json", "--disk-min-free", "1%")
	if code != 0 {
		t.Fatalf("exit %d, stderr %q", code, errOut)
	}
	var res daemon.DoctorResult
	dec := json.NewDecoder(strings.NewReader(out))
	if err := dec.Decode(&res); err != nil || len(res.Components) < 3 || dec.More() {
		t.Fatalf("output %q: %+v, %v", out, res, err)
	}
}

func TestWorkerDoctor_AFailureExitsNonZeroAndSaysWhich(t *testing.T) {
	doctorMachine(t, "git") // no tmux
	code, out, errOut := cf(t, "worker", "doctor", "--disk-min-free", "1%")
	if code == 0 {
		t.Fatalf("exit 0 with no tmux:\n%s", out)
	}
	if !strings.Contains(out, "fail") || !strings.Contains(out, "instala tmux") || !strings.Contains(errOut, "1 component") {
		t.Errorf("stdout:\n%s\nstderr: %s", out, errOut)
	}

	code, out, _ = cf(t, "worker", "doctor", "--json", "--disk-min-free", "1%")
	var envelope struct {
		Error struct {
			Details struct {
				Components []struct{ Name, Status string } `json:"components"`
			} `json:"details"`
		} `json:"error"`
	}
	if err := json.Unmarshal([]byte(out), &envelope); err != nil || code == 0 {
		t.Fatalf("exit %d, output %q: %v", code, out, err)
	}
	var failing []string
	for _, c := range envelope.Error.Details.Components {
		if c.Status == "fail" {
			failing = append(failing, c.Name)
		}
	}
	if len(failing) != 1 || failing[0] != "runtime" {
		t.Errorf("failing = %v", failing)
	}
}

func TestWorkerDoctor_ReadsTheDiskThresholdOfTheWorkerConfig(t *testing.T) {
	doctorMachine(t, "tmux", "git")
	if code, _, _ := cf(t, "worker", "doctor", "--disk-min-free", "99%"); code == 0 {
		t.Fatal("a 99% minimum passed")
	}
	if code, _, _ := cf(t, "worker", "doctor", "--disk-min-free", "nope"); code == 0 {
		t.Fatal("an invalid minimum passed")
	}
	paths := doctorPaths(t)
	if err := doctor.SaveSettings(paths, doctor.Settings{DiskMinFree: "99%"}); err != nil {
		t.Fatal(err)
	}
	if code, out, _ := cf(t, "worker", "doctor"); code == 0 || !strings.Contains(out, "mínimo 99%") {
		t.Errorf("exit %d with the persisted 99%%:\n%s", code, out)
	}
}

func doctorPaths(t *testing.T) config.Paths {
	t.Helper()
	p, err := config.ResolveFromEnv()
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func TestWorkerJoin_KeepsTheDiskThresholdInTheWorkerConfig(t *testing.T) {
	c := newController(t)
	paths := machine(t)
	t.Setenv("CF_CONFIG_DIR", paths.ConfigDir)
	t.Setenv("CF_DATA_DIR", paths.DataDir)
	if code, _, errOut := cf(t, "worker", "join", "x", "--disk-min-free", "nope"); code == 0 {
		t.Fatalf("an invalid threshold joined: %s", errOut)
	}
	res, err := c.owner.CreateToken(t.Context(), domain.KindWorker, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if code, _, errOut := cf(t, "worker", "join", res.Invite, "--name", "box", "--disk-min-free", "50GiB"); code != 0 {
		t.Fatalf("join exit %d: %s", code, errOut)
	}
	if s, err := doctor.LoadSettings(paths); err != nil || s.DiskMinFree != "50GiB" {
		t.Errorf("settings = %+v, %v", s, err)
	}
}
