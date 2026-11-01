package setup_test

import (
	"os"
	"reflect"
	"testing"

	"github.com/Autarquico/codefabric/internal/config"
	"github.com/Autarquico/codefabric/internal/worker/doctor"
	"github.com/Autarquico/codefabric/internal/worker/setup"
)

func paths(t *testing.T) config.Paths {
	return config.Paths{ConfigDir: t.TempDir()}
}

func TestProgressRoundTripsAndKeepsOtherSettings(t *testing.T) {
	p := paths(t)
	if err := doctor.SaveSettings(p, doctor.Settings{Name: "w1", DiskMinFree: "10%"}); err != nil {
		t.Fatal(err)
	}
	want := setup.Progress{
		Step: 3, Name: "w1", DiskMinFree: "15%",
		Harnesses: []string{"claude", "codex"}, InitialWorkflow: "default",
	}
	if err := setup.SaveProgress(p, want); err != nil {
		t.Fatal(err)
	}
	got, pending, err := setup.Load(p)
	if err != nil || !pending || !reflect.DeepEqual(got, want) {
		t.Fatalf("got %+v pending=%v err=%v", got, pending, err)
	}
	if s, _ := doctor.LoadSettings(p); s.Name != "w1" || s.DiskMinFree != "10%" {
		t.Fatalf("other settings lost: %+v", s)
	}
}

func TestMarkDoneClearsOnlyThePendingFlag(t *testing.T) {
	p := paths(t)
	if err := setup.SaveProgress(p, setup.Progress{Step: 5, Name: "w1"}); err != nil {
		t.Fatal(err)
	}
	if err := setup.MarkDone(p); err != nil {
		t.Fatal(err)
	}
	got, pending, _ := setup.Load(p)
	if pending || got.Step != 5 || got.Name != "w1" || doctor.SetupPending(p) {
		t.Fatalf("got %+v pending=%v", got, pending)
	}
}

func TestAConfigFromBeforeTheWizardIsDone(t *testing.T) {
	p := paths(t)
	if err := os.MkdirAll(p.ConfigDir+"/worker", 0o700); err != nil {
		t.Fatal(err)
	}
	old := `{"name":"w0","disk_min_free":"20%"}`
	if err := os.WriteFile(doctor.SettingsFile(p), []byte(old), 0o600); err != nil {
		t.Fatal(err)
	}
	got, pending, err := setup.Load(p)
	if err != nil || pending || !reflect.DeepEqual(got, setup.Progress{}) {
		t.Fatalf("got %+v pending=%v err=%v", got, pending, err)
	}
	if s, _ := doctor.LoadSettings(p); s.Name != "w0" || doctor.SetupPending(p) {
		t.Fatalf("%+v", s)
	}
}
