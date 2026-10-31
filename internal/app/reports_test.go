package app_test

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	v1 "github.com/Autarquico/codefabric/internal/api/v1"
	"github.com/Autarquico/codefabric/internal/controller/pki"
	"github.com/Autarquico/codefabric/internal/controller/store"
	"github.com/Autarquico/codefabric/internal/domain"
)

const reportEpoch = "01J00000000000000000000000"

// reporter builds the entries of one Worker daemon, with the seq it never reuses.
type reporter struct {
	e     *env
	w     pki.Session
	run   v1.Run
	lease store.Lease
	epoch string
	seq   uint64
}

// acceptedRun gives a Run to the Worker and has it accept: the Run is preparing.
func (e *env) acceptedRun(w pki.Session, prompt string) *reporter {
	e.t.Helper()
	run := e.queuedRun(prompt)
	l := e.offer(run.ID, w)
	_, err := e.accept(w, l)
	must(e.t, err)
	return &reporter{e: e, w: w, run: run, lease: l, epoch: reportEpoch}
}

func (r *reporter) entry(kind v1.RunReportKind, mutate ...func(*v1.RunReportEntry)) v1.RunReportEntry {
	r.seq++
	en := v1.RunReportEntry{
		WorkerEpoch: r.epoch, WorkerSeq: r.seq, LeaseEpoch: r.lease.Epoch, Kind: kind, At: r.e.clock.Now(),
	}
	if kind == v1.ReportStarted {
		en.Handle = &v1.RuntimeHandle{Runtime: "tmux", Ref: "cf-abc/cf-" + r.run.ID + "-e1"}
	}
	if kind == v1.ReportExited {
		en.Cause = v1.ExitCauseExit
	}
	for _, m := range mutate {
		m(&en)
	}
	return en
}

func exitCode(n int) func(*v1.RunReportEntry) {
	return func(en *v1.RunReportEntry) { en.ExitCode = &n }
}

func neverStarted(en *v1.RunReportEntry) { en.NeverStarted = true }

func (r *reporter) send(entries ...v1.RunReportEntry) (v1.RunReportResponse, error) {
	r.e.t.Helper()
	return r.e.app.Runs.Report(r.e.t.Context(), r.w, r.run.ID, v1.RunReportRequest{Entries: entries})
}

func (r *reporter) mustSend(entries ...v1.RunReportEntry) v1.RunReportResponse {
	r.e.t.Helper()
	out, err := r.send(entries...)
	must(r.e.t, err)
	return out
}

func results(out v1.RunReportResponse) string {
	var parts []string
	for _, o := range out.Outcomes {
		part := o.Result
		if o.Code != "" {
			part += ":" + string(o.Code)
		}
		parts = append(parts, part)
	}
	return strings.Join(parts, ",")
}

func TestReport_StartedExitedStoppedInOneBatchLeaveTheRunFailedAndPublicationPending(t *testing.T) {
	e := newEnv(t)
	w := e.joinWorker("alpha")
	r := e.acceptedRun(w, "x")

	out := r.mustSend(r.entry(v1.ReportStarted), r.entry(v1.ReportExited, exitCode(7)), r.entry(v1.ReportStopped))
	if got := results(out); got != "applied,applied,applied" {
		t.Fatalf("outcomes = %s", got)
	}
	got := e.storedRun(r.run.ID)
	if got.State != domain.RunFailed || got.EndReason != domain.EndExitCode || got.ExitCode == nil || *got.ExitCode != 7 ||
		got.Publication != domain.PublicationPending || got.Runtime != domain.RuntimeStopped || got.FinishedAt == nil || got.StartedAt == nil {
		t.Errorf("Run = %+v", got)
	}
	if !strings.Contains(got.RuntimeHandle, `"tmux"`) {
		t.Errorf("handle = %q", got.RuntimeHandle)
	}
	if err := domain.ValidateRun(domain.Run{
		State: got.State, EndReason: got.EndReason, Publication: got.Publication, Evaluation: got.Evaluation,
		Runtime: got.Runtime, FinishedAt: got.FinishedAt,
	}); err != nil {
		t.Errorf("the Run breaks an invariant: %v", err)
	}
	if out.Run.State != domain.RunFailed || out.Run.ExitCode == nil {
		t.Errorf("answer Run = %+v", out.Run)
	}
	for _, typ := range []domain.EventType{domain.EventRunStarted, domain.EventRunFailed, domain.EventRunStopped} {
		evs := e.events(typ)
		if len(evs) != 1 || evs[0].WorkerSeq == nil || evs[0].WorkerEpoch != reportEpoch || evs[0].Actor.Kind != domain.ActorWorker {
			t.Errorf("%s events = %+v", typ, evs)
		}
	}
	if res := e.reservation(r.lease.ID); res.ReleasedAt != nil {
		t.Errorf("the reservation of a Run that is not sealed was released: %+v", res)
	}
	r.mustSend(r.entry(v1.ReportSealed, sealedTo(sha1)))
	if res := e.reservation(r.lease.ID); res.ReleasedAt == nil || res.ReleaseReason != "run_stopped" {
		t.Errorf("reservation = %+v", res)
	}
	if task, err := e.app.Tasks.Get(t.Context(), r.run.TaskID, ""); err != nil || task.State != domain.TaskFailed {
		t.Errorf("Task = %v (%v)", task.State, err)
	}
}

func TestReport_AStartedWithAModelPutsItInRunStartedAndOnTheRun(t *testing.T) {
	e := newEnv(t)
	r := e.acceptedRun(e.joinWorker("alpha"), "x")

	out := r.mustSend(r.entry(v1.ReportStarted, func(en *v1.RunReportEntry) { en.Model = "claude-opus-5-5" }))
	if got := results(out); got != "applied" {
		t.Fatalf("outcomes = %s", got)
	}
	if got := e.storedRun(r.run.ID).Model; got != "claude-opus-5-5" {
		t.Errorf("model = %q", got)
	}
	evs := e.events(domain.EventRunStarted)
	if len(evs) != 1 || evs[0].Data["model"] != "claude-opus-5-5" {
		t.Errorf("RunStarted events = %+v", evs)
	}
}

func TestReport_ExitZeroCompletesTheRunWithPublicationPending(t *testing.T) {
	e := newEnv(t)
	r := e.acceptedRun(e.joinWorker("alpha"), "x")
	r.mustSend(r.entry(v1.ReportStarted), r.entry(v1.ReportExited, exitCode(0)))
	got := e.storedRun(r.run.ID)
	if got.State != domain.RunCompleted || got.Publication != domain.PublicationPending || got.ExitCode == nil || *got.ExitCode != 0 {
		t.Errorf("Run = %+v", got)
	}
	if got.Runtime != domain.RuntimeActive {
		t.Errorf("runtime = %s: it is active until the stop is confirmed", got.Runtime)
	}
	if res := e.reservation(r.lease.ID); res.ReleasedAt != nil {
		t.Error("the reservation was released before RunStopped")
	}
}

func TestReport_AWrapperLostAfterStartFailsTheRunWithoutAnExitCode(t *testing.T) {
	e := newEnv(t)
	r := e.acceptedRun(e.joinWorker("alpha"), "x")
	r.mustSend(r.entry(v1.ReportStarted), r.entry(v1.ReportExited, func(en *v1.RunReportEntry) { en.Cause = v1.ExitCauseWrapperLost }), r.entry(v1.ReportStopped))
	got := e.storedRun(r.run.ID)
	if got.State != domain.RunFailed || got.EndReason != domain.EndWrapperLost || got.ExitCode != nil || got.Publication != domain.PublicationPending {
		t.Errorf("Run = %+v", got)
	}
}

func TestReport_PrepareFailedFailsTheRunAndLeavesPublicationNone(t *testing.T) {
	e := newEnv(t)
	r := e.acceptedRun(e.joinWorker("alpha"), "x")
	out := r.mustSend(r.entry(v1.ReportPrepareFailed, func(en *v1.RunReportEntry) { en.Message = "no tmux" }), r.entry(v1.ReportStopped, neverStarted))
	if got := results(out); got != "applied,applied" {
		t.Fatalf("outcomes = %s", got)
	}
	got := e.storedRun(r.run.ID)
	if got.State != domain.RunFailed || got.Publication != domain.PublicationNone || got.Runtime != domain.RuntimeStopped || got.StartedAt != nil {
		t.Errorf("Run = %+v", got)
	}
	if res := e.reservation(r.lease.ID); res.ReleasedAt == nil {
		t.Error("a Run that never started kept its reservation")
	}
}

func TestReport_AStoppedThatWarnsOfAServiceReleasesTheRunAndRaisesEnvironmentStopFailed(t *testing.T) {
	e := newEnv(t)
	r := e.acceptedRun(e.joinWorker("alpha"), "x")
	out := r.mustSend(r.entry(v1.ReportPrepareFailed, func(en *v1.RunReportEntry) { en.Message = "make deps failed" }),
		r.entry(v1.ReportStopped, neverStarted, func(en *v1.RunReportEntry) {
			en.StopFailures = []v1.StopFailure{{Service: "db", Error: "stop exited with 1"}}
		}))
	if got := results(out); got != "applied,applied" {
		t.Fatalf("outcomes = %s", got)
	}
	if res := e.reservation(r.lease.ID); res.ReleasedAt == nil {
		t.Error("a warning held the reservation")
	}
	evs := e.events(domain.EventEnvironmentStopFailed)
	if len(evs) != 1 || evs[0].Level != domain.LevelWarn || !jsonIs(evs[0].Data["failures"], `[{"error":"stop exited with 1","service":"db"}]`) || string(evs[0].RunID) != string(r.run.ID) {
		t.Fatalf("EnvironmentStopFailed = %+v", evs)
	}
	if evs := e.events(domain.EventRunStopped); len(evs) != 1 {
		t.Errorf("RunStopped = %+v", evs)
	}
}

func jsonIs(v any, want string) bool {
	b, err := json.Marshal(v)
	return err == nil && string(b) == want
}

func TestReport_AStoppedWithOnlyAMessageRaisesNoEnvironmentStopFailed(t *testing.T) {
	e := newEnv(t)
	r := e.acceptedRun(e.joinWorker("alpha"), "x")
	out := r.mustSend(r.entry(v1.ReportStopped, neverStarted, func(en *v1.RunReportEntry) { en.Message = "nothing of the Run is on this Worker" }))
	if got := results(out); got != "applied" {
		t.Fatalf("outcomes = %s", got)
	}
	if evs := e.events(domain.EventEnvironmentStopFailed); len(evs) != 0 {
		t.Errorf("EnvironmentStopFailed = %+v: a message of a stopped is not a stop failure", evs)
	}
}

func TestScenario_WorkerExecutorTmux_StopAntesDeArrancar_Controller(t *testing.T) {
	e := newEnv(t)
	w := e.joinWorker("alpha")
	r := e.acceptedRun(w, "x")
	if _, err := e.app.Tasks.Cancel(t.Context(), e.owner, r.run.TaskID, "", v1.ActionRequest{}); err != nil {
		t.Fatal(err)
	}
	if got := e.storedRun(r.run.ID); got.State != domain.RunCancelled || got.Runtime != domain.RuntimeNone {
		t.Fatalf("Run after cancel = %+v", got)
	}
	if res := e.reservation(r.lease.ID); res.ReleasedAt != nil {
		t.Fatal("the reservation of a cancelled, accepted Run goes only with RunStopped")
	}

	out := r.mustSend(r.entry(v1.ReportStopped, neverStarted))
	if got := results(out); got != "applied" {
		t.Fatalf("outcomes = %s", got)
	}
	if got := e.storedRun(r.run.ID); got.Runtime != domain.RuntimeStopped || got.State != domain.RunCancelled {
		t.Errorf("Run after RunStopped = %+v", got)
	}
	if res := e.reservation(r.lease.ID); res.ReleasedAt == nil || res.ReleaseReason != "run_stopped" {
		t.Errorf("reservation = %+v", res)
	}
	if evs := e.events(domain.EventRunStopped); len(evs) != 1 || evs[0].Data["never_started"] != true {
		t.Errorf("RunStopped = %+v", evs)
	}
	if got := e.storedWorker(w); got.Reservations != 0 {
		t.Errorf("the Worker still holds %d reservations", got.Reservations)
	}
}

func TestReleasable_StartedRunIsReleasedOnlyOnceSealed(t *testing.T) {
	e := newEnv(t)
	for name, closing := range map[string]func(r *reporter) v1.RunReportEntry{
		"sealed": func(r *reporter) v1.RunReportEntry { return r.entry(v1.ReportSealed, sealedTo(sha1)) },
		"publication failed": func(r *reporter) v1.RunReportEntry {
			return r.entry(v1.ReportPublicationFailed, func(en *v1.RunReportEntry) { en.Phase = v1.PhaseSeal })
		},
	} {
		r := e.acceptedRun(e.joinWorker(strings.ReplaceAll(name, " ", "-")), "x")
		r.mustSend(r.entry(v1.ReportStarted), r.entry(v1.ReportExited, exitCode(0)))
		if res := e.reservation(r.lease.ID); res.ReleasedAt != nil {
			t.Fatalf("%s: released before RunStopped", name)
		}
		r.mustSend(r.entry(v1.ReportStopped))
		if res := e.reservation(r.lease.ID); res.ReleasedAt != nil {
			t.Fatalf("%s: a stopped Run that is not sealed released its reservation", name)
		}
		r.mustSend(closing(r))
		if res := e.reservation(r.lease.ID); res.ReleasedAt == nil {
			t.Errorf("%s: the reservation of a sealed (or given-up) Run was kept", name)
		}
	}
}

func TestReport_AStopWhileTheRunIsNotTerminalKeepsTheReservation(t *testing.T) {
	e := newEnv(t)
	r := e.acceptedRun(e.joinWorker("alpha"), "x")
	r.mustSend(r.entry(v1.ReportStopped, neverStarted))
	if got := e.storedRun(r.run.ID); got.Runtime != domain.RuntimeStopped || got.State != domain.RunPreparing {
		t.Errorf("Run = %+v", got)
	}
	if res := e.reservation(r.lease.ID); res.ReleasedAt != nil {
		t.Error("a non-terminal Run released its reservation")
	}
}

func TestReport_TheFirstRejectionStopsTheBatchAndWritesNothingFromThere(t *testing.T) {
	e := newEnv(t)
	r := e.acceptedRun(e.joinWorker("alpha"), "x")
	// exited before started: invalid for a preparing Run.
	out := r.mustSend(r.entry(v1.ReportExited, exitCode(0)), r.entry(v1.ReportStarted), r.entry(v1.ReportStopped))
	if got := results(out); got != "rejected:CF_INVALID_TRANSITION,not_applied,not_applied" {
		t.Fatalf("outcomes = %s", got)
	}
	if got := e.storedRun(r.run.ID); got.State != domain.RunPreparing || got.Runtime != domain.RuntimeNone {
		t.Errorf("Run = %+v", got)
	}
	if n := len(e.events(domain.EventRunStarted, domain.EventRunCompleted, domain.EventRunFailed, domain.EventRunStopped)); n != 0 {
		t.Errorf("%d events were written", n)
	}

	// In the middle: what precedes stays.
	r2 := e.acceptedRun(e.sessionOf("alpha"), "y")
	out = r2.mustSend(r2.entry(v1.ReportStarted), r2.entry(v1.ReportStarted), r2.entry(v1.ReportStopped))
	if got := results(out); got != "applied,rejected:CF_INVALID_TRANSITION,not_applied" {
		t.Fatalf("outcomes = %s", got)
	}
	if got := e.storedRun(r2.run.ID); got.State != domain.RunRunning {
		t.Errorf("Run = %+v", got)
	}
}

func TestScenario_WorkerExecutorTmux_ReenvioDelJournal_Controller(t *testing.T) {
	e := newEnv(t)
	w := e.joinWorker("alpha")
	var batches []*reporter
	for i, prompt := range []string{"a", "b", "c"} {
		r := e.acceptedRun(w, prompt)
		r.seq = uint64(10 * i) // one Worker numbers all its entries: runs do not share seqs
		batches = append(batches, r)
	}
	type sent struct {
		r       *reporter
		entries []v1.RunReportEntry
	}
	var all []sent
	for _, r := range batches {
		all = append(all, sent{r, []v1.RunReportEntry{r.entry(v1.ReportStarted), r.entry(v1.ReportExited, exitCode(3)), r.entry(v1.ReportStopped)}})
	}
	for _, s := range all {
		if got := results(s.r.mustSend(s.entries...)); got != "applied,applied,applied" {
			t.Fatalf("first send = %s", got)
		}
	}
	events, states := len(e.events()), e.dump("runs")

	// The response was lost: everything is sent again, the same.
	for _, s := range all {
		out := s.r.mustSend(s.entries...)
		if got := results(out); got != "duplicate,duplicate,duplicate" {
			t.Fatalf("second send = %s", got)
		}
		if out.Run.State != domain.RunFailed {
			t.Errorf("Run = %+v", out.Run)
		}
	}
	if len(e.events()) != events || e.dump("runs") != states {
		t.Error("a repeated report changed the store")
	}
	if n := len(e.events(domain.EventRunStarted)); n != 3 {
		t.Errorf("%d RunStarted events", n)
	}
}

func TestReport_ASeqReusedForADifferentReportIsRejected(t *testing.T) {
	e := newEnv(t)
	r := e.acceptedRun(e.joinWorker("alpha"), "x")
	first := r.entry(v1.ReportStarted)
	r.mustSend(first)

	other := first
	other.Kind, other.Handle, other.Cause = v1.ReportExited, nil, v1.ExitCauseExit
	out := r.mustSend(other)
	if got := results(out); got != "rejected:CF_INVALID_REQUEST" || !strings.Contains(out.Outcomes[0].Message, "worker_seq reused") {
		t.Errorf("outcomes = %+v", out.Outcomes)
	}
	// The same seq of a different epoch is another report.
	again := first
	again.WorkerEpoch = "01J00000000000000000000001"
	again.Kind, again.Handle, again.Cause = v1.ReportExited, nil, v1.ExitCauseExit
	if got := results(r.mustSend(again)); got != "applied" {
		t.Errorf("a new epoch with the same seq: %s", got)
	}
}

func TestReport_AReplayOfAnEntryThatTheStateHasPassedIsStillADuplicate(t *testing.T) {
	e := newEnv(t)
	r := e.acceptedRun(e.joinWorker("alpha"), "x")
	started := r.entry(v1.ReportStarted)
	r.mustSend(started, r.entry(v1.ReportExited, exitCode(0)))
	if got := results(r.mustSend(started)); got != "duplicate" {
		t.Errorf("replay = %s", got)
	}
}

func TestReport_AnotherStartedWithANewSeqIsNotADuplicate(t *testing.T) {
	e := newEnv(t)
	r := e.acceptedRun(e.joinWorker("alpha"), "x")
	r.mustSend(r.entry(v1.ReportStarted))
	if got := results(r.mustSend(r.entry(v1.ReportStarted))); got != "rejected:CF_INVALID_TRANSITION" {
		t.Errorf("a second started = %s", got)
	}
}

func TestReport_FencingAndAuthorization(t *testing.T) {
	e := newEnv(t)
	alpha, beta := e.joinWorker("alpha"), e.joinWorker("beta")
	r := e.acceptedRun(alpha, "x")

	// A Worker that was never offered the Run learns nothing about it.
	other := &reporter{e: e, w: beta, run: r.run, lease: r.lease, epoch: reportEpoch}
	_, err := other.send(other.entry(v1.ReportStarted))
	wantCode(t, err, domain.CodeNotFound)
	_, err = e.app.Runs.Report(t.Context(), alpha, "run_01ARZ3NDEKTSV4RRFFQ69G5FAV", v1.RunReportRequest{Entries: []v1.RunReportEntry{r.entry(v1.ReportStarted)}})
	wantCode(t, err, domain.CodeNotFound)
	_, err = e.app.Runs.Report(t.Context(), alpha, "nonsense", v1.RunReportRequest{Entries: []v1.RunReportEntry{r.entry(v1.ReportStarted)}})
	wantCode(t, err, domain.CodeNotFound)
	_, err = e.app.Runs.Report(t.Context(), e.owner, r.run.ID, v1.RunReportRequest{Entries: []v1.RunReportEntry{r.entry(v1.ReportStarted)}})
	wantCode(t, err, domain.CodeUnauthenticated) // a client is not a Worker: an error of the identity, not of the Run

	// An epoch the Worker never had.
	bad := r.entry(v1.ReportStarted, func(en *v1.RunReportEntry) { en.LeaseEpoch = 9 })
	if got := results(r.mustSend(bad)); got != "rejected:CF_NOT_FOUND" {
		t.Errorf("unknown epoch = %s", got)
	}
	if got := e.storedRun(r.run.ID); got.State != domain.RunPreparing {
		t.Errorf("a rejected report moved the Run: %+v", got)
	}
}

func TestReport_AnObsoleteEpochIsStale(t *testing.T) {
	e := newEnv(t)
	w := e.joinWorker("alpha")
	r := e.acceptedRun(w, "x")
	// The Run is given another epoch, as a lost Run would be: simulate with a
	// second lease row for the same Run.
	must(t, e.db.WithTx(t.Context(), func(tx *store.Tx) error {
		_, err := tx.Exec(t.Context(), `UPDATE runs SET lease_epoch = 2 WHERE id = ?`, r.run.ID)
		return err
	}))
	if got := results(r.mustSend(r.entry(v1.ReportStarted))); got != "rejected:CF_STALE_LEASE" {
		t.Errorf("outcomes = %s", got)
	}
}

func TestReport_AStartedOnARunThatExitedIsAnInvalidTransitionButAStopIsAccepted(t *testing.T) {
	e := newEnv(t)
	r := e.acceptedRun(e.joinWorker("alpha"), "x")
	r.mustSend(r.entry(v1.ReportStarted), r.entry(v1.ReportExited, exitCode(1)))
	if got := results(r.mustSend(r.entry(v1.ReportStarted))); got != "rejected:CF_INVALID_TRANSITION" {
		t.Errorf("started on a failed Run = %s", got)
	}
	if got := results(r.mustSend(r.entry(v1.ReportStopped))); got != "applied" {
		t.Errorf("stopped on a failed Run = %s", got)
	}
}

func TestReport_AnInternalFailureRollsTheWholeRequestBack(t *testing.T) {
	e := newEnv(t)
	r := e.acceptedRun(e.joinWorker("alpha"), "x")
	e.refuseEvents(domain.EventRunStopped)
	entries := []v1.RunReportEntry{r.entry(v1.ReportStarted), r.entry(v1.ReportExited, exitCode(0)), r.entry(v1.ReportStopped)}
	if _, err := r.send(entries...); err == nil {
		t.Fatal("the failure was not reported")
	}
	if got := e.storedRun(r.run.ID); got.State != domain.RunPreparing || got.Runtime != domain.RuntimeNone || got.StartedAt != nil {
		t.Errorf("Run = %+v: part of the request stayed", got)
	}
	if n := len(e.events(domain.EventRunStarted, domain.EventRunCompleted)); n != 0 {
		t.Errorf("%d events stayed", n)
	}
}

func TestReport_ChecksTheShapeOfTheRequest(t *testing.T) {
	e := newEnv(t)
	r := e.acceptedRun(e.joinWorker("alpha"), "x")
	ok := func() v1.RunReportEntry { r.seq = 0; return r.entry(v1.ReportStarted) }
	big := make([]v1.RunReportEntry, 65)
	for i := range big {
		big[i] = ok()
		big[i].WorkerSeq = uint64(i + 1)
	}
	for name, entries := range map[string][]v1.RunReportEntry{
		"empty":          nil,
		"too many":       big,
		"bad epoch":      {func() v1.RunReportEntry { en := ok(); en.WorkerEpoch = "nope"; return en }()},
		"zero seq":       {func() v1.RunReportEntry { en := ok(); en.WorkerSeq = 0; return en }()},
		"zero lease":     {func() v1.RunReportEntry { en := ok(); en.LeaseEpoch = 0; return en }()},
		"unknown kind":   {func() v1.RunReportEntry { en := ok(); en.Kind = "paused"; return en }()},
		"no time":        {func() v1.RunReportEntry { en := ok(); en.At = time.Time{}; return en }()},
		"long message":   {func() v1.RunReportEntry { en := ok(); en.Message = strings.Repeat("x", 513); return en }()},
		"no handle":      {func() v1.RunReportEntry { en := ok(); en.Handle = nil; return en }()},
		"handle spaces":  {func() v1.RunReportEntry { en := ok(); en.Handle.Ref = "a b"; return en }()},
		"handle too big": {func() v1.RunReportEntry { en := ok(); en.Handle.Ref = strings.Repeat("x", 300); return en }()},
		"exit on start":  {func() v1.RunReportEntry { en := ok(); n := 1; en.ExitCode = &n; return en }()},
		"seq not growing": {
			func() v1.RunReportEntry { en := ok(); en.WorkerSeq = 5; return en }(),
			func() v1.RunReportEntry { en := ok(); en.WorkerSeq = 5; return en }(),
		},
		"bad cause": {func() v1.RunReportEntry { en := r.entry(v1.ReportExited); en.Cause = "?"; return en }()},
		"exit range": {func() v1.RunReportEntry {
			en := r.entry(v1.ReportExited)
			n := 300
			en.ExitCode = &n
			return en
		}()},
	} {
		_, err := r.send(entries...)
		if err == nil {
			t.Errorf("%s: accepted", name)
			continue
		}
		wantCode(t, err, domain.CodeInvalidRequest)
	}
	if got := e.storedRun(r.run.ID); got.State != domain.RunPreparing {
		t.Errorf("a malformed request moved the Run: %+v", got)
	}
}

func TestHeartbeat_AnswersWhatTheWorkerMayTakeOn(t *testing.T) {
	e := newEnv(t)
	w := e.joinWorkerWith("alpha", v1.WorkerSettings{CPUMax: 4, MaxConcurrentRuns: 3})
	resp, err := e.app.Workers.Heartbeat(t.Context(), w, v1.HeartbeatRequest{
		Epoch: e.epochOf(w), Hostname: "box", OS: "linux", Arch: "amd64",
		Total: v1.Capacity{CPU: 16, MemoryMB: 65536, DiskMB: 1000}, Health: v1.Health{Status: v1.HealthOK},
	})
	must(t, err)
	if resp.MaxConcurrentRuns != 3 || resp.Allocatable.CPU != 4 || resp.Allocatable.MemoryMB != 65536 {
		t.Errorf("response = %+v", resp)
	}
}

func TestReport_AStopThatSaysTheHarnessRanNeedsARunThatIsOver(t *testing.T) {
	e := newEnv(t)
	r := e.acceptedRun(e.joinWorker("alpha"), "x")
	r.mustSend(r.entry(v1.ReportStarted))
	if got := results(r.mustSend(r.entry(v1.ReportStopped))); got != "rejected:CF_INVALID_TRANSITION" {
		t.Errorf("stopped on a running Run = %s", got)
	}
	if got := e.storedRun(r.run.ID); got.Runtime != domain.RuntimeActive {
		t.Errorf("runtime = %s", got.Runtime)
	}
}

func TestReport_AHarnessThatCouldNotSpawnFailsTheRunWithPublicationNone(t *testing.T) {
	e := newEnv(t)
	r := e.acceptedRun(e.joinWorker("alpha"), "x")
	// `started` is the session being created; the harness then fails to spawn.
	out := r.mustSend(r.entry(v1.ReportStarted), r.entry(v1.ReportPrepareFailed), r.entry(v1.ReportStopped))
	if got := results(out); got != "applied,applied,applied" {
		t.Fatalf("outcomes = %s", got)
	}
	got := e.storedRun(r.run.ID)
	if got.State != domain.RunFailed || got.EndReason != domain.EndEnvironment || got.Publication != domain.PublicationNone || got.Runtime != domain.RuntimeStopped {
		t.Errorf("Run = %+v", got)
	}
	if res := e.reservation(r.lease.ID); res.ReleasedAt == nil {
		t.Error("the reservation was kept")
	}
}

func TestReport_NumbersAndTimesOutOfRangeAreInvalidRequestsNot500s(t *testing.T) {
	e := newEnv(t)
	r := e.acceptedRun(e.joinWorker("alpha"), "x")
	for name, mutate := range map[string]func(*v1.RunReportEntry){
		"huge lease epoch": func(en *v1.RunReportEntry) { en.LeaseEpoch = 1<<63 + 5 },
		"huge seq":         func(en *v1.RunReportEntry) { en.WorkerSeq = 1<<63 + 5 },
		"year 1":           func(en *v1.RunReportEntry) { en.At = time.Date(1, 1, 1, 0, 0, 0, 0, time.UTC) },
		"far future":       func(en *v1.RunReportEntry) { en.At = e.clock.Now().Add(25 * time.Hour) },
	} {
		en := r.entry(v1.ReportStarted)
		mutate(&en)
		_, err := r.send(en)
		if err == nil {
			t.Errorf("%s: accepted", name)
			continue
		}
		wantCode(t, err, domain.CodeInvalidRequest)
	}
	// A day ahead is within the tolerance.
	en := r.entry(v1.ReportStarted, func(en *v1.RunReportEntry) { en.At = e.clock.Now().Add(23 * time.Hour) })
	if got := results(r.mustSend(en)); got != "applied" {
		t.Errorf("a time within a day = %s", got)
	}
}

func TestReport_ARunThatIsNoLongerTheWorkersTellsItLittle(t *testing.T) {
	e := newEnv(t)
	w := e.joinWorker("alpha")
	r := e.acceptedRun(w, "x")
	// The Run moves on to another lease epoch, as a lost Run would.
	must(t, e.db.WithTx(t.Context(), func(tx *store.Tx) error {
		_, err := tx.Exec(t.Context(), `UPDATE runs SET lease_epoch = 2 WHERE id = ?`, r.run.ID)
		return err
	}))
	out := r.mustSend(r.entry(v1.ReportStarted))
	if got := results(out); got != "rejected:CF_STALE_LEASE" {
		t.Fatalf("outcomes = %s", got)
	}
	if out.Run.WorkerID != "" || out.Run.Lease != nil || out.Run.TaskID != "" || out.Run.State == "" {
		t.Errorf("Run = %+v: only state, runtime and publication", out.Run)
	}
}
