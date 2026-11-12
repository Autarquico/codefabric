package domain

import "testing"

func TestDeriveTaskState_FailedMoveWaitsForDecisionUntilCleared(t *testing.T) {
	running := TaskView{Completion: CompletionFirstSuccess, DepsSatisfied: true, Runs: []RunSummary{{ShortN: 1, State: RunRunning}}}

	pending := running
	pending.Markers.DecisionPending = true
	state, reason, row, err := deriveTaskRow(pending)
	if err != nil || state != TaskWaiting || reason != WaitingDecision || row != moveDecisionRow {
		t.Errorf("pending: got %s/%s row %d err %v, want waiting/decision", state, reason, row, err)
	}

	if state, _, _, _ := deriveTaskRow(running); state != TaskRunning {
		t.Errorf("cleared: got %s, want running", state)
	}

	for _, m := range []Markers{{DecisionPending: true, Cancelled: true}, {DecisionPending: true, Archived: true}} {
		v := running
		v.Markers = m
		if state, _, _, _ := deriveTaskRow(v); state != TaskCancelled && state != TaskArchived {
			t.Errorf("%+v: a cancel or archive must win over a pending decision, got %s", m, state)
		}
	}
}
