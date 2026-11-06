package app_test

import (
	"fmt"
	"strings"
	"testing"
	"time"

	v1 "github.com/Autarquico/codefabric/internal/api/v1"
	"github.com/Autarquico/codefabric/internal/controller/pki"
	"github.com/Autarquico/codefabric/internal/domain"
)

const inputHarness = "command"

func onHarness(r *v1.CreateTaskRequest) { r.Requirements.Harness = inputHarness }

// structuredWorker is a Worker that can be answered while it runs the harness.
func (e *env) structuredWorker(name string) pki.Session {
	return e.joinWorker(name, withCapabilities(domain.HarnessStructuredInputCapability(inputHarness)))
}

// harnessRun gives a Run of the harness to the Worker and starts it.
func (e *env) harnessRun(w pki.Session) *reporter {
	e.t.Helper()
	run := e.queuedRun("x", onHarness)
	l := e.offer(run.ID, w)
	_, err := e.accept(w, l)
	must(e.t, err)
	r := &reporter{e: e, w: w, run: run, lease: l, epoch: reportEpoch}
	r.mustSend(r.entry(v1.ReportStarted))
	return r
}

func asks(id string, kind domain.PromptKind) func(*v1.RunReportEntry) {
	return func(en *v1.RunReportEntry) {
		en.PromptID, en.PromptKind, en.Summary = id, kind, "run `make test`?"
	}
}

func delivered(id string) func(*v1.RunReportEntry) {
	return func(en *v1.RunReportEntry) { en.PromptID = id }
}

func (e *env) respond(run v1.Run, req v1.RespondRequest) (v1.RespondResponse, error) {
	e.t.Helper()
	return e.app.Runs.Respond(e.t.Context(), e.owner, run.TaskID, "", req)
}

func allow() v1.InputAnswer { return v1.InputAnswer{Decision: domain.AnswerAllow} }

func inputOrders(cmds []v1.WorkerCommand) []v1.WorkerCommand {
	var out []v1.WorkerCommand
	for _, c := range cmds {
		if c.Type == domain.CommandInput {
			out = append(out, c)
		}
	}
	return out
}

func TestScenario_NeedsInputInbox_Permiso(t *testing.T) {
	e := newEnv(t)
	r := e.harnessRun(e.structuredWorker("alpha"))

	out := r.mustSend(r.entry(v1.ReportInputRequested, asks("p1", domain.PromptPermission)))
	if got := results(out); got != "applied" {
		t.Fatalf("outcomes = %s", got)
	}
	if got := e.storedRun(r.run.ID).State; got != domain.RunWaitingInput {
		t.Errorf("Run = %s", got)
	}
	if len(e.events(domain.EventRunWaitingInput)) != 1 || len(e.events(domain.EventTaskNeedsInput)) != 1 {
		t.Errorf("events: waiting %d, needs input %d", len(e.events(domain.EventRunWaitingInput)), len(e.events(domain.EventTaskNeedsInput)))
	}
	task, err := e.app.Tasks.Get(t.Context(), r.run.TaskID, "")
	must(t, err)
	if task.State != domain.TaskWaiting || task.WaitingReason != domain.WaitingInput {
		t.Errorf("Task = %s(%s)", task.State, task.WaitingReason)
	}
	inbox, err := e.app.Inbox.List(t.Context())
	must(t, err)
	if len(inbox.Items) != 1 {
		t.Fatalf("inbox = %+v", inbox.Items)
	}
	it := inbox.Items[0]
	if it.Kind != v1.InboxNeedsInput || it.Task.ID != r.run.TaskID || it.Prompt == nil || it.Prompt.ID != "p1" ||
		it.Prompt.Kind != domain.PromptPermission || it.Prompt.Summary == "" || !it.Run.Respondable || it.Run.Harness != inputHarness {
		t.Errorf("item = %+v", it)
	}

	// The same prompt told again, by a Worker that restarted, changes nothing.
	again := r.mustSend(r.entry(v1.ReportInputRequested, asks("p1", domain.PromptPermission)))
	if got := results(again); got != "applied" || len(e.events(domain.EventTaskNeedsInput)) != 1 {
		t.Errorf("replay: %s, needs input events %d", got, len(e.events(domain.EventTaskNeedsInput)))
	}
}

func TestScenario_NeedsInputInbox_Respuesta(t *testing.T) {
	e := newEnv(t)
	w := e.structuredWorker("alpha")
	r := e.harnessRun(w)
	r.mustSend(r.entry(v1.ReportInputRequested, asks("p1", domain.PromptPermission)))

	out, err := e.respond(r.run, v1.RespondRequest{RequestID: "a1", Answer: allow()})
	must(t, err)
	if out.PromptID != "p1" || out.State != domain.PromptQueued || out.RunShort != r.run.Short {
		t.Errorf("response = %+v", out)
	}
	orders := inputOrders(e.commands(w))
	if len(orders) != 1 || orders[0].Input == nil {
		t.Fatalf("orders = %+v", orders)
	}
	in := orders[0].Input
	if in.RunID != r.run.ID || in.Epoch != r.lease.Epoch || in.PromptID != "p1" || in.Answer.Decision != domain.AnswerAllow ||
		in.Kind != domain.PromptPermission || in.Actor.Kind != domain.ActorHuman {
		t.Errorf("input = %+v", in)
	}
	provided := e.events(domain.EventInputProvided)
	if len(provided) != 1 || provided[0].Actor.ID != string(e.owner.Principal.ID) || provided[0].Actor.Kind != domain.ActorHuman ||
		string(provided[0].RunID) != r.run.ID {
		t.Errorf("InputProvided = %+v", provided)
	}
	if got := e.storedRun(r.run.ID).State; got != domain.RunWaitingInput {
		t.Errorf("the Run left waiting_input before the Worker delivered: %s", got)
	}
	if inbox, _ := e.app.Inbox.List(t.Context()); len(inbox.Items) != 0 {
		t.Errorf("an answered prompt is still in the inbox: %+v", inbox.Items)
	}

	r.mustSend(r.entry(v1.ReportInputDelivered, delivered("p1")))
	if got := e.storedRun(r.run.ID).State; got != domain.RunRunning {
		t.Errorf("Run = %s", got)
	}
	if task, err := e.app.Tasks.Get(t.Context(), r.run.TaskID, ""); err != nil || task.State != domain.TaskRunning {
		t.Errorf("Task = %v (%v)", task.State, err)
	}
	// A delivery told again is a duplicate of nothing.
	if got := results(r.mustSend(r.entry(v1.ReportInputDelivered, delivered("p1")))); got != "applied" {
		t.Errorf("replayed delivery = %s", got)
	}
}

func TestScenario_NeedsInputInbox_NoRespondable(t *testing.T) {
	e := newEnv(t)
	w := e.joinWorker("alpha") // no structured input
	r := e.harnessRun(w)

	_, err := e.respond(r.run, v1.RespondRequest{RequestID: "a1", Answer: allow()})
	assertNotRespondable(t, err, r.run.Short)

	// A Worker that stops announcing structured input after it asked is not answered either.
	s := e.structuredWorker("beta")
	r2 := e.harnessRun(s)
	r2.mustSend(r2.entry(v1.ReportInputRequested, asks("p1", domain.PromptPermission)))
	e.beat(s)
	_, err = e.respond(r2.run, v1.RespondRequest{RequestID: "a2", Answer: allow()})
	assertNotRespondable(t, err, r2.run.Short)
	if n := len(inputOrders(e.commands(s))); n != 0 {
		t.Errorf("%d input orders were queued for a Worker without the capability", n)
	}
	if len(e.events(domain.EventInputProvided)) != 0 {
		t.Error("InputProvided was written for an answer that was not delivered")
	}
}

func assertNotRespondable(t *testing.T, err error, runShort string) {
	t.Helper()
	wantCode(t, err, domain.CodeRunNotRespondable)
	var de domain.Error
	if !asDomain(err, &de) {
		t.Fatalf("err = %v", err)
	}
	if domain.ExitCode(de.Code) != 8 {
		t.Errorf("exit code = %d", domain.ExitCode(de.Code))
	}
	if de.Details["attach"] != "cf attach "+runShort {
		t.Errorf("details = %v", de.Details)
	}
}

func TestScenario_NeedsInputInbox_Idempotencia(t *testing.T) {
	e := newEnv(t)
	w := e.structuredWorker("alpha")
	r := e.harnessRun(w)
	r.mustSend(r.entry(v1.ReportInputRequested, asks("p1", domain.PromptPermission)))

	req := v1.RespondRequest{RequestID: "same", Answer: allow()}
	first, err := e.respond(r.run, req)
	must(t, err)
	second, err := e.respond(r.run, req)
	must(t, err)
	if first != second {
		t.Errorf("answers differ: %+v / %+v", first, second)
	}
	if n := len(inputOrders(e.commands(w))); n != 1 {
		t.Errorf("%d input orders", n)
	}
	if n := len(e.events(domain.EventInputProvided)); n != 1 {
		t.Errorf("%d InputProvided events", n)
	}
}

func TestRespond_OfTwoCompetingAnswersOnlyTheFirstIsClaimed(t *testing.T) {
	e := newEnv(t)
	w := e.structuredWorker("alpha")
	r := e.harnessRun(w)
	r.mustSend(r.entry(v1.ReportInputRequested, asks("p1", domain.PromptPermission)))

	_, err := e.respond(r.run, v1.RespondRequest{RequestID: "a", PromptID: "p1", Answer: allow()})
	must(t, err)
	_, err = e.respond(r.run, v1.RespondRequest{RequestID: "b", PromptID: "p1", Answer: v1.InputAnswer{Decision: domain.AnswerDeny}})
	wantCode(t, err, domain.CodeInvalidTransition)
	_, err = e.respond(r.run, v1.RespondRequest{RequestID: "c", Answer: allow()})
	wantCode(t, err, domain.CodeInvalidTransition)

	orders := inputOrders(e.commands(w))
	if len(orders) != 1 || orders[0].Input.Answer.Decision != domain.AnswerAllow {
		t.Errorf("orders = %+v", orders)
	}
}

func TestRespond_ChecksTheAnswerAgainstThePrompt(t *testing.T) {
	e := newEnv(t)
	w := e.structuredWorker("alpha")
	r := e.harnessRun(w)
	r.mustSend(r.entry(v1.ReportInputRequested, asks("p1", domain.PromptQuestion)))

	_, err := e.respond(r.run, v1.RespondRequest{Answer: allow()})
	wantCode(t, err, domain.CodeInvalidRequest)
	_, err = e.respond(r.run, v1.RespondRequest{PromptID: "nope", Answer: v1.InputAnswer{Text: "yes"}})
	wantCode(t, err, domain.CodeNotFound)
	_, err = e.respond(r.run, v1.RespondRequest{Answer: v1.InputAnswer{Text: "yes"}})
	must(t, err)
	if orders := inputOrders(e.commands(w)); len(orders) != 1 || orders[0].Input.Answer.Text != "yes" {
		t.Errorf("orders = %+v", orders)
	}
}

func TestRespond_ALateAnswerAfterCancelIsRejected(t *testing.T) {
	e := newEnv(t)
	w := e.structuredWorker("alpha")
	r := e.harnessRun(w)
	r.mustSend(r.entry(v1.ReportInputRequested, asks("p1", domain.PromptPermission)))

	_, err := e.app.Tasks.Cancel(t.Context(), e.owner, r.run.TaskID, "", v1.ActionRequest{})
	must(t, err)
	_, err = e.respond(r.run, v1.RespondRequest{Answer: allow()})
	wantCode(t, err, domain.CodeRunNotRespondable)
	if n := len(inputOrders(e.commands(w))); n != 0 {
		t.Errorf("%d input orders after the cancel", n)
	}
	if inbox, _ := e.app.Inbox.List(t.Context()); len(inbox.Items) != 0 {
		t.Errorf("a cancelled Task is in the inbox: %+v", inbox.Items)
	}
}

func TestReport_InputKindsNeedTheCapabilityOfTheWorker(t *testing.T) {
	e := newEnv(t)
	r := e.harnessRun(e.joinWorker("alpha"))

	out := r.mustSend(r.entry(v1.ReportInputRequested, asks("p1", domain.PromptPermission)))
	if got := results(out); got != "rejected:CF_INVALID_REQUEST" {
		t.Errorf("outcomes = %s", got)
	}
	if got := e.storedRun(r.run.ID).State; got != domain.RunRunning {
		t.Errorf("Run = %s", got)
	}

	s := e.structuredWorker("beta")
	r2 := e.harnessRun(s)
	bad := r2.entry(v1.ReportInputRequested, func(en *v1.RunReportEntry) { en.PromptID, en.PromptKind = "p1", "bogus" })
	if _, err := r2.send(bad); err == nil {
		t.Error("an invalid prompt kind was taken")
	}
	noID := r2.entry(v1.ReportInputDelivered)
	if _, err := r2.send(noID); err == nil {
		t.Error("a delivery without a prompt_id was taken")
	}
}

func TestReport_ADeliveryNeedsAnAnswerAndTheRunStaysWaitingWhileAnotherPromptIsOpen(t *testing.T) {
	e := newEnv(t)
	w := e.structuredWorker("alpha")
	r := e.harnessRun(w)
	r.mustSend(r.entry(v1.ReportInputRequested, asks("p1", domain.PromptPermission)))
	r.mustSend(r.entry(v1.ReportInputRequested, asks("p2", domain.PromptPermission)))

	if got := results(r.mustSend(r.entry(v1.ReportInputDelivered, delivered("p1")))); got != "rejected:CF_INVALID_TRANSITION" {
		t.Errorf("a delivery of an unanswered prompt = %s", got)
	}
	if _, err := e.respond(r.run, v1.RespondRequest{Answer: allow()}); err == nil {
		t.Error("two pending prompts were answered without naming one")
	}
	_, err := e.respond(r.run, v1.RespondRequest{PromptID: "p1", Answer: allow()})
	must(t, err)
	r.mustSend(r.entry(v1.ReportInputDelivered, delivered("p1")))
	if got := e.storedRun(r.run.ID).State; got != domain.RunWaitingInput {
		t.Errorf("Run = %s with p2 still open", got)
	}
	_, err = e.respond(r.run, v1.RespondRequest{Answer: allow()}) // p2 is now the only pending
	must(t, err)
	r.mustSend(r.entry(v1.ReportInputDelivered, delivered("p2")))
	if got := e.storedRun(r.run.ID).State; got != domain.RunRunning {
		t.Errorf("Run = %s", got)
	}
}

func TestInbox_ListsTheTasksThatFailedAndWaitForADecision(t *testing.T) {
	e := newEnv(t)
	r := e.harnessRun(e.joinWorker("alpha"))
	r.mustSend(r.entry(v1.ReportExited, exitCode(1)))

	inbox, err := e.app.Inbox.List(t.Context())
	must(t, err)
	if len(inbox.Items) != 1 || inbox.Items[0].Kind != v1.InboxFailed || inbox.Items[0].Task.ID != r.run.TaskID ||
		inbox.Items[0].EndReason != domain.EndExitCode {
		t.Errorf("inbox = %+v", inbox.Items)
	}
}

func (e *env) inboxFailed() []v1.InboxItem {
	e.t.Helper()
	inbox, err := e.app.Inbox.List(e.t.Context())
	must(e.t, err)
	return inbox.Items
}

func TestInbox_AFailedRunWhoseRetryPolicyIsToAskWaitsForADecision(t *testing.T) {
	e := newEnv(t)
	r := e.failedRun("x", withRetry(domain.RetryAsk, domain.RetryAsk, 2))
	task, err := e.app.Tasks.Get(t.Context(), r.run.TaskID, "")
	must(t, err)
	if task.State != domain.TaskWaiting || task.WaitingReason != domain.WaitingDecision {
		t.Fatalf("Task = %s(%s)", task.State, task.WaitingReason)
	}
	items := e.inboxFailed()
	if len(items) != 1 || items[0].Kind != v1.InboxFailed || items[0].Run.State != domain.RunFailed || items[0].EndReason != domain.EndExitCode {
		t.Errorf("inbox = %+v", items)
	}
}

func TestInbox_ALostRunIsListed(t *testing.T) {
	e := newEnvWith(t, startedNow)
	r := e.runningRun(e.joinWorker("alpha"), "x")
	e.clock.Advance(lostTimeout + time.Second)
	e.sweepLost()
	items := e.inboxFailed()
	if len(items) != 1 || items[0].Kind != v1.InboxFailed || items[0].Run.State != domain.RunLost ||
		items[0].Task.ID != r.run.TaskID {
		t.Errorf("inbox = %+v", items)
	}
}

func TestInbox_ACancelledTaskAndAnArchivedFailureAreNotListed(t *testing.T) {
	e := newEnv(t)
	cancelled := e.runningRun(e.joinWorker("alpha"), "a")
	_, err := e.app.Tasks.Cancel(t.Context(), e.owner, cancelled.run.TaskID, "", v1.ActionRequest{})
	must(t, err)
	if items := e.inboxFailed(); len(items) != 0 {
		t.Errorf("a cancelled Task is listed: %+v", items)
	}

	failed := e.failedRun("b", withRetry(domain.RetryAsk, domain.RetryAsk, 2))
	if items := e.inboxFailed(); len(items) != 1 {
		t.Fatalf("inbox = %+v", items)
	}
	_, err = e.app.Tasks.Archive(t.Context(), e.owner, failed.run.TaskID, "", v1.ActionRequest{})
	must(t, err)
	if items := e.inboxFailed(); len(items) != 0 {
		t.Errorf("an archived Task is listed: %+v", items)
	}
}

func TestRespond_AnAnswerQueuedBeforeACancelIsWithdrawn(t *testing.T) {
	e := newEnv(t)
	w := e.structuredWorker("alpha")
	r := e.harnessRun(w)
	r.mustSend(r.entry(v1.ReportInputRequested, asks("p1", domain.PromptPermission)))
	_, err := e.respond(r.run, v1.RespondRequest{Answer: allow()})
	must(t, err)
	if n := len(inputOrders(e.commands(w))); n != 1 {
		t.Fatalf("%d input orders before the cancel", n)
	}

	_, err = e.app.Tasks.Cancel(t.Context(), e.owner, r.run.TaskID, "", v1.ActionRequest{})
	must(t, err)
	if n := len(inputOrders(e.commands(w))); n != 0 {
		t.Errorf("%d input orders still reach the Worker after the cancel", n)
	}
	if len(e.stops(w)) != 1 {
		t.Errorf("stops = %+v", e.stops(w))
	}
}

func TestReport_AtMostMaxOpenPromptsAreOpenAtOnce(t *testing.T) {
	e := newEnv(t)
	r := e.harnessRun(e.structuredWorker("alpha"))
	for i := range v1.MaxOpenPrompts {
		r.mustSend(r.entry(v1.ReportInputRequested, asks(fmt.Sprintf("p%d", i), domain.PromptQuestion)))
	}
	over := r.mustSend(r.entry(v1.ReportInputRequested, asks("over", domain.PromptQuestion)))
	if got := results(over); got != "rejected:CF_INVALID_REQUEST" {
		t.Errorf("one prompt over the bound = %s", got)
	}
}

func TestReport_APromptThatIsNotBoundedPrintableTextIsRefused(t *testing.T) {
	e := newEnv(t)
	r := e.harnessRun(e.structuredWorker("alpha"))
	cases := map[string]func(*v1.RunReportEntry){
		"control character in summary": func(en *v1.RunReportEntry) { en.Summary = "ok\x1b[31m" },
		"summary too long":             func(en *v1.RunReportEntry) { en.Summary = strings.Repeat("a", v1.MaxReportMessageLen+1) },
		"empty summary":                func(en *v1.RunReportEntry) { en.Summary = "" },
		"too many options":             func(en *v1.RunReportEntry) { en.Options = make([]string, v1.MaxPromptOption+1) },
		"control character in option":  func(en *v1.RunReportEntry) { en.Options = []string{"a\nb"} },
		"option too long":              func(en *v1.RunReportEntry) { en.Options = []string{strings.Repeat("a", v1.MaxOptionLen+1)} },
	}
	for name, mutate := range cases {
		en := r.entry(v1.ReportInputRequested, asks("pb", domain.PromptQuestion), mutate)
		if _, err := r.send(en); err == nil {
			if got := e.storedRun(r.run.ID).State; got == domain.RunWaitingInput {
				t.Errorf("%s: the prompt was taken", name)
			}
		}
	}
	if got := e.storedRun(r.run.ID).State; got != domain.RunRunning {
		t.Errorf("Run = %s", got)
	}
}

func TestRespond_APromptIdOpenInTwoRunsNeedsTheRunToBeNamed(t *testing.T) {
	e := newEnv(t)
	w1, w2 := e.structuredWorker("alpha"), e.structuredWorker("beta")
	first := e.harnessRun(w1)
	first.mustSend(first.entry(v1.ReportInputRequested, asks("p1", domain.PromptPermission)))
	second := e.secondRunOf(first, w2)
	second.mustSend(second.entry(v1.ReportInputRequested, asks("p1", domain.PromptPermission)))

	_, err := e.respond(first.run, v1.RespondRequest{PromptID: "p1", Answer: allow()})
	wantCode(t, err, domain.CodeInvalidRequest)
	if !strings.Contains(err.Error(), first.run.Short) || !strings.Contains(err.Error(), second.run.Short) {
		t.Errorf("the error does not list both Runs: %v", err)
	}
	if n := len(inputOrders(e.commands(w1))) + len(inputOrders(e.commands(w2))); n != 0 {
		t.Fatalf("a rejected answer queued %d orders", n)
	}

	out, err := e.respond(first.run, v1.RespondRequest{PromptID: "p1", Run: second.run.Short, Answer: allow()})
	must(t, err)
	if out.RunID != second.run.ID {
		t.Errorf("answered run %s, want %s", out.RunID, second.run.ID)
	}
	if len(inputOrders(e.commands(w1))) != 0 || len(inputOrders(e.commands(w2))) != 1 {
		t.Errorf("the order did not go to the named Run's Worker")
	}
}

func TestRespond_APermissionIsNotAnsweredWithText(t *testing.T) {
	e := newEnv(t)
	r := e.harnessRun(e.structuredWorker("alpha"))
	r.mustSend(r.entry(v1.ReportInputRequested, asks("p1", domain.PromptPermission)))

	_, err := e.respond(r.run, v1.RespondRequest{Answer: v1.InputAnswer{Decision: domain.AnswerAllow, Text: "do not run it"}})
	wantCode(t, err, domain.CodeInvalidRequest)
	if n := len(inputOrders(e.commands(r.w))); n != 0 {
		t.Errorf("a rejected answer queued %d orders", n)
	}
}

// secondRunOf starts another Run of the Task of first on w and reports it started.
func (e *env) secondRunOf(first *reporter, w pki.Session) *reporter {
	e.t.Helper()
	run, err := e.startRun(first.run.TaskID, strings.Repeat("a", 40))
	must(e.t, err)
	l := e.offer(run.ID, w)
	_, err = e.accept(w, l)
	must(e.t, err)
	r := &reporter{e: e, w: w, run: run, lease: l, epoch: reportEpoch}
	r.mustSend(r.entry(v1.ReportStarted))
	return r
}
