package domain

import "fmt"

// Markers are the explicit, persistent facts that override derivation (invariant 16).
type Markers struct {
	Cancelled         bool
	Archived          bool
	CompletedManually bool
}

// RetryView is the retry policy plus the attempts still available.
type RetryView struct {
	OnFailed     RetryAction
	OnLost       RetryAction
	AttemptsLeft int
}

// RunSummary is the part of a Run that Task derivation reads. The last Run is
// the one with the highest ShortN, the global short ID number (§11.7).
type RunSummary struct {
	ShortN      uint64
	State       RunState
	Publication Publication
	Evaluation  Evaluation
	Promoted    bool
}

// TaskView is everything DeriveTaskState reads.
type TaskView struct {
	Markers       Markers
	Completion    Completion // effective policy, already resolved Task → Plan → Project → Defaults() (§3.3)
	DepsSatisfied bool
	Autostart     bool
	Retry         RetryView
	Runs          []RunSummary
}

// DeriveTaskState applies the §11.7 table.
func DeriveTaskState(v TaskView) (TaskState, WaitingReason, error) {
	state, reason, _, err := deriveTaskRow(v)
	return state, reason, err
}

// taskRows is the §11.7 table: the first row whose condition holds wins.
// Row 1 has two outcomes and so two entries; archived wins over cancelled (§11.7, B5).
var taskRows = []struct {
	number  int
	applies func(taskFacts) bool
	state   TaskState
	reason  WaitingReason
}{
	{1, func(f taskFacts) bool { return f.Markers.Archived }, TaskArchived, WaitingNone},
	{1, func(f taskFacts) bool { return f.Markers.Cancelled }, TaskCancelled, WaitingNone},
	{2, taskFacts.completionSatisfied, TaskCompleted, WaitingNone},
	{3, func(f taskFacts) bool { return !f.DepsSatisfied }, TaskBlocked, WaitingNone},
	{4, taskFacts.hasRunInProgress, TaskRunning, WaitingNone},
	{5, func(f taskFacts) bool { return f.anyRunIn(RunWaitingInput) }, TaskWaiting, WaitingInput},
	{6, func(f taskFacts) bool { return f.anyRunIn(RunQueued) }, TaskQueued, WaitingNone},
	{7, func(f taskFacts) bool { return !f.hasRuns && f.Autostart }, TaskQueued, WaitingNone},
	{8, func(f taskFacts) bool { return !f.hasRuns && !f.Autostart }, TaskWaiting, WaitingStart},
	{9, func(f taskFacts) bool { return f.last.State == RunSuspended }, TaskWaiting, WaitingDeferred},
	{10, func(f taskFacts) bool { return f.anyRunIn(RunCompleted) }, TaskWaiting, WaitingReview},
	{11, func(f taskFacts) bool { return f.lastRunFailedOrLost() && f.lastRunRetryAction() == RetryAsk }, TaskWaiting, WaitingDecision},
	{12, func(f taskFacts) bool { return f.lastRunFailedOrLost() && f.canReschedule() }, TaskQueued, WaitingNone},
	{13, func(f taskFacts) bool { return f.last.State == RunCancelled }, TaskWaiting, WaitingDecision},
	{14, taskFacts.lastRunFailedOrLost, TaskFailed, WaitingNone},
}

// deriveTaskRow also returns the matched row number, for tests. An error means
// the completion is unknown or no row matched (a Run state outside RunState).
func deriveTaskRow(v TaskView) (TaskState, WaitingReason, int, error) {
	switch v.Completion {
	case CompletionFirstSuccess, CompletionPromote, CompletionManual:
	default:
		return "", WaitingNone, 0, fmt.Errorf("domain: unknown completion %q", v.Completion)
	}
	facts := newTaskFacts(v)
	for _, row := range taskRows {
		if row.applies(facts) {
			return row.state, row.reason, row.number, nil
		}
	}
	return "", WaitingNone, 0, fmt.Errorf("domain: no Task state row matches %+v", v)
}

type taskFacts struct {
	TaskView
	last    RunSummary
	hasRuns bool
}

func newTaskFacts(v TaskView) taskFacts {
	facts := taskFacts{TaskView: v, hasRuns: len(v.Runs) > 0}
	for _, run := range v.Runs {
		if run.ShortN >= facts.last.ShortN {
			facts.last = run
		}
	}
	return facts
}

func (f taskFacts) anyRun(pred func(RunSummary) bool) bool {
	for _, run := range f.Runs {
		if pred(run) {
			return true
		}
	}
	return false
}

func (f taskFacts) anyRunIn(state RunState) bool {
	return f.anyRun(func(r RunSummary) bool { return r.State == state })
}

func (f taskFacts) completionSatisfied() bool {
	if f.Markers.CompletedManually {
		return true
	}
	switch f.Completion {
	case CompletionFirstSuccess:
		return f.anyRun(func(r RunSummary) bool {
			return r.State == RunCompleted && r.Publication == PublicationPublished &&
				(r.Evaluation == EvaluationPassed || r.Evaluation == EvaluationNone)
		})
	case CompletionPromote:
		return f.anyRun(func(r RunSummary) bool { return r.Promoted })
	}
	return false
}

// hasRunInProgress includes finished Runs whose result is still being
// published or evaluated: the Task is not done with them yet.
func (f taskFacts) hasRunInProgress() bool {
	return f.anyRun(func(r RunSummary) bool {
		switch r.State {
		case RunPreparing, RunRunning, RunCheckpointing:
			return true
		case RunCompleted:
			return r.Evaluation == EvaluationPending || r.Publication == PublicationPending
		}
		return false
	})
}

func (f taskFacts) lastRunFailedOrLost() bool {
	return f.last.State == RunFailed || f.last.State == RunLost
}

func (f taskFacts) lastRunRetryAction() RetryAction {
	if f.last.State == RunLost {
		return f.Retry.OnLost
	}
	return f.Retry.OnFailed
}

func (f taskFacts) canReschedule() bool {
	return f.lastRunRetryAction() == RetryReschedule && f.Retry.AttemptsLeft > 0
}
