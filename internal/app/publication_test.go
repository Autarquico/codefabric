package app_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	v1 "github.com/Autarquico/codefabric/internal/api/v1"
	"github.com/Autarquico/codefabric/internal/app/runs"
	"github.com/Autarquico/codefabric/internal/controller/store"
	"github.com/Autarquico/codefabric/internal/domain"
)

const (
	sha1 = "1111111111111111111111111111111111111111"
	sha2 = "2222222222222222222222222222222222222222"
)

// fakeRelay stands for the git relay: it says where the branches stand.
type fakeRelay struct {
	tips map[string]string // "<project>/<branch>" -> commit
	err  error
}

func (f *fakeRelay) BranchTip(_ context.Context, project domain.ID, branch string) (string, bool, error) {
	if f.err != nil {
		return "", false, f.err
	}
	oid, ok := f.tips[string(project)+"/"+branch]
	return oid, ok, nil
}

// pushes records that the Worker pushed commit to the branch of the Run.
func (e *env) pushes(r *reporter, commit string) {
	e.t.Helper()
	run := e.storedRun(r.run.ID)
	task, err := e.app.Tasks.Get(e.t.Context(), run.TaskID.String(), "")
	must(e.t, err)
	if e.relay.tips == nil {
		e.relay.tips = map[string]string{}
	}
	e.relay.tips[task.ProjectID+"/"+run.Branch] = commit
}

func sealedTo(commit string) func(*v1.RunReportEntry) {
	return func(en *v1.RunReportEntry) { en.ResultCommit = commit }
}

func publishedAs(commit string) func(*v1.RunReportEntry) {
	return func(en *v1.RunReportEntry) { en.ResultCommit = commit }
}

func phase(p string) func(*v1.RunReportEntry) {
	return func(en *v1.RunReportEntry) { en.Phase = p }
}

// exitedAndStopped takes the Run to where the Worker has to seal it.
func (r *reporter) exitedAndStopped(code int) {
	r.e.t.Helper()
	r.mustSend(r.entry(v1.ReportStarted), r.entry(v1.ReportExited, exitCode(code)), r.entry(v1.ReportStopped))
}

// completedAndNone counts the Runs the database must never hold.
func (e *env) completedAndNone() int {
	e.t.Helper()
	var n int
	must(e.t, e.db.Reader().QueryRowContext(e.t.Context(), `SELECT COUNT(*) FROM runs WHERE state = 'completed' AND publication = 'none'`).Scan(&n))
	return n
}

func TestScenario_GitRelayPublication_ExitAtomico(t *testing.T) {
	e := newEnv(t)
	r := e.acceptedRun(e.joinWorker("alpha"), "x")

	// Every prefix of the sequences a Worker can send leaves no completed Run with publication none.
	steps := []v1.RunReportEntry{r.entry(v1.ReportStarted), r.entry(v1.ReportExited, exitCode(0)), r.entry(v1.ReportStopped)}
	for i, step := range steps {
		r.mustSend(step)
		if n := e.completedAndNone(); n != 0 {
			t.Fatalf("after step %d: %d completed Runs have publication none", i, n)
		}
		if got := e.storedRun(r.run.ID); i >= 1 && (got.State != domain.RunCompleted || got.Publication != domain.PublicationPending) {
			t.Fatalf("after the exit the Run is %s/%s, in the same transaction as the exit", got.State, got.Publication)
		}
	}
	// The same, for the other ways a Run ends.
	for _, name := range []string{"beta", "gamma"} {
		o := e.acceptedRun(e.joinWorker(name), name)
		if name == "beta" {
			o.mustSend(o.entry(v1.ReportStarted), o.entry(v1.ReportExited, exitCode(1)), o.entry(v1.ReportStopped))
		} else {
			o.mustSend(o.entry(v1.ReportPrepareFailed), o.entry(v1.ReportStopped, neverStarted))
		}
		if n := e.completedAndNone(); n != 0 {
			t.Fatalf("after %s: %d completed Runs have publication none", name, n)
		}
	}
	if evs := e.events(domain.EventRunCompleted); len(evs) != 1 {
		t.Errorf("RunCompleted events = %d", len(evs))
	}
}

func TestPublication_SealedPublishedHappyPath(t *testing.T) {
	e := newEnv(t)
	r := e.acceptedRun(e.joinWorker("alpha"), "x")
	r.exitedAndStopped(0)
	if res := e.reservation(r.lease.ID); res.ReleasedAt != nil {
		t.Fatal("released before sealing")
	}

	out := r.mustSend(r.entry(v1.ReportSealed, sealedTo(sha1), func(en *v1.RunReportEntry) {
		en.Excluded, en.ExcludedTruncated = []string{".env", "key.pem"}, true
	}))
	got := e.storedRun(r.run.ID)
	if got.ResultCommit != sha1 || got.Publication != domain.PublicationPending || len(got.ResultExcluded) != 2 || !got.ResultExcludedTruncated {
		t.Fatalf("Run = %+v", got)
	}
	if out.Run.ResultCommit != sha1 || out.Run.Branch == "" || out.Run.Branch != got.Branch {
		t.Errorf("answer Run = %+v", out.Run)
	}
	if res := e.reservation(r.lease.ID); res.ReleasedAt == nil {
		t.Error("a sealed Run keeps its reservation")
	}
	if evs := e.events(domain.EventRunSealed); len(evs) != 1 || evs[0].Data["result_commit"] != sha1 {
		t.Errorf("RunSealed = %+v", evs)
	}

	e.pushes(r, sha1)
	out = r.mustSend(r.entry(v1.ReportPublished, publishedAs(sha1)))
	if got := e.storedRun(r.run.ID); got.Publication != domain.PublicationPublished || out.Run.Publication != domain.PublicationPublished {
		t.Errorf("Run = %+v, answer %s", got, out.Run.Publication)
	}
	if evs := e.events(domain.EventRunPublished); len(evs) != 1 {
		t.Errorf("RunPublished = %d", len(evs))
	}
	if task, err := e.app.Tasks.Get(t.Context(), r.run.TaskID, ""); err != nil || task.State != domain.TaskCompleted {
		t.Errorf("Task = %v (%v)", task.State, err)
	}
}

func TestPublication_SealedIsIdempotentAndNeverChanges(t *testing.T) {
	e := newEnv(t)
	r := e.acceptedRun(e.joinWorker("alpha"), "x")
	r.exitedAndStopped(0)
	r.mustSend(r.entry(v1.ReportSealed, sealedTo(sha1)))
	// The same commit under another seq: accepted, nothing to change.
	if got := results(r.mustSend(r.entry(v1.ReportSealed, sealedTo(sha1)))); got != "applied" {
		t.Errorf("sealed again = %s", got)
	}
	// Another commit: a result does not change.
	if got := results(r.mustSend(r.entry(v1.ReportSealed, sealedTo(sha2)))); got != "rejected:CF_INVALID_TRANSITION" {
		t.Errorf("sealed to another commit = %s", got)
	}
	if got := e.storedRun(r.run.ID).ResultCommit; got != sha1 {
		t.Errorf("result commit = %s", got)
	}
}

func TestPublication_EntriesThatDoNotFit(t *testing.T) {
	e := newEnv(t)
	// A Run that never started has nothing to seal or publish.
	n := e.acceptedRun(e.joinWorker("alpha"), "a")
	n.mustSend(n.entry(v1.ReportPrepareFailed), n.entry(v1.ReportStopped, neverStarted))
	for name, kind := range map[string]func(*v1.RunReportEntry){
		"sealed":    sealedTo(sha1),
		"published": publishedAs(sha1),
	} {
		k := v1.ReportSealed
		if name == "published" {
			k = v1.ReportPublished
		}
		if got := results(n.mustSend(n.entry(k, kind))); got != "rejected:CF_INVALID_TRANSITION" {
			t.Errorf("%s on a Run that never ran = %s", name, got)
		}
	}

	r := e.acceptedRun(e.joinWorker("beta"), "b")
	r.exitedAndStopped(0)
	// Published before sealed: the commit is not the sealed one.
	if got := results(r.mustSend(r.entry(v1.ReportPublished, publishedAs(sha1)))); got != "rejected:CF_INVALID_REQUEST" {
		t.Errorf("published before sealed = %s", got)
	}
	r.mustSend(r.entry(v1.ReportSealed, sealedTo(sha1)))
	if got := results(r.mustSend(r.entry(v1.ReportPublished, publishedAs(sha2)))); got != "rejected:CF_INVALID_REQUEST" {
		t.Errorf("published another commit = %s", got)
	}
	o := e.acceptedRun(e.joinWorker("gamma"), "c")
	o.exitedAndStopped(0)
	// Shapes.
	for name, mutate := range map[string]func(*v1.RunReportEntry){
		"bad commit":     func(en *v1.RunReportEntry) { en.ResultCommit = "abc" },
		"upper case":     func(en *v1.RunReportEntry) { en.ResultCommit = strings.Repeat("A", 40) },
		"unsorted":       func(en *v1.RunReportEntry) { en.ResultCommit, en.Excluded = sha1, []string{"b", "a"} },
		"hidden path":    func(en *v1.RunReportEntry) { en.ResultCommit, en.Excluded = sha1, []string{"a\nb"} },
		"too many paths": func(en *v1.RunReportEntry) { en.ResultCommit, en.Excluded = sha1, make([]string, 101) },
	} {
		_, err := o.send(o.entry(v1.ReportSealed, mutate))
		wantCode(t, err, domain.CodeInvalidRequest)
		_ = name
	}
	if _, err := o.send(o.entry(v1.ReportPublicationFailed, phase("verify"))); err == nil {
		t.Error("a Worker said verify")
	}
	if _, err := o.send(o.entry(v1.ReportStarted, sealedTo(sha1))); err == nil {
		t.Error("started with a result commit")
	}
}

func TestPublication_ARelayThatDoesNotHoldTheResultFailsItAndTheReportIsApplied(t *testing.T) {
	e := newEnv(t)
	r := e.acceptedRun(e.joinWorker("alpha"), "x")
	r.exitedAndStopped(0)
	r.mustSend(r.entry(v1.ReportSealed, sealedTo(sha1)))

	e.pushes(r, sha2) // the branch is somewhere else
	out := r.mustSend(r.entry(v1.ReportPublished, publishedAs(sha1)))
	if got := results(out); got != "applied" {
		t.Fatalf("a failed verification is applied, not rejected (a rejection fences the Run): %s", got)
	}
	if out.Run.Publication != domain.PublicationFailed {
		t.Errorf("publication = %s", out.Run.Publication)
	}
	evs := e.events(domain.EventRunPublicationFailed)
	if len(evs) != 1 || evs[0].Data["phase"] != v1.PhaseVerify {
		t.Errorf("RunPublicationFailed = %+v", evs)
	}
	if n := len(e.events(domain.EventRunPublished)); n != 0 {
		t.Errorf("%d RunPublished events", n)
	}
}

func TestPublication_AnErrorAskingTheRelayRollsTheBatchBackWithoutFencing(t *testing.T) {
	e := newEnv(t)
	r := e.acceptedRun(e.joinWorker("alpha"), "x")
	r.exitedAndStopped(0)
	r.mustSend(r.entry(v1.ReportSealed, sealedTo(sha1)))
	e.relay.err = errors.New("git is on fire")
	_, err := r.send(r.entry(v1.ReportPublished, publishedAs(sha1)))
	var coded *domain.Error
	if err == nil || errors.As(err, &coded) {
		t.Fatalf("an internal failure must roll the request back, not be answered as a rejection: %v", err)
	}
	if got := e.storedRun(r.run.ID).Publication; got != domain.PublicationPending {
		t.Errorf("publication = %s", got)
	}
	e.relay.err = nil
	e.pushes(r, sha1)
	if got := results(r.mustSend(r.entry(v1.ReportPublished, publishedAs(sha1)))); got != "applied" {
		t.Errorf("after the relay recovered = %s", got)
	}
}

func TestScenario_GitRelayPublication_SelladoFallido_Controller(t *testing.T) {
	e := newEnv(t)
	w := e.joinWorker("alpha")
	r := e.acceptedRun(w, "x")
	r.exitedAndStopped(0)

	out := r.mustSend(r.entry(v1.ReportPublicationFailed, phase(v1.PhaseSeal), func(en *v1.RunReportEntry) { en.Message = "no space left on device" }))
	if got := results(out); got != "applied" {
		t.Fatalf("outcomes = %s", got)
	}
	got := e.storedRun(r.run.ID)
	if got.Publication != domain.PublicationFailed || got.State != domain.RunCompleted {
		t.Fatalf("Run = %+v", got)
	}
	if evs := e.events(domain.EventRunPublicationFailed); len(evs) != 1 || evs[0].Data["phase"] != v1.PhaseSeal || evs[0].Data["message"] != "no space left on device" {
		t.Errorf("RunPublicationFailed = %+v", evs)
	}
	if res := e.reservation(r.lease.ID); res.ReleasedAt == nil {
		t.Error("a Run whose publication failed keeps its reservation")
	}
	if task, err := e.app.Tasks.Get(t.Context(), r.run.TaskID, ""); err != nil || task.State != domain.TaskWaiting || task.WaitingReason != domain.WaitingReview {
		t.Errorf("Task = %v/%v (%v)", task.State, task.WaitingReason, err)
	}

	// cf run publish: pending again, an order for the Worker, no new reservation.
	before := len(e.commands(w))
	run, err := e.app.Runs.Publish(t.Context(), e.owner, r.run.ID, v1.ActionRequest{RequestID: "again-1"})
	must(t, err)
	if run.Publication != domain.PublicationPending {
		t.Fatalf("publication = %s", run.Publication)
	}
	cmds := e.commands(w)
	if len(cmds) != before+1 || cmds[len(cmds)-1].Type != domain.CommandPublish || cmds[len(cmds)-1].Publish.RunID != r.run.ID ||
		cmds[len(cmds)-1].Publish.Epoch != r.lease.Epoch {
		t.Errorf("commands = %+v", cmds)
	}
	if evs := e.events(domain.EventRunPublicationRequested); len(evs) != 1 || evs[0].Actor.Kind != domain.ActorHuman {
		t.Errorf("RunPublicationRequested = %+v", evs)
	}
	if res := e.reservation(r.lease.ID); res.ReleasedAt == nil {
		t.Error("publishing again took the reservation back")
	}
	// The same request again answers the stored response; a new one is not valid on a pending publication.
	again, err := e.app.Runs.Publish(t.Context(), e.owner, r.run.ID, v1.ActionRequest{RequestID: "again-1"})
	must(t, err)
	if again.Publication != domain.PublicationPending || len(e.commands(w)) != len(cmds) {
		t.Errorf("the repeated request changed things: %+v", again)
	}
	_, err = e.app.Runs.Publish(t.Context(), e.owner, r.run.ID, v1.ActionRequest{RequestID: "again-2"})
	wantCode(t, err, domain.CodeInvalidTransition)

	// The Worker seals again (it had no result) and publishes.
	r.mustSend(r.entry(v1.ReportSealed, sealedTo(sha1)))
	e.pushes(r, sha1)
	r.mustSend(r.entry(v1.ReportPublished, publishedAs(sha1)))
	if got := e.storedRun(r.run.ID).Publication; got != domain.PublicationPublished {
		t.Errorf("publication = %s", got)
	}
	if task, err := e.app.Tasks.Get(t.Context(), r.run.TaskID, ""); err != nil || task.State != domain.TaskCompleted {
		t.Errorf("Task = %v (%v)", task.State, err)
	}
}

func TestPublish_OnlyAFinishedRunWhoseWorkerStillExists(t *testing.T) {
	e := newEnv(t)
	w := e.joinWorker("alpha")
	r := e.acceptedRun(w, "x")
	_, err := e.app.Runs.Publish(t.Context(), e.owner, r.run.ID, v1.ActionRequest{})
	wantCode(t, err, domain.CodeInvalidTransition)
	r.exitedAndStopped(0)
	_, err = e.app.Runs.Publish(t.Context(), e.owner, r.run.ID, v1.ActionRequest{})
	wantCode(t, err, domain.CodeInvalidTransition) // pending, not failed
	r.mustSend(r.entry(v1.ReportPublicationFailed, phase(v1.PhaseSeal)))

	must(t, e.db.WithTx(t.Context(), func(tx *store.Tx) error {
		_, err := tx.Exec(t.Context(), `UPDATE workers SET state = 'removed'`)
		return err
	}))
	_, err = e.app.Runs.Publish(t.Context(), e.owner, r.run.ID, v1.ActionRequest{})
	wantCode(t, err, domain.CodeInvalidTransition)
	if got := e.storedRun(r.run.ID).Publication; got != domain.PublicationFailed {
		t.Errorf("publication = %s", got)
	}
	_, err = e.app.Runs.Publish(t.Context(), w, r.run.ID, v1.ActionRequest{})
	if err == nil {
		t.Error("a Worker published a Run")
	}
}

func TestBranchOf_AFrozenBranchAndTheRuleForTheOnesThatHaveNone(t *testing.T) {
	e := newEnv(t)
	e.project("p")
	task := e.mustTask("p", "x", func(r *v1.CreateTaskRequest) { r.Name = "foo.lock" })
	run := e.storedRun(task.Run.ID)
	if run.Branch != "cf/task-1/run-1" {
		t.Errorf("branch of a Task named foo.lock = %q", run.Branch)
	}

	// A Run from before the branch was stored has none; reading it applies the same rule.
	must(t, e.db.WithTx(t.Context(), func(tx *store.Tx) error {
		_, err := tx.Exec(t.Context(), `UPDATE runs SET branch = NULL`)
		return err
	}))
	got, err := e.app.Runs.Get(t.Context(), task.Run.ID)
	must(t, err)
	if got.Branch != "cf/task-1/run-1" {
		t.Errorf("derived branch = %q", got.Branch)
	}
	list, err := e.app.Runs.List(t.Context(), runs.ListFilter{})
	must(t, err)
	if len(list) != 1 || list[0].Branch != "cf/task-1/run-1" {
		t.Errorf("listed = %+v", list)
	}
}

func TestAssignCarriesTheBranchAndTheRepoOfTheProject(t *testing.T) {
	e := newEnv(t)
	w := e.joinWorker("alpha")
	run := e.queuedRun("x")
	e.offer(run.ID, w)
	cmds := e.commands(w)
	if len(cmds) != 1 || cmds[0].Assign == nil {
		t.Fatalf("commands = %+v", cmds)
	}
	a := cmds[0].Assign
	if a.Branch != e.storedRun(run.ID).Branch || !strings.HasPrefix(a.Branch, "cf/") || a.Repo != "git@example.com:org/backend.git" {
		t.Errorf("assign = %+v", a)
	}
}

func TestPublish_ATaskThatIsArchivedIsNotPublishedAgain(t *testing.T) {
	e := newEnv(t)
	r := e.acceptedRun(e.joinWorker("alpha"), "x")
	r.exitedAndStopped(0)
	r.mustSend(r.entry(v1.ReportPublicationFailed, phase(v1.PhaseSeal)))
	_, err := e.app.Tasks.Archive(t.Context(), e.owner, r.run.TaskID, "", v1.ActionRequest{})
	must(t, err)
	_, err = e.app.Runs.Publish(t.Context(), e.owner, r.run.ID, v1.ActionRequest{})
	wantCode(t, err, domain.CodeInvalidTransition)
	if got := e.storedRun(r.run.ID).Publication; got != domain.PublicationFailed {
		t.Errorf("publication = %s", got)
	}
}

func TestReleasable_ACancelledRunThatStartedKeepsItsReservationUntilSomeoneSealsIt(t *testing.T) {
	e := newEnv(t)
	r := e.acceptedRun(e.joinWorker("alpha"), "x")
	r.mustSend(r.entry(v1.ReportStarted))
	_, err := e.app.Tasks.Cancel(t.Context(), e.owner, r.run.TaskID, "", v1.ActionRequest{})
	must(t, err)
	// started, cancelled, stopped: the worktree is not sealed, so the reservation stays (#13 closes this).
	if got := results(r.mustSend(r.entry(v1.ReportStopped))); got != "applied" {
		t.Fatalf("stopped = %s", got)
	}
	if res := e.reservation(r.lease.ID); res.ReleasedAt != nil {
		t.Errorf("the reservation of a cancelled Run that started was released: %+v", res)
	}
}
