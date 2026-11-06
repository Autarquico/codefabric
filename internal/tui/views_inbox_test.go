package tui

import (
	"strings"
	"testing"

	v1 "github.com/Autarquico/codefabric/internal/api/v1"
	"github.com/Autarquico/codefabric/internal/client"
	"github.com/Autarquico/codefabric/internal/domain"
)

func inboxFixture() []v1.InboxItem {
	return []v1.InboxItem{
		{
			Kind: v1.InboxNeedsInput, Since: at("12:40"),
			Task:   v1.InboxTask{ID: "tsk_1", Short: "t-4k2m", Name: "auth-refactor"},
			Run:    v1.InboxRun{ID: "run_1", Short: "r-8a1b", State: domain.RunWaitingInput, Harness: "claude", Respondable: true},
			Prompt: &v1.InboxPrompt{ID: "p1", Kind: domain.PromptPermission, Summary: "Bash: echo ok\nrm -rf build/\x1b[2J"},
		},
		{
			Kind: v1.InboxNeedsInput, Since: at("12:10"),
			Task:   v1.InboxTask{ID: "tsk_2", Short: "t-9p1x", Name: "infra"},
			Run:    v1.InboxRun{ID: "run_2", Short: "r-3c9d", State: domain.RunWaitingInput, Harness: "claude", Respondable: true},
			Prompt: &v1.InboxPrompt{ID: "p2", Kind: domain.PromptQuestion, Summary: "Which region?", Options: []string{"eu-west-1", "us-east-1"}},
		},
		{
			Kind: v1.InboxNeedsInput, Since: at("11:30"),
			Task:   v1.InboxTask{ID: "tsk_3", Short: "t-2c7q", Name: "payments"},
			Run:    v1.InboxRun{ID: "run_3", Short: "r-7e2f", State: domain.RunWaitingInput, Harness: "codex"},
			Prompt: &v1.InboxPrompt{ID: "p3", Kind: domain.PromptPermission, Summary: "Edit: billing.go"},
		},
		{
			Kind: v1.InboxFailed, Since: at("10:15"),
			Task:        v1.InboxTask{ID: "tsk_4", Short: "t-5d6e", Name: "migrations"},
			Run:         v1.InboxRun{ID: "run_4", Short: "r-1f0a", State: domain.RunFailed, Harness: "claude"},
			EndReason:   domain.EndExitCode,
			FailureCode: domain.CodeInternal,
		},
	}
}

func openInbox(t *testing.T, be Backend, items []v1.InboxItem) *Model {
	t.Helper()
	m := viewShellWith(t, be, "i")
	feed(t, m, feedMsg{RouteInbox, client.Update[[]v1.InboxItem]{Kind: client.UpdateSnapshot, State: items}})
	feed(t, m, feedMsg{RouteInbox, client.Update[[]v1.InboxItem]{Kind: client.UpdateConnected}})
	frame(m) // the person sees the list before pressing a key
	return m
}

func TestScenario_NeedsInputInbox_View(t *testing.T) {
	got := frame(openInbox(t, newBackend(), inboxFixture()))
	compareGolden(t, "inbox-80x24.golden", got)
	if strings.Contains(got, "\x1b") {
		t.Errorf("the prompt text acted on the terminal:\n%q", got)
	}
}

func TestScenario_NeedsInputInbox_Empty(t *testing.T) {
	compareGolden(t, "inbox-empty-80x24.golden", frame(openInbox(t, newBackend(), nil)))
}

func TestScenario_NeedsInputInbox_RespondAllowRunsTheCommandsAction(t *testing.T) {
	be := newBackend()
	m := openInbox(t, be, inboxFixture())
	press(t, m, "y")
	if len(be.ran) != 1 || be.ran[0] != "task.respond tsk_1" {
		t.Fatalf("ran %v", be.ran)
	}
	got := be.inputs[0].Values
	if len(got) != 3 || got["run"] != "run_1" || got["prompt"] != "p1" || got["allow"] != "true" {
		t.Errorf("values %v", got)
	}
	if !strings.Contains(frame(m), "✓ Answer task · auth-refactor") {
		t.Errorf("no confirmation:\n%s", frame(m))
	}
}

func TestScenario_NeedsInputInbox_RespondDeny(t *testing.T) {
	be := newBackend()
	m := openInbox(t, be, inboxFixture())
	press(t, m, "n")
	if len(be.inputs) != 1 || be.inputs[0].Values["deny"] != "true" || be.inputs[0].Values["allow"] != "" {
		t.Errorf("inputs %v", be.inputs)
	}
}

func TestScenario_NeedsInputInbox_RespondText(t *testing.T) {
	be := newBackend()
	m := openInbox(t, be, inboxFixture())
	press(t, m, "down")
	press(t, m, "enter")
	if got := frame(m); !strings.Contains(got, "eu-west-1, us-east-1") {
		t.Errorf("the options are not offered:\n%s", got)
	}
	typeText(t, m, "eu-west-1")
	press(t, m, "enter")
	if len(be.ran) != 1 || be.ran[0] != "task.respond tsk_2" {
		t.Fatalf("ran %v", be.ran)
	}
	v := be.inputs[0].Values
	if v["text"] != "eu-west-1" || v["run"] != "run_2" || v["prompt"] != "p2" {
		t.Errorf("values %v", v)
	}
}

func TestScenario_NeedsInputInbox_KeysThatDoNotFitTheItemSayWhy(t *testing.T) {
	be := newBackend()
	m := openInbox(t, be, inboxFixture())
	press(t, m, "enter") // a permission
	if !strings.Contains(frame(m), "Press y to allow or n to deny") {
		t.Errorf("no hint:\n%s", frame(m))
	}
	press(t, m, "down")
	press(t, m, "y") // a question
	if !strings.Contains(frame(m), "This asks for text") {
		t.Errorf("no hint:\n%s", frame(m))
	}
	if len(be.ran) != 0 {
		t.Errorf("ran %v", be.ran)
	}
}

func TestScenario_NeedsInputInbox_NotRespondableShowsTheAttachHint(t *testing.T) {
	be := newBackend()
	m := openInbox(t, be, inboxFixture())
	press(t, m, "down")
	press(t, m, "down")
	if got := frame(m); !strings.Contains(got, "cf attach r-7e2f") {
		t.Errorf("the detail has no attach hint:\n%s", got)
	}
	press(t, m, "y")
	if got := frame(m); !strings.Contains(got, "cannot be answered from here: cf attach r-7e2f") || len(be.ran) != 0 {
		t.Errorf("answered a Run that cannot be, or said nothing (ran %v):\n%s", be.ran, got)
	}
}

func TestScenario_NeedsInputInbox_FailureShowsItsEndReason(t *testing.T) {
	be := newBackend()
	m := openInbox(t, be, inboxFixture())
	for range 3 {
		press(t, m, "down")
	}
	got := frame(m)
	if !strings.Contains(got, "exit_code") || !strings.Contains(got, "migrations") {
		t.Errorf("the failure does not say why it ended:\n%s", got)
	}
	press(t, m, "y")
	if len(be.ran) != 0 {
		t.Errorf("a failure was answered: %v", be.ran)
	}
}

func TestScenario_NeedsInputInbox_FollowsTheFeedLive(t *testing.T) {
	m := openInbox(t, newBackend(), nil)
	feed(t, m, feedMsg{RouteInbox, client.Update[[]v1.InboxItem]{Kind: client.UpdateSnapshot, State: inboxFixture()[:1]}})
	if got := frame(m); !strings.Contains(got, "auth-refactor") || strings.Contains(got, "Nothing needs you") {
		t.Errorf("the new item did not appear:\n%s", got)
	}
}

func TestScenario_NeedsInputInbox_MultiLinePromptKeepsItsLines(t *testing.T) {
	m := openInbox(t, newBackend(), inboxFixture())
	got := frame(m)
	if !strings.Contains(got, "echo ok …") || !strings.Contains(got, "echo ok ⏎ rm -rf build/") {
		t.Errorf("the lines of the prompt run together:\n%s", got)
	}
}

func TestScenario_NeedsInputInbox_DoesNotAnswerWhatSlidUnderTheCursor(t *testing.T) {
	be := newBackend()
	items := inboxFixture()
	m := openInbox(t, be, items)
	// the person looks at the first permission; it is answered elsewhere and
	// the next permission-like item takes its place before the key arrives
	rest := append([]v1.InboxItem{}, items[2:]...)
	rest[0].Run.Respondable = true
	feed(t, m, feedMsg{RouteInbox, client.Update[[]v1.InboxItem]{Kind: client.UpdateSnapshot, State: rest}})
	press(t, m, "y")
	if len(be.ran) != 0 {
		t.Fatalf("answered an item the person never selected: %v", be.ran)
	}
	if got := frame(m); !strings.Contains(got, "The list changed") {
		t.Errorf("said nothing:\n%s", got)
	}
	press(t, m, "y") // now it has been seen
	if len(be.ran) != 1 || be.inputs[0].Values["prompt"] != "p3" {
		t.Errorf("ran %v %v", be.ran, be.inputs)
	}
}

func TestScenario_NeedsInputInbox_SaysItIsBusy(t *testing.T) {
	be := newBackend()
	m := openInbox(t, be, inboxFixture())
	_, cmd := m.Update(key("y")) // not driven: the answer is still running
	if cmd == nil {
		t.Fatal("no command")
	}
	press(t, m, "n")
	if got := frame(m); !strings.Contains(got, "Wait for") || len(be.ran) != 0 {
		t.Errorf("silent while busy (ran %v):\n%s", be.ran, got)
	}
}

func TestScenario_NeedsInputInbox_FooterShowsOnlyTheKeysThatApply(t *testing.T) {
	m := openInbox(t, newBackend(), inboxFixture())
	if got := frame(m); !strings.Contains(got, "[y] Allow") || !strings.Contains(got, "[n] Deny") {
		t.Errorf("a permission lacks y/n:\n%s", got)
	}
	press(t, m, "down")
	if got := frame(m); strings.Contains(got, "[y] Allow") || !strings.Contains(got, "[enter] Answer") {
		t.Errorf("a question:\n%s", got)
	}
	press(t, m, "down")
	press(t, m, "down")
	if got := frame(m); strings.Contains(got, "[y] Allow") || strings.Contains(got, "[enter] Answer") {
		t.Errorf("a failure:\n%s", got)
	}
}
