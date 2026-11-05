package executor

import (
	"context"
	"database/sql"
	"encoding/json"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/Autarquico/codefabric/internal/adapters/harness/command"
	v1 "github.com/Autarquico/codefabric/internal/api/v1"
	"github.com/Autarquico/codefabric/internal/domain"
	"github.com/Autarquico/codefabric/internal/support/redact"
	"github.com/Autarquico/codefabric/internal/worker/execwrap"
	"github.com/Autarquico/codefabric/internal/worker/inputsock"
	"github.com/Autarquico/codefabric/internal/worker/state"
)

// The summary and the options of a prompt leave the Worker without the secrets
// of the Run: the Controller has none to redact them with.
func TestOpenPromptRedactsTheSummaryAndTheOptionsWithTheSecretsOfTheRun(t *testing.T) {
	f := newFixture(t)
	k := f.started("ask")
	const secret = "s3cr3t-value-9f2c"
	if err := f.e.jit.Deliver(f.runDir(k), 1, []redact.Needle{{Ref: "github.personal", Value: []byte(secret)}}, func() error { return nil }); err != nil {
		t.Fatal(err)
	}

	err := f.e.openPrompt(t.Context(), f.run(k), "q-1", domain.PromptQuestion, "use "+secret+"?", []string{secret, "no"}, true)
	if err != nil {
		t.Fatal(err)
	}
	reports := f.reports(k)
	got := reports[len(reports)-1]
	if got.Kind != v1.ReportInputRequested || strings.Contains(got.Summary, secret) || slices.ContainsFunc(got.Options, func(o string) bool { return strings.Contains(o, secret) }) {
		t.Errorf("input_requested = %+v", got)
	}
	if len(got.Options) != 2 || got.Options[1] != "no" {
		t.Errorf("options = %v", got.Options)
	}
}

// Text that cannot be redacted is withheld, never reported as it is.
func TestOpenPromptWithholdsWhatItCannotRedact(t *testing.T) {
	f := newFixture(t)
	k := f.started("ask")
	if err := f.e.jit.Deliver(f.runDir(k), 1, []redact.Needle{{Ref: "github.personal", Value: []byte("s3cr3t-value-9f2c")}}, func() error { return nil }); err != nil {
		t.Fatal(err)
	}
	if err := corruptDelivery(f.runDir(k)); err != nil {
		t.Skipf("cannot corrupt the delivery: %v", err)
	}
	if err := f.e.openPrompt(t.Context(), f.run(k), "q-1", domain.PromptQuestion, "s3cr3t-value-9f2c", nil, true); err != nil {
		t.Fatal(err)
	}
	reports := f.reports(k)
	if got := reports[len(reports)-1].Summary; got != withheld {
		t.Errorf("summary = %q", got)
	}
}

// An order for a Run that is not up here, or under another lease, is dropped:
// the cursor moves and nothing is queued.
func TestAnInputOrderForARunThatIsNotUpIsIgnored(t *testing.T) {
	f := newFixture(t)
	accepted := f.accepted("not started") // not running: nobody to answer
	running := f.started("running")
	ghost := state.Key{RunID: runID(9999), LeaseEpoch: 1}

	order := func(seq uint64, k state.Key, lease string) v1.WorkerCommand {
		return v1.WorkerCommand{Seq: seq, Type: domain.CommandInput, Input: &v1.InputCommand{
			RunID: k.RunID, LeaseID: lease, Epoch: k.LeaseEpoch, PromptID: "p1", Kind: domain.PromptPermission,
			Answer: v1.InputAnswer{Decision: domain.AnswerAllow},
		}}
	}
	cmds := []v1.WorkerCommand{
		order(5001, accepted, f.run(accepted).LeaseID),
		order(5002, ghost, "lease_x"),
		order(5003, running, "another-lease"),
	}
	blocked, err := f.e.persist(t.Context(), cmds)
	if err != nil || blocked {
		t.Fatalf("persist: blocked=%v err=%v", blocked, err)
	}
	for _, k := range []state.Key{accepted, running, ghost} {
		orders, err := state.PendingInputOrders(t.Context(), f.db.Reader(), k)
		if err != nil || len(orders) != 0 {
			t.Errorf("%s: orders = %v (%v)", k.RunID, orders, err)
		}
	}
}

// corruptDelivery makes the secrets of the run directory unreadable.
func corruptDelivery(runDir string) error {
	return os.WriteFile(filepath.Join(runDir, "secrets", "manifest.json"), []byte("{"), 0o600)
}

// socketDir is a directory whose sockets fit: t.TempDir() is too long on macOS.
func socketDir(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("/tmp", "cfs")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	return dir
}

// askSocket is a started Run with the socket of its bridge up.
func askSocket(t *testing.T, mutate ...func(*Options)) (*fixture, state.Key, *inputsock.Client) {
	t.Helper()
	dir := socketDir(t)
	f := newFixture(t, append(mutate, func(o *Options) { o.SocketDir = dir })...)
	k := f.started("ask")
	f.e.ensureSocket(t.Context(), f.run(k))
	if f.e.inputOf(k) == nil {
		t.Fatal("the socket of the Run is not up")
	}
	t.Cleanup(f.e.closeInputs)
	path, err := inputsock.Path(dir, k.RunID, k.LeaseEpoch)
	if err != nil {
		t.Fatal(err)
	}
	return f, k, &inputsock.Client{Path: path, Wait: 10 * time.Millisecond}
}

// The supervisor takes the socket of a Run down under the lock of the Run, and a
// question of the bridge may be waiting for that lock: the supervisor does not
// wait for it.
func TestTakingTheSocketDownUnderTheLockOfTheRunDoesNotWaitForItsHandlers(t *testing.T) {
	f, k, client := askSocket(t)
	unlock := f.e.lock(k)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	go func() {
		_, _ = client.Ask(ctx, inputsock.Message{PromptID: "p1", Tool: "Bash", Input: json.RawMessage(`{}`)})
	}()
	waitFor(t, "a handler waiting for the lock", func() bool {
		f.e.mu.Lock()
		defer f.e.mu.Unlock()
		l := f.e.locks[k]
		return l != nil && l.refs == 2
	})
	done := make(chan struct{})
	go func() {
		f.e.dropInput(t.Context(), k)
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("taking the socket down waited for a handler that waits for the lock it holds")
	}
	unlock()
	cancel()
}

func execSQL(t *testing.T, f *fixture, query string) {
	t.Helper()
	if _, err := f.db.Reader().(*sql.DB).ExecContext(t.Context(), query); err != nil {
		t.Fatal(err)
	}
}

// answerOrder is the `input` order of the Controller for a permission of k.
func answerOrder(t *testing.T, f *fixture, k state.Key, seq uint64, promptID, decision string) {
	t.Helper()
	blocked, err := f.e.persist(t.Context(), []v1.WorkerCommand{{Seq: seq, Type: domain.CommandInput, Input: &v1.InputCommand{
		RunID: k.RunID, LeaseID: f.run(k).LeaseID, Epoch: k.LeaseEpoch, PromptID: promptID, Kind: domain.PromptPermission,
		Answer: v1.InputAnswer{Decision: decision},
	}}})
	if err != nil || blocked {
		t.Fatalf("persist: blocked=%v err=%v", blocked, err)
	}
}

func (f *fixture) deliver(k state.Key) {
	unlock := f.e.lock(k)
	defer unlock()
	f.e.deliverInputs(f.t.Context(), f.run(k))
}

// A question the daemon failed to record is not left waiting on a connection
// nobody answers: the bridge is made to ask again, and gets its answer.
func TestAPermissionThatFailedToBeRecordedIsAskedAgain(t *testing.T) {
	logs := &syncBuffer{}
	f, k, client := askSocket(t, func(o *Options) { o.Log = slog.New(slog.NewTextHandler(logs, nil)) })
	execSQL(t, f, `CREATE TRIGGER fail_prompts BEFORE INSERT ON input_prompts BEGIN SELECT RAISE(ABORT, 'disk full'); END`)
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
	defer cancel()
	got := make(chan inputsock.Message, 1)
	go func() {
		ans, _ := client.Ask(ctx, inputsock.Message{PromptID: "p1", Tool: "Bash", Input: json.RawMessage(`{}`)})
		got <- ans
	}()
	waitFor(t, "a failed record", func() bool { return strings.Contains(logs.String(), "recording a permission prompt failed") })
	execSQL(t, f, `DROP TRIGGER fail_prompts`)
	waitFor(t, "the prompt asked again", func() bool {
		_, err := state.InputPromptOf(t.Context(), f.db.Reader(), k, "p1")
		return err == nil
	})
	answerOrder(t, f, k, 7001, "p1", domain.AnswerAllow)
	f.deliver(k)
	select {
	case ans := <-got:
		if ans.Decision != domain.AnswerAllow {
			t.Errorf("answer = %+v", ans)
		}
	case <-ctx.Done():
		t.Fatal("the bridge got no answer")
	}
}

// A withdrawn prompt is closed: it frees its place among the open prompts, so
// that the input of the harness can end, and an answer for it is dropped.
func TestAWithdrawnPermissionIsClosed(t *testing.T) {
	f, k, client := askSocket(t)
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
	defer cancel()
	go func() {
		_, _ = client.Ask(ctx, inputsock.Message{PromptID: "p1", Tool: "Bash", Input: json.RawMessage(`{}`)})
	}()
	waitFor(t, "the prompt", func() bool {
		n, err := state.OpenInputPrompts(t.Context(), f.db.Reader(), k)
		return err == nil && n == 1
	})
	if err := client.Withdraw(ctx, "p1"); err != nil {
		t.Fatal(err)
	}
	if n, err := state.OpenInputPrompts(t.Context(), f.db.Reader(), k); err != nil || n != 0 {
		t.Errorf("open prompts = %d (%v)", n, err)
	}
	answerOrder(t, f, k, 7002, "p1", domain.AnswerAllow)
	f.deliver(k)
	if orders, err := state.PendingInputOrders(t.Context(), f.db.Reader(), k); err != nil || len(orders) != 0 {
		t.Errorf("orders = %v (%v)", orders, err)
	}
	if delivered := slices.ContainsFunc(f.reports(k), func(r v1.RunReportEntry) bool { return r.Kind == v1.ReportInputDelivered }); delivered {
		t.Error("a withdrawn prompt was reported delivered")
	}
}

// Whether a Run is answered while it runs is what it was launched with, not what
// the daemon announces now: Claude Code may have changed since the launch.
func TestARunKeepsTheInputItWasLaunchedWith(t *testing.T) {
	f := newFixture(t)
	k := f.started("plain")
	r, dir := f.run(k), f.runDir(k)
	adapter, _ := f.e.harnesses.Get(command.ID)
	if h, _ := f.e.handleOf(adapter, r, parseSpec(r.Spec)); h.InputPath != "" || launched(t, f, k) {
		t.Fatalf("a Run launched without input has %q", h.InputPath)
	}
	var l execwrap.Launch
	if ok, err := execwrap.Read(dir, execwrap.LaunchFile, &l); err != nil || !ok {
		t.Fatalf("launch: %v %v", ok, err)
	}
	l.InputPath = filepath.Join(dir, InputFIFO)
	if err := execwrap.Write(dir, execwrap.LaunchFile, l); err != nil {
		t.Fatal(err)
	}
	// This daemon announces no structured input: the Run still has the FIFO it was launched with.
	if h, _ := f.e.handleOf(adapter, r, parseSpec(r.Spec)); h.InputPath != l.InputPath || !launched(t, f, k) {
		t.Errorf("a Run launched with input has %q", h.InputPath)
	}
}

func launched(t *testing.T, f *fixture, k state.Key) bool {
	t.Helper()
	ok, err := f.e.launchedWithInput(k)
	if err != nil {
		t.Fatal(err)
	}
	return ok
}

// A secret split by a character that does not print is still redacted: that
// character is removed before the redaction, not after it.
func TestOpenPromptRedactsASecretSplitByAFormatCharacter(t *testing.T) {
	f := newFixture(t)
	k := f.started("ask")
	const secret = "s3cr3t-value-9f2c"
	if err := f.e.jit.Deliver(f.runDir(k), 1, []redact.Needle{{Ref: "github.personal", Value: []byte(secret)}}, func() error { return nil }); err != nil {
		t.Fatal(err)
	}
	split := "s3cr3t-\u200bvalue-9f2c"
	if err := f.e.openPrompt(t.Context(), f.run(k), "q-1", domain.PromptQuestion, "use "+split+"?", []string{split}, true); err != nil {
		t.Fatal(err)
	}
	reports := f.reports(k)
	got := reports[len(reports)-1]
	if strings.Contains(got.Summary, secret) || slices.ContainsFunc(got.Options, func(o string) bool { return strings.Contains(o, secret) }) {
		t.Errorf("input_requested = %+v", got)
	}
}
