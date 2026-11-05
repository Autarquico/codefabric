//go:build linux || darwin

package executor

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/Autarquico/codefabric/internal/adapters/harness"
	v1 "github.com/Autarquico/codefabric/internal/api/v1"
	"github.com/Autarquico/codefabric/internal/domain"
	"github.com/Autarquico/codefabric/internal/worker/execwrap"
	"github.com/Autarquico/codefabric/internal/worker/inputsock"
	"github.com/Autarquico/codefabric/internal/worker/state"
)

// streamedFixture is a running Run of lineAdapter launched with input, as
// Claude Code is: fed by a stream, it ends only once input.eof is written.
func streamedFixture(t *testing.T) (*fixture, state.Key) {
	t.Helper()
	a := newLineAdapter()
	a.Caps.StructuredInput = true
	return streamedFixtureOf(t, a)
}

func streamedFixtureOf(t *testing.T, a harness.Adapter) (*fixture, state.Key) {
	t.Helper()
	sockets := socketDir(t)
	f, k, _ := eventsFixtureOf(t, a, func(o *Options) { o.SocketDir = sockets })
	dir := f.runDir(k)
	var l execwrap.Launch
	if ok, err := execwrap.Read(dir, execwrap.LaunchFile, &l); err != nil || !ok {
		t.Fatalf("launch: %v %v", ok, err)
	}
	l.InputPath = filepath.Join(dir, InputFIFO)
	if err := execwrap.Write(dir, execwrap.LaunchFile, l); err != nil {
		t.Fatal(err)
	}
	return f, k
}

// bridgeOf has the socket of the Run up, as serveInput does for a Run that asks
// permissions, and returns a client of it.
func bridgeOf(t *testing.T, f *fixture, k state.Key) *inputsock.Client {
	t.Helper()
	e := f.e
	e.ensureSocket(t.Context(), f.run(k))
	if e.inputOf(k) == nil {
		t.Fatal("the socket of the Run is not up")
	}
	t.Cleanup(e.closeInputs)
	path, err := inputsock.Path(f.opts.SocketDir, k.RunID, k.LeaseEpoch)
	if err != nil {
		t.Fatal(err)
	}
	return &inputsock.Client{Path: path, Wait: 10 * time.Millisecond}
}

func (f *fixture) inputEnded(k state.Key) bool {
	_, err := os.Stat(filepath.Join(f.runDir(k), execwrap.InputEOFFile))
	return err == nil
}

// askedAndAnswered is a permission p1 the bridge asked and got its allow for,
// not yet acked.
func askedAndAnswered(t *testing.T, f *fixture, k state.Key, client *inputsock.Client) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
	defer cancel()
	got := make(chan inputsock.Message, 1)
	go func() {
		ans, _ := client.Ask(ctx, inputsock.Message{PromptID: "p1", Tool: "Bash", Input: json.RawMessage(`{}`)})
		got <- ans
	}()
	waitFor(t, "the prompt", func() bool {
		n, err := state.OpenInputPrompts(t.Context(), f.db.Reader(), k)
		return err == nil && n == 1
	})
	answerOrder(t, f, k, 8001, "p1", domain.AnswerAllow)
	f.deliver(k)
	if ans := <-got; ans.Decision != domain.AnswerAllow {
		t.Fatalf("answer = %+v", ans)
	}
}

// The daemon answers a permission and dies before it hears the ack; the harness
// runs the tool and ends its turn while it is down. The restarted daemon reads
// the end of the turn with the prompt still open, and the ack comes after: that
// ack is what ends the input, or Claude Code never exits.
func TestALateAckAfterARestartEndsTheInputOfTheHarness(t *testing.T) {
	f, k := streamedFixture(t)
	client := bridgeOf(t, f, k)
	askedAndAnswered(t, f, k, client)

	f.e.closeInputs() // the daemon dies before the ack
	f.restart("01J0000000000000000000000E2")
	f.appendEvents(k, "result\n")
	f.e.step(t.Context(), k)
	if f.inputEnded(k) {
		t.Fatal("the input ended with a prompt open")
	}
	if !f.cursor(k).TurnOver {
		t.Fatal("the end of the turn was not kept")
	}

	client = bridgeOf(t, f, k) // the socket is back up; the bridge acks again
	if err := client.Confirm(t.Context(), "p1"); err != nil {
		t.Fatal(err)
	}
	if !f.inputEnded(k) {
		t.Error("the late ack did not end the input of the harness")
	}
}

// A withdrawal heard after the end of the turn ends the input as well.
func TestALateWithdrawalEndsTheInputOfTheHarness(t *testing.T) {
	f, k := streamedFixture(t)
	client := bridgeOf(t, f, k)
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
	defer cancel()
	go func() {
		_, _ = client.Ask(ctx, inputsock.Message{PromptID: "p1", Tool: "Bash", Input: json.RawMessage(`{}`)})
	}()
	waitFor(t, "the prompt", func() bool {
		n, err := state.OpenInputPrompts(t.Context(), f.db.Reader(), k)
		return err == nil && n == 1
	})
	f.appendEvents(k, "result\n")
	f.e.step(t.Context(), k)
	if f.inputEnded(k) {
		t.Fatal("the input ended with a prompt open")
	}
	if err := client.Withdraw(ctx, "p1"); err != nil {
		t.Fatal(err)
	}
	if !f.inputEnded(k) {
		t.Error("the late withdrawal did not end the input of the harness")
	}
}

// The end of the turn that a restart missed is looked at again by the
// supervisor: the ack recorded, the input is ended at its next turn.
func TestTheSupervisorEndsTheInputOfAnIdleHarness(t *testing.T) {
	f, k := streamedFixture(t)
	f.appendEvents(k, "result\n")
	if err := os.Remove(filepath.Join(f.runDir(k), execwrap.InputEOFFile)); err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	cur := f.cursor(k)
	cur.Consumed, cur.TurnOver = uint64(len("result\n")), true // drained by a daemon that died before input.eof
	if err := f.db.WithTx(t.Context(), func(tx *state.Tx) error { return state.SaveEventCursor(t.Context(), tx, cur) }); err != nil {
		t.Fatal(err)
	}
	f.e.step(t.Context(), k)
	if !f.inputEnded(k) {
		t.Error("the input of an idle harness was not ended")
	}
}

// An answer written to the harness begins a turn: the end of the turn before it
// does not end the input under it.
func TestAnAnsweredQuestionKeepsTheInputOpenForTheTurnItBegins(t *testing.T) {
	f, k := streamedFixture(t)
	if err := os.WriteFile(filepath.Join(f.runDir(k), InputFIFO), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	f.appendEvents(k, "ask\nresult\n")
	f.e.step(t.Context(), k)
	if f.inputEnded(k) {
		t.Fatal("the input ended with a question open")
	}
	blocked, err := f.e.persist(t.Context(), []v1.WorkerCommand{{Seq: 8101, Type: domain.CommandInput, Input: &v1.InputCommand{
		RunID: k.RunID, LeaseID: f.run(k).LeaseID, Epoch: k.LeaseEpoch, PromptID: "q-0-0", Kind: domain.PromptQuestion,
		Answer: v1.InputAnswer{Text: "the second"},
	}}})
	if err != nil || blocked {
		t.Fatalf("persist: blocked=%v err=%v", blocked, err)
	}
	f.e.step(t.Context(), k)
	if p, err := state.InputPromptOf(t.Context(), f.db.Reader(), k, "q-0-0"); err != nil || p.State != state.PromptAcknowledged {
		t.Fatalf("prompt = %+v (%v)", p, err)
	}
	if f.inputEnded(k) {
		t.Fatal("the input ended under the turn the answer began")
	}
	f.appendEvents(k, "result\n")
	f.e.step(t.Context(), k)
	if !f.inputEnded(k) {
		t.Error("the input did not end with the turn of the answer")
	}
}

// A launch that cannot be read says nothing about the input of the Run: its
// question is not taken for one it cannot be asked, which would stop it. The
// same bytes are read again once the launch can be.
func TestALaunchThatCannotBeReadDoesNotStopARunLaunchedWithInput(t *testing.T) {
	f, k := streamedFixture(t)
	launch := filepath.Join(f.runDir(k), execwrap.LaunchFile)
	good, err := os.ReadFile(launch) //nolint:gosec // a file of the test
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(launch, []byte("{"), 0o600); err != nil {
		t.Fatal(err)
	}
	f.appendEvents(k, "ask\n")
	unlock := f.e.lock(k)
	_, err = f.e.drainEvents(t.Context(), f.run(k), false)
	unlock()
	if err == nil {
		t.Error("a launch that cannot be read was taken for an answer")
	}
	if _, err := state.StopOrderOf(t.Context(), f.db.Reader(), k); err == nil {
		t.Fatal("a Run launched with input was stopped for asking")
	}
	if c := f.cursor(k); c.Consumed != 0 {
		t.Fatalf("the question was consumed: %+v", c)
	}

	if err := os.WriteFile(launch, good, 0o600); err != nil { //nolint:gosec // a file of the test
		t.Fatal(err)
	}
	f.e.step(t.Context(), k)
	if _, err := state.StopOrderOf(t.Context(), f.db.Reader(), k); err == nil {
		t.Error("a Run launched with input was stopped for asking")
	}
	if _, err := state.InputPromptOf(t.Context(), f.db.Reader(), k, "q-0-0"); err != nil {
		t.Errorf("the question is not a prompt: %v", err)
	}
}

// halfWritten is a harness whose input takes part of an answer and fails.
type halfWritten struct {
	lineAdapter
	sends *int
}

func (a halfWritten) SendInput(context.Context, harness.Host, harness.Handle, harness.Input) error {
	*a.sends++
	return errors.New("3 of 40 bytes written: broken pipe")
}

// An answer that may be half in the stream is not written again: the harness
// would read a broken line and then the whole one.
func TestAnAnswerThatFailedHalfWayIsNotSentAgain(t *testing.T) {
	a := newLineAdapter()
	a.Caps.StructuredInput = true
	sends := 0
	f, k := streamedFixtureOf(t, halfWritten{a, &sends})
	f.appendEvents(k, "ask\n")
	f.e.step(t.Context(), k)
	blocked, err := f.e.persist(t.Context(), []v1.WorkerCommand{{Seq: 8201, Type: domain.CommandInput, Input: &v1.InputCommand{
		RunID: k.RunID, LeaseID: f.run(k).LeaseID, Epoch: k.LeaseEpoch, PromptID: "q-0-0", Kind: domain.PromptQuestion,
		Answer: v1.InputAnswer{Text: "the second"},
	}}})
	if err != nil || blocked {
		t.Fatalf("persist: blocked=%v err=%v", blocked, err)
	}
	f.e.step(t.Context(), k)
	f.e.step(t.Context(), k)
	if sends != 1 {
		t.Errorf("the answer was written %d times", sends)
	}
	if orders, err := state.PendingInputOrders(t.Context(), f.db.Reader(), k); err != nil || len(orders) != 0 {
		t.Errorf("orders = %v (%v)", orders, err)
	}
}
