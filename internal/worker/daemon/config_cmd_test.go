package daemon_test

import (
	"strings"
	"testing"

	"github.com/Autarquico/codefabric/internal/config"
	"github.com/Autarquico/codefabric/internal/worker/doctor"
)

func TestWorkerConfigSet_DiskMinFreeKeepsTheOtherSettings(t *testing.T) {
	doctorMachine(t)
	paths, err := config.ResolveFromEnv()
	if err != nil {
		t.Fatal(err)
	}
	before := doctor.Settings{Name: "mini", DiskMinFree: "20%", Harnesses: []string{"claude"}, SetupPending: true}
	if err := doctor.SaveSettings(paths, before); err != nil {
		t.Fatal(err)
	}

	if code, out, errOut := cf(t, "worker", "config", "set", "disk-min-free", "50GiB"); code != 0 {
		t.Fatalf("exit %d: %q %q", code, out, errOut)
	}
	after, err := doctor.LoadSettings(paths)
	if err != nil {
		t.Fatal(err)
	}
	if after.Name != "mini" || after.DiskMinFree != "50GiB" || len(after.Harnesses) != 1 || !after.SetupPending {
		t.Errorf("settings = %+v", after)
	}
}

func TestWorkerConfigSet_RefusesWhatItCannotUnderstandAndChangesNothing(t *testing.T) {
	doctorMachine(t)
	paths, err := config.ResolveFromEnv()
	if err != nil {
		t.Fatal(err)
	}
	if err := doctor.SaveSettings(paths, doctor.Settings{DiskMinFree: "20%"}); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{
		{"disk-min-free", "lots"}, {"disk-min-free", "100%"}, {"nope", "1"},
	} {
		if code, _, _ := cf(t, append([]string{"worker", "config", "set"}, args...)...); code == 0 {
			t.Errorf("%v: exit 0", args)
		}
	}
	if got, _ := doctor.LoadSettings(paths); got.DiskMinFree != "20%" {
		t.Errorf("disk_min_free = %q", got.DiskMinFree)
	}
}

func TestWorkerConfigShow_PrintsTheSettings(t *testing.T) {
	doctorMachine(t)
	if code, _, errOut := cf(t, "worker", "config", "set", "disk-min-free", "15%"); code != 0 {
		t.Fatal(errOut)
	}
	code, out, errOut := cf(t, "worker", "config", "show")
	if code != 0 || !strings.Contains(out, `"disk_min_free": "15%"`) {
		t.Errorf("exit %d: %q %q", code, out, errOut)
	}
}
