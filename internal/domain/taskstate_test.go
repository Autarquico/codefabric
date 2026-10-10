package domain

import (
	"fmt"
	"math/rand/v2"
	"testing"
	"time"
)

// rowStates lists the states each §11.7 row may produce.
var rowStates = map[int][]TaskState{
	1: {TaskCancelled, TaskArchived}, 2: {TaskCompleted}, 3: {TaskBlocked}, 4: {TaskRunning},
	5: {TaskWaiting}, 6: {TaskQueued}, 7: {TaskQueued}, 8: {TaskWaiting}, 9: {TaskWaiting},
	10: {TaskWaiting}, 11: {TaskWaiting}, 12: {TaskQueued}, 13: {TaskWaiting}, 14: {TaskFailed},
}

func rowAllows(row int, state TaskState) bool {
	for _, s := range rowStates[row] {
		if s == state {
			return true
		}
	}
	return false
}

// runClasses are the equivalence classes of one Run: the 9 states other than
// completed, and completed crossed with publication, evaluation and promoted.
func runClasses() []RunSummary {
	var classes []RunSummary
	for _, state := range allRunStates {
		if state == RunCompleted {
			continue
		}
		classes = append(classes, RunSummary{State: state, Publication: PublicationNone, Evaluation: EvaluationNone})
	}
	for _, pub := range []Publication{PublicationNone, PublicationPending, PublicationPublished, PublicationFailed} {
		for _, eval := range []Evaluation{EvaluationNone, EvaluationPending, EvaluationPassed, EvaluationFailed, EvaluationError} {
			for _, promoted := range []bool{false, true} {
				classes = append(classes, RunSummary{State: RunCompleted, Publication: pub, Evaluation: eval, Promoted: promoted})
			}
		}
	}
	return classes
}

// representativeRunClasses is one Run per way the §11.7 predicates can tell
// Runs apart: the 9 states other than completed, and completed split by what
// publication, evaluation and promotion say about it.
func representativeRunClasses() []RunSummary {
	var classes []RunSummary
	for _, state := range allRunStates {
		if state != RunCompleted {
			classes = append(classes, RunSummary{State: state, Publication: PublicationNone, Evaluation: EvaluationNone})
		}
	}
	done := func(pub Publication, eval Evaluation, promoted bool) RunSummary {
		return RunSummary{State: RunCompleted, Publication: pub, Evaluation: eval, Promoted: promoted}
	}
	return append(classes,
		done(PublicationPublished, EvaluationPassed, true),
		done(PublicationPublished, EvaluationPassed, false),
		done(PublicationPublished, EvaluationNone, false),
		done(PublicationPending, EvaluationNone, false),
		done(PublicationFailed, EvaluationNone, false),
		done(PublicationPublished, EvaluationPending, false),
		done(PublicationPublished, EvaluationFailed, false),
		done(PublicationPublished, EvaluationError, false),
	)
}

func allMarkers() []Markers {
	var all []Markers
	for bits := range 8 {
		all = append(all, Markers{Cancelled: bits&1 != 0, Archived: bits&2 != 0, CompletedManually: bits&4 != 0})
	}
	return all
}

// retryClasses cover every outcome of rows 11, 12 and 14 for failed and lost
// Runs: ask, reschedule with and without attempts, and none.
func retryClasses() []RetryView {
	return []RetryView{
		{OnFailed: RetryNone, OnLost: RetryNone, AttemptsLeft: 0},
		{OnFailed: RetryAsk, OnLost: RetryReschedule, AttemptsLeft: 1},
		{OnFailed: RetryReschedule, OnLost: RetryAsk, AttemptsLeft: 1},
		{OnFailed: RetryReschedule, OnLost: RetryReschedule, AttemptsLeft: 0},
	}
}

// expectedTaskState is a reference oracle written straight from the §11.7
// table (and §22.6 for what counts as a success), not from the production code.
func expectedTaskState(v TaskView) (TaskState, WaitingReason) {
	var last RunSummary
	for _, r := range v.Runs {
		if r.ShortN > last.ShortN {
			last = r
		}
	}
	some := func(pred func(RunSummary) bool) bool {
		for _, r := range v.Runs {
			if pred(r) {
				return true
			}
		}
		return false
	}
	published := func(r RunSummary) bool { return r.State == RunCompleted && r.Publication == PublicationPublished }

	policyMet := v.Markers.CompletedManually
	switch v.Completion {
	case CompletionFirstSuccess:
		policyMet = policyMet || some(func(r RunSummary) bool {
			return published(r) && (r.Evaluation == EvaluationPassed || r.Evaluation == EvaluationNone)
		})
	case CompletionPromote:
		policyMet = policyMet || some(func(r RunSummary) bool { return published(r) && r.Promoted })
	}
	inProgress := some(func(r RunSummary) bool {
		return r.State == RunPreparing || r.State == RunRunning || r.State == RunCheckpointing ||
			(r.State == RunCompleted && (r.Evaluation == EvaluationPending || r.Publication == PublicationPending))
	})
	lastFailedOrLost := len(v.Runs) > 0 && (last.State == RunFailed || last.State == RunLost)
	retry := v.Retry.OnFailed
	if len(v.Runs) > 0 && last.State == RunLost {
		retry = v.Retry.OnLost
	}

	switch {
	case v.Markers.Archived:
		return TaskArchived, WaitingNone
	case v.Markers.Cancelled:
		return TaskCancelled, WaitingNone
	case policyMet:
		return TaskCompleted, WaitingNone
	case !v.DepsSatisfied:
		return TaskBlocked, WaitingNone
	case inProgress:
		return TaskRunning, WaitingNone
	case some(func(r RunSummary) bool { return r.State == RunWaitingInput }):
		return TaskWaiting, WaitingInput
	case some(func(r RunSummary) bool { return r.State == RunQueued }):
		return TaskQueued, WaitingNone
	case len(v.Runs) == 0 && v.Autostart:
		return TaskQueued, WaitingNone
	case len(v.Runs) == 0:
		return TaskWaiting, WaitingStart
	case last.State == RunSuspended:
		return TaskWaiting, WaitingDeferred
	case some(func(r RunSummary) bool { return r.State == RunCompleted }):
		return TaskWaiting, WaitingReview
	case lastFailedOrLost && retry == RetryAsk:
		return TaskWaiting, WaitingDecision
	case lastFailedOrLost && retry == RetryReschedule && v.Retry.AttemptsLeft > 0:
		return TaskQueued, WaitingNone
	case last.State == RunCancelled:
		return TaskWaiting, WaitingDecision
	case lastFailedOrLost:
		return TaskFailed, WaitingNone
	}
	panic("expectedTaskState: the §11.7 table is exhaustive, nothing matched")
}

func derivedRow(v TaskView) (state TaskState, reason WaitingReason, row int, err error) {
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("panic: %v", r)
		}
	}()
	return deriveTaskRow(v)
}

func checkDerivation(t *testing.T, v TaskView) {
	t.Helper()
	state, reason, row, err := derivedRow(v)
	if err != nil || row < 1 || row > 14 {
		t.Fatalf("no row selected for %+v: row %d, err %v", v, row, err)
	}
	if !rowAllows(row, state) {
		t.Fatalf("row %d produced state %q for %+v", row, state, v)
	}
	if (state == TaskWaiting) != (reason != WaitingNone) {
		t.Fatalf("state %q with waiting_reason %q for %+v", state, reason, v)
	}
	if v.Markers.Archived && state != TaskArchived {
		t.Fatalf("archived marker gave %q for %+v", state, v)
	}
	if v.Markers.Cancelled && !v.Markers.Archived && state != TaskCancelled {
		t.Fatalf("cancelled marker gave %q for %+v", state, v)
	}
	if v.Markers.CompletedManually && !v.Markers.Cancelled && !v.Markers.Archived && state != TaskCompleted {
		t.Fatalf("completed_manually marker gave %q for %+v", state, v)
	}
}

// TestScenario_DomainCore_TaskDerivationExhaustive crosses every marker
// combination, completion policy, dependency/autostart/retry class with every
// history of 0 to 3 Runs drawn from the representative Run classes, in every
// order. Three Runs are enough because each §11.7 predicate is either
// existential over the set of Runs or reads the last Run, so a history of
// three holds the last Run plus two distinct existential witnesses, which is
// the most any single predicate pair can need. Longer histories are covered by
// the random test below. Each case must select a row and agree with the oracle.
func TestScenario_DomainCore_TaskDerivationExhaustive(t *testing.T) {
	classes := representativeRunClasses()
	completions := []Completion{CompletionFirstSuccess, CompletionPromote, CompletionManual}
	start := time.Now()
	cases := 0

	check := func(v TaskView) {
		checkDerivation(t, v)
		wantState, wantReason := expectedTaskState(v)
		gotState, gotReason, err := DeriveTaskState(v)
		if err != nil || gotState != wantState || gotReason != wantReason {
			t.Fatalf("DeriveTaskState = %s/%q, %v; oracle says %s/%q for %+v", gotState, gotReason, err, wantState, wantReason, v)
		}
		cases++
	}
	var history [3]RunSummary
	shortNs := [3]uint64{1, 2, 3}
	var walk func(v TaskView, depth, maxDepth int)
	walk = func(v TaskView, depth, maxDepth int) {
		if depth == maxDepth {
			v.Runs = history[:depth]
			check(v)
			return
		}
		for _, class := range classes {
			class.ShortN = shortNs[depth]
			history[depth] = class
			walk(v, depth+1, maxDepth)
		}
	}

	for _, markers := range allMarkers() {
		for _, completion := range completions {
			for _, deps := range []bool{false, true} {
				for _, autostart := range []bool{false, true} {
					for _, retry := range retryClasses() {
						view := TaskView{Markers: markers, Completion: completion, DepsSatisfied: deps, Autostart: autostart, Retry: retry}
						for size := range 4 {
							walk(view, 0, size)
						}
					}
				}
			}
		}
	}
	t.Logf("%d Run classes, histories of 0-3 Runs: %d cases in %v", len(classes), cases, time.Since(start))
}

func TestTaskDerivation_LastRunIsTheHighestShortNWhateverTheSliceOrder(t *testing.T) {
	base := TaskView{DepsSatisfied: true, Completion: CompletionManual, Retry: RetryView{OnFailed: RetryAsk}}
	failedLast := []RunSummary{{ShortN: 9, State: RunFailed}, {ShortN: 3, State: RunSuspended}}
	suspendedLast := []RunSummary{{ShortN: 3, State: RunFailed}, {ShortN: 9, State: RunSuspended}}
	for _, c := range []struct {
		runs []RunSummary
		row  int
	}{{failedLast, 11}, {suspendedLast, 9}} {
		for _, order := range [][]int{{0, 1}, {1, 0}} {
			v := base
			v.Runs = []RunSummary{c.runs[order[0]], c.runs[order[1]]}
			if _, _, row, err := deriveTaskRow(v); err != nil || row != c.row {
				t.Errorf("runs %+v: row %d, err %v; want row %d", v.Runs, row, err, c.row)
			}
		}
	}
}

// TestTaskDerivation_AnyRunAndLastRunRules uses random histories of 3-6 Runs
// to check the "some Run" rows (4, 5, 6, 10) and the "last Run" rows (9, 11-14).
// Completion is manual and nothing is marked, so only the table's order decides.
func TestTaskDerivation_AnyRunAndLastRunRules(t *testing.T) {
	rng := rand.New(rand.NewPCG(20261010, 3)) //nolint:gosec // deterministic test data
	all := runClasses()
	var settled, settledOrCompleted []RunSummary
	for _, c := range all {
		switch c.State {
		case RunFailed, RunLost, RunCancelled, RunSuspended:
			settled = append(settled, c)
			settledOrCompleted = append(settledOrCompleted, c)
		case RunCompleted:
			settledOrCompleted = append(settledOrCompleted, c)
		}
	}
	// Uniform draws almost always contain an in-progress Run, which hides
	// rows 9-14; the narrower pools reach them.
	pools := [][]RunSummary{all, settledOrCompleted, settled}
	retryActions := []RetryAction{RetryNone, RetryReschedule, RetryAsk}
	seen := map[int]int{}

	for range 20000 {
		classes := pools[rng.IntN(len(pools))]
		runs := make([]RunSummary, 3+rng.IntN(4))
		shortNs := []uint64{1, 2, 3, 4, 5, 6}
		rng.Shuffle(len(shortNs), func(i, j int) { shortNs[i], shortNs[j] = shortNs[j], shortNs[i] })
		for i := range runs {
			runs[i] = classes[rng.IntN(len(classes))]
			runs[i].ShortN = shortNs[i]
		}
		view := TaskView{
			Completion:    CompletionManual,
			DepsSatisfied: true,
			Retry: RetryView{
				OnFailed:     retryActions[rng.IntN(3)],
				OnLost:       retryActions[rng.IntN(3)],
				AttemptsLeft: rng.IntN(2),
			},
			Runs: runs,
		}
		_, _, row, err := deriveTaskRow(view)
		if err != nil {
			t.Fatalf("no row for %+v: %v", view, err)
		}
		seen[row]++

		last := runs[0]
		for _, r := range runs {
			if r.ShortN > last.ShortN {
				last = r
			}
		}
		has := func(pred func(RunSummary) bool) bool {
			for _, r := range runs {
				if pred(r) {
					return true
				}
			}
			return false
		}
		inProgress := has(func(r RunSummary) bool {
			return r.State == RunPreparing || r.State == RunRunning || r.State == RunCheckpointing ||
				(r.State == RunCompleted && (r.Evaluation == EvaluationPending || r.Publication == PublicationPending))
		})
		waiting := has(func(r RunSummary) bool { return r.State == RunWaitingInput })
		queued := has(func(r RunSummary) bool { return r.State == RunQueued })
		completed := has(func(r RunSummary) bool { return r.State == RunCompleted })

		want := wantRowFromTheRules(inProgress, waiting, queued, completed, last.State)
		if want != 0 && row != want {
			t.Fatalf("row %d, want %d for %+v", row, want, view)
		}
		if want == 0 && (row < 11 || row > 14) {
			t.Fatalf("row %d, want a 'last Run' row (11-14) for %+v", row, view)
		}
		checkLastRunRow(t, view, last.State, row)
	}
	for _, row := range []int{4, 5, 6, 9, 10, 11, 12, 13, 14} {
		if seen[row] == 0 {
			t.Errorf("row %d never exercised by the random histories", row)
		}
	}
}

// wantRowFromTheRules returns the row forced by the "some Run" and "last Run"
// rules, or 0 when only rows 11-14 can apply.
func wantRowFromTheRules(inProgress, waiting, queued, completed bool, last RunState) int {
	switch {
	case inProgress:
		return 4
	case waiting:
		return 5
	case queued:
		return 6
	case last == RunSuspended:
		return 9
	case completed:
		return 10
	}
	return 0
}

func checkLastRunRow(t *testing.T, view TaskView, last RunState, row int) {
	t.Helper()
	if row < 11 || row > 14 {
		return
	}
	action := view.Retry.OnFailed
	if last == RunLost {
		action = view.Retry.OnLost
	}
	want := 14
	switch {
	case last == RunCancelled:
		want = 13
	case action == RetryAsk:
		want = 11
	case action == RetryReschedule && view.Retry.AttemptsLeft > 0:
		want = 12
	}
	if row != want {
		t.Fatalf("last Run %s, retry %+v: row %d, want %d", last, view.Retry, row, want)
	}
}

func run(n uint64, state RunState) RunSummary {
	return RunSummary{ShortN: n, State: state, Publication: PublicationNone, Evaluation: EvaluationNone}
}

func completedRun(n uint64, pub Publication, eval Evaluation) RunSummary {
	return RunSummary{ShortN: n, State: RunCompleted, Publication: pub, Evaluation: eval}
}

func TestTaskDerivation_UnknownCompletionIsAnError(t *testing.T) {
	for _, completion := range []Completion{"", "sometimes"} {
		view := TaskView{DepsSatisfied: true, Completion: completion}
		if _, _, err := DeriveTaskState(view); err == nil {
			t.Errorf("DeriveTaskState with completion %q: want error", completion)
		}
		if _, _, _, err := deriveTaskRow(view); err == nil {
			t.Errorf("deriveTaskRow with completion %q: want error", completion)
		}
	}
}

func TestTaskRows_Golden(t *testing.T) {
	ready := TaskView{DepsSatisfied: true, Completion: CompletionFirstSuccess}
	with := func(base TaskView, mutate func(*TaskView)) TaskView {
		mutate(&base)
		return base
	}
	runs := func(rs ...RunSummary) func(*TaskView) {
		return func(v *TaskView) { v.Runs = rs }
	}
	retry := func(onFailed, onLost RetryAction, attempts int) func(*TaskView) {
		return func(v *TaskView) { v.Retry = RetryView{OnFailed: onFailed, OnLost: onLost, AttemptsLeft: attempts} }
	}
	both := func(fs ...func(*TaskView)) func(*TaskView) {
		return func(v *TaskView) {
			for _, f := range fs {
				f(v)
			}
		}
	}

	cases := []struct {
		name   string
		view   TaskView
		row    int
		state  TaskState
		reason WaitingReason
	}{
		// One case per row.
		{"row 1: cancelled marker", with(ready, func(v *TaskView) { v.Markers.Cancelled = true }), 1, TaskCancelled, WaitingNone},
		{"row 1: archived marker", with(ready, func(v *TaskView) { v.Markers.Archived = true }), 1, TaskArchived, WaitingNone},
		{"row 2: first_success met by a published Run without evaluators", with(ready, runs(completedRun(1, PublicationPublished, EvaluationNone))), 2, TaskCompleted, WaitingNone},
		{"row 2: first_success met by a published Run that passed", with(ready, runs(completedRun(1, PublicationPublished, EvaluationPassed))), 2, TaskCompleted, WaitingNone},
		{"row 2: promote met by a published, promoted Run", with(ready, both(runs(RunSummary{ShortN: 1, State: RunCompleted, Publication: PublicationPublished, Evaluation: EvaluationNone, Promoted: true}), func(v *TaskView) { v.Completion = CompletionPromote })), 2, TaskCompleted, WaitingNone},
		{"row 4: promote with a promoted Run still publishing", with(ready, both(runs(RunSummary{ShortN: 1, State: RunCompleted, Publication: PublicationPending, Evaluation: EvaluationNone, Promoted: true}), func(v *TaskView) { v.Completion = CompletionPromote })), 4, TaskRunning, WaitingNone},
		{"row 10: promote with a promoted Run whose publication failed", with(ready, both(runs(RunSummary{ShortN: 1, State: RunCompleted, Publication: PublicationFailed, Evaluation: EvaluationNone, Promoted: true}), func(v *TaskView) { v.Completion = CompletionPromote })), 10, TaskWaiting, WaitingReview},
		{"row 10: promote with a promoted Run without publication", with(ready, both(runs(RunSummary{ShortN: 1, State: RunCompleted, Publication: PublicationNone, Evaluation: EvaluationNone, Promoted: true}), func(v *TaskView) { v.Completion = CompletionPromote })), 10, TaskWaiting, WaitingReview},
		{"row 2: completed_manually marker", with(ready, func(v *TaskView) { v.Completion = CompletionManual; v.Markers.CompletedManually = true }), 2, TaskCompleted, WaitingNone},
		{"row 3: a dependency is not completed", with(ready, func(v *TaskView) { v.DepsSatisfied = false }), 3, TaskBlocked, WaitingNone},
		{"row 4: a Run is preparing", with(ready, runs(run(1, RunPreparing))), 4, TaskRunning, WaitingNone},
		{"row 4: a Run is running", with(ready, runs(run(1, RunRunning))), 4, TaskRunning, WaitingNone},
		{"row 4: a Run is checkpointing", with(ready, runs(run(1, RunCheckpointing))), 4, TaskRunning, WaitingNone},
		{"row 4: a completed Run is still evaluating", with(ready, runs(completedRun(1, PublicationPublished, EvaluationPending))), 4, TaskRunning, WaitingNone},
		{"row 4: a completed Run is still publishing", with(ready, runs(completedRun(1, PublicationPending, EvaluationNone))), 4, TaskRunning, WaitingNone},
		{"row 5: a Run waits for input", with(ready, runs(run(1, RunWaitingInput))), 5, TaskWaiting, WaitingInput},
		{"row 6: a Run is queued", with(ready, runs(run(1, RunQueued))), 6, TaskQueued, WaitingNone},
		{"row 7: no Runs and autostart", with(ready, func(v *TaskView) { v.Autostart = true }), 7, TaskQueued, WaitingNone},
		{"row 8: no Runs and no autostart", ready, 8, TaskWaiting, WaitingStart},
		{"row 9: last Run suspended", with(ready, runs(run(1, RunSuspended))), 9, TaskWaiting, WaitingDeferred},
		{"row 10: completed Run whose evaluators failed", with(ready, runs(completedRun(1, PublicationPublished, EvaluationFailed))), 10, TaskWaiting, WaitingReview},
		{"row 10: completed Run whose evaluators errored", with(ready, runs(completedRun(1, PublicationPublished, EvaluationError))), 10, TaskWaiting, WaitingReview},
		{"row 10: completed Run whose publication failed", with(ready, runs(completedRun(1, PublicationFailed, EvaluationNone))), 10, TaskWaiting, WaitingReview},
		{"row 10: completed Run under promote and not promoted", with(ready, both(runs(completedRun(1, PublicationPublished, EvaluationPassed)), func(v *TaskView) { v.Completion = CompletionPromote })), 10, TaskWaiting, WaitingReview},
		{"row 11: last Run failed, on_failed ask", with(ready, both(runs(run(1, RunFailed)), retry(RetryAsk, RetryNone, 0))), 11, TaskWaiting, WaitingDecision},
		{"row 11: last Run lost, on_lost ask", with(ready, both(runs(run(1, RunLost)), retry(RetryNone, RetryAsk, 0))), 11, TaskWaiting, WaitingDecision},
		{"row 12: last Run failed, reschedule with attempts left", with(ready, both(runs(run(1, RunFailed)), retry(RetryReschedule, RetryNone, 1))), 12, TaskQueued, WaitingNone},
		{"row 12: last Run lost, reschedule with attempts left", with(ready, both(runs(run(1, RunLost)), retry(RetryNone, RetryReschedule, 1))), 12, TaskQueued, WaitingNone},
		{"row 13: last Run cancelled", with(ready, runs(run(1, RunCancelled))), 13, TaskWaiting, WaitingDecision},
		{"row 14: last Run failed, no retry", with(ready, both(runs(run(1, RunFailed)), retry(RetryNone, RetryNone, 0))), 14, TaskFailed, WaitingNone},
		{"row 14: last Run failed, reschedule without attempts", with(ready, both(runs(run(1, RunFailed)), retry(RetryReschedule, RetryNone, 0))), 14, TaskFailed, WaitingNone},
		{"row 14: last Run lost, on_lost none", with(ready, both(runs(run(1, RunLost)), retry(RetryAsk, RetryNone, 1))), 14, TaskFailed, WaitingNone},

		// Precedence: the case fits several rows and the first wins.
		{"cancelled + unmet deps + active Run is row 1", with(ready, both(runs(run(1, RunRunning)), func(v *TaskView) { v.Markers.Cancelled = true; v.DepsSatisfied = false })), 1, TaskCancelled, WaitingNone},
		{"archived and cancelled is archived", with(ready, func(v *TaskView) { v.Markers.Cancelled = true; v.Markers.Archived = true }), 1, TaskArchived, WaitingNone},
		{"cancelled beats completed_manually", with(ready, func(v *TaskView) { v.Markers.Cancelled = true; v.Markers.CompletedManually = true }), 1, TaskCancelled, WaitingNone},
		{"archived beats a satisfied policy", with(ready, both(runs(completedRun(1, PublicationPublished, EvaluationPassed)), func(v *TaskView) { v.Markers.Archived = true })), 1, TaskArchived, WaitingNone},
		{"completed_manually satisfies row 2 under first_success", with(ready, func(v *TaskView) { v.Markers.CompletedManually = true }), 2, TaskCompleted, WaitingNone},
		{"completed_manually satisfies row 2 under promote", with(ready, func(v *TaskView) { v.Completion = CompletionPromote; v.Markers.CompletedManually = true }), 2, TaskCompleted, WaitingNone},
		{"completed_manually beats unmet deps and an active Run", with(ready, both(runs(run(1, RunRunning)), func(v *TaskView) { v.Markers.CompletedManually = true; v.DepsSatisfied = false })), 2, TaskCompleted, WaitingNone},
		{"a satisfied policy beats an active Run", with(ready, runs(completedRun(1, PublicationPublished, EvaluationPassed), run(2, RunRunning))), 2, TaskCompleted, WaitingNone},
		{"unmet deps beat an active Run", with(ready, both(runs(run(1, RunRunning)), func(v *TaskView) { v.DepsSatisfied = false })), 3, TaskBlocked, WaitingNone},
		{"unmet deps beat autostart", with(ready, func(v *TaskView) { v.DepsSatisfied = false; v.Autostart = true }), 3, TaskBlocked, WaitingNone},
		{"running beats waiting_input", with(ready, runs(run(1, RunWaitingInput), run(2, RunRunning))), 4, TaskRunning, WaitingNone},
		{"waiting_input beats queued", with(ready, runs(run(1, RunQueued), run(2, RunWaitingInput))), 5, TaskWaiting, WaitingInput},
		{"an older Run waiting for input beats a newer failed one", with(ready, both(runs(run(1, RunWaitingInput), run(2, RunFailed)), retry(RetryAsk, RetryAsk, 1))), 5, TaskWaiting, WaitingInput},
		{"queued beats a suspended last Run", with(ready, runs(run(1, RunQueued), run(2, RunSuspended))), 6, TaskQueued, WaitingNone},
		{"last Run suspended beats an older completed Run", with(ready, runs(completedRun(1, PublicationPublished, EvaluationFailed), run(2, RunSuspended))), 9, TaskWaiting, WaitingDeferred},
		{"old completed Run + last Run failed is row 10, not row 14", with(ready, both(runs(completedRun(1, PublicationPublished, EvaluationFailed), run(2, RunFailed)), retry(RetryNone, RetryNone, 0))), 10, TaskWaiting, WaitingReview},
		{"old completed Run + last Run failed with ask is row 10, not row 11", with(ready, both(runs(completedRun(1, PublicationFailed, EvaluationNone), run(2, RunLost)), retry(RetryAsk, RetryAsk, 1))), 10, TaskWaiting, WaitingReview},
		{"old completed Run + last Run cancelled is row 10, not row 13", with(ready, runs(completedRun(1, PublicationPublished, EvaluationError), run(2, RunCancelled))), 10, TaskWaiting, WaitingReview},
		{"ask beats reschedule when both apply", with(ready, both(runs(run(1, RunFailed)), retry(RetryAsk, RetryReschedule, 1))), 11, TaskWaiting, WaitingDecision},
		{"a lost Run reads on_lost, not on_failed", with(ready, both(runs(run(1, RunLost)), retry(RetryAsk, RetryNone, 1))), 14, TaskFailed, WaitingNone},
		{"the last Run decides the retry row, not an older failure", with(ready, both(runs(run(1, RunFailed), run(2, RunCancelled)), retry(RetryAsk, RetryAsk, 1))), 13, TaskWaiting, WaitingDecision},
	}
	covered := map[int]bool{}
	for _, c := range cases {
		covered[c.row] = true
		t.Run(c.name, func(t *testing.T) {
			state, reason, row, err := deriveTaskRow(c.view)
			if err != nil || row != c.row || state != c.state || reason != c.reason {
				t.Fatalf("got row %d %s/%q (err %v), want row %d %s/%q", row, state, reason, err, c.row, c.state, c.reason)
			}
			publicState, publicReason, err := DeriveTaskState(c.view)
			if err != nil || publicState != c.state || publicReason != c.reason {
				t.Fatalf("DeriveTaskState = %s/%q, %v; want %s/%q", publicState, publicReason, err, c.state, c.reason)
			}
		})
	}
	for row := 1; row <= 14; row++ {
		if !covered[row] {
			t.Errorf("row %d has no golden case", row)
		}
	}
}
