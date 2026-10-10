package domain

import (
	"slices"
	"testing"
	"time"
)

func TestDefaults_BudgetAndPromoteAppendixA(t *testing.T) {
	d := Defaults()
	if d.BudgetOnExceed != BudgetBlockNew {
		t.Errorf("BudgetOnExceed = %q, want block_new", d.BudgetOnExceed)
	}
	if d.BudgetOnExceedMaxDuration != BudgetCheckpointStop {
		t.Errorf("BudgetOnExceedMaxDuration = %q, want checkpoint_stop", d.BudgetOnExceedMaxDuration)
	}
	if d.PromoteStrategy != "branch" || !d.PromoteRequireConfirmation {
		t.Errorf("promote = %q, require confirmation %v; want branch, true", d.PromoteStrategy, d.PromoteRequireConfirmation)
	}
	if d.BudgetWarnAt != 0.8 || d.BudgetOnUnobservable != BudgetUnobservableWarn {
		t.Errorf("budget warn_at %v, on_unobservable %q", d.BudgetWarnAt, d.BudgetOnUnobservable)
	}
	if d.ConfirmationTTL != 24*time.Hour {
		t.Errorf("ConfirmationTTL = %v", d.ConfirmationTTL)
	}
}

func TestDefaults_CheckpointExcludeIsNotShared(t *testing.T) {
	want := []string{".env*", "*.pem", "*.key"}
	first := Defaults()
	first.CheckpointExclude[0] = "mutated"
	first.CheckpointExclude = append(first.CheckpointExclude, "extra")
	if got := Defaults().CheckpointExclude; !slices.Equal(got, want) {
		t.Fatalf("second Defaults().CheckpointExclude = %v, want %v", got, want)
	}
}
