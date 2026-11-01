// Package setup keeps the state of the join wizard in the Worker config, so that
// an interrupted join resumes where it stopped and the Worker takes no Runs
// until it finishes.
package setup

import (
	"github.com/Autarquico/codefabric/internal/config"
	"github.com/Autarquico/codefabric/internal/worker/doctor"
)

// Progress is where the wizard stopped and its answers.
type Progress = doctor.Progress

// Load returns the saved progress and whether the join is pending. A Worker
// without wizard state is done, with zero progress.
func Load(p config.Paths) (Progress, bool, error) {
	s, err := doctor.LoadSettings(p)
	if err != nil {
		return Progress{}, false, err
	}
	var pr Progress
	if s.Wizard != nil {
		pr = *s.Wizard
	}
	return pr, s.SetupPending, nil
}

// SaveProgress replaces the progress and leaves the Worker pending.
func SaveProgress(p config.Paths, pr Progress) error {
	return update(p, func(s *doctor.Settings) { s.SetupPending, s.Wizard = true, &pr })
}

// MarkDone ends the setup: the Worker takes Runs again. It clears the pending
// flag and nothing else, so the saved answers stay.
func MarkDone(p config.Paths) error {
	return update(p, func(s *doctor.Settings) { s.SetupPending = false })
}

func update(p config.Paths, f func(*doctor.Settings)) error {
	s, err := doctor.LoadSettings(p)
	if err != nil {
		return err
	}
	f(&s)
	return doctor.SaveSettings(p, s)
}
