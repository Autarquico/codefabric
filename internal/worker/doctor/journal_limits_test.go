package doctor_test

import (
	"testing"

	"github.com/Autarquico/codefabric/internal/config"
	"github.com/Autarquico/codefabric/internal/worker/doctor"
)

func TestJournalLimits(t *testing.T) {
	for _, c := range []struct {
		name       string
		settings   *doctor.Settings
		max, hard  uint64
		wantFailed bool
	}{
		{"no config is the defaults of Appendix A", nil, 1 << 30, 5 << 30, false},
		{"empty sizes are the defaults", &doctor.Settings{}, 1 << 30, 5 << 30, false},
		{"both chosen", &doctor.Settings{JournalMaxSize: "2GiB", JournalHardMax: "8 GiB"}, 2 << 30, 8 << 30, false},
		{"one chosen", &doctor.Settings{JournalMaxSize: "100MB"}, 100 << 20, 5 << 30, false},
		{"not a size", &doctor.Settings{JournalMaxSize: "lots"}, 1 << 30, 5 << 30, true},
		{"zero", &doctor.Settings{JournalHardMax: "0GB"}, 1 << 30, 5 << 30, true},
		{"hard_max under max_size", &doctor.Settings{JournalMaxSize: "6GiB"}, 1 << 30, 5 << 30, true},
	} {
		t.Run(c.name, func(t *testing.T) {
			p := config.Paths{ConfigDir: t.TempDir()}
			if c.settings != nil {
				if err := doctor.SaveSettings(p, *c.settings); err != nil {
					t.Fatal(err)
				}
			}
			maxSize, hardMax, err := doctor.JournalLimits(p)
			if (err != nil) != c.wantFailed {
				t.Fatalf("err = %v, want failure %v", err, c.wantFailed)
			}
			if maxSize != c.max || hardMax != c.hard {
				t.Errorf("limits = %d, %d; want %d, %d", maxSize, hardMax, c.max, c.hard)
			}
		})
	}
}
