package doctor_test

import (
	"testing"

	"github.com/Autarquico/codefabric/internal/config"
	"github.com/Autarquico/codefabric/internal/worker/doctor"
)

func TestParseThreshold(t *testing.T) {
	good := map[string]doctor.Threshold{
		"20%": {Percent: 20}, "7.5%": {Percent: 7.5}, "50GiB": {Bytes: 50 << 30}, "50gb": {Bytes: 50 << 30},
		"512MiB": {Bytes: 512 << 20}, "1TiB": {Bytes: 1 << 40},
	}
	for text, want := range good {
		if got, err := doctor.ParseThreshold(text); err != nil || got != want {
			t.Errorf("%q = %+v, %v; want %+v", text, got, err, want)
		}
	}
	for _, text := range []string{"", "0%", "100%", "-5%", "abc", "50", "GiB", "0GiB", "5XB", "99999999999999TiB", "NaN%"} {
		if _, err := doctor.ParseThreshold(text); err == nil {
			t.Errorf("%q was accepted", text)
		}
	}
}

func TestSettingsPersistAndOverride(t *testing.T) {
	paths := config.Paths{ConfigDir: t.TempDir(), DataDir: t.TempDir()}
	if got, err := doctor.MinFree(paths, ""); err != nil || got.Percent != 20 {
		t.Fatalf("default = %+v, %v", got, err)
	}
	if err := doctor.SaveSettings(paths, doctor.Settings{DiskMinFree: "50GiB"}); err != nil {
		t.Fatal(err)
	}
	if got, err := doctor.MinFree(paths, ""); err != nil || got.Bytes != 50<<30 {
		t.Errorf("persisted = %+v, %v", got, err)
	}
	if got, err := doctor.MinFree(paths, "10%"); err != nil || got.Percent != 10 {
		t.Errorf("override = %+v, %v", got, err)
	}
}
