package doctor_test

import (
	"testing"
	"time"

	"github.com/Autarquico/codefabric/internal/config"
	"github.com/Autarquico/codefabric/internal/worker/doctor"
)

func TestScenario_RetentionGc_WorktreeRetentionOfTheWorkerSurvivesItsSettingsFile(t *testing.T) {
	p := config.Paths{ConfigDir: t.TempDir()}
	if s, err := doctor.LoadSettings(p); err != nil || s.Retention.Worktrees != 0 {
		t.Fatalf("unset: %+v, %v", s.Retention, err)
	}
	want := doctor.Settings{Name: "w", Retention: doctor.Retention{Worktrees: config.Duration(36 * time.Hour)}}
	if err := doctor.SaveSettings(p, want); err != nil {
		t.Fatal(err)
	}
	s, err := doctor.LoadSettings(p)
	if err != nil || time.Duration(s.Retention.Worktrees) != 36*time.Hour {
		t.Fatalf("retention.worktrees = %v, %v", time.Duration(s.Retention.Worktrees), err)
	}
}
