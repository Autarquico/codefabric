package daemon

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Autarquico/codefabric/internal/worker/inputsock"
)

// daemonSide answers every question with decision, and acks.
type daemonSide struct{ decision string }

func (d daemonSide) Request(c *inputsock.Conn, m inputsock.Message) {
	_ = c.Send(inputsock.Message{Op: inputsock.OpAnswer, PromptID: m.PromptID, Decision: d.decision, Message: "no thanks"})
}

func (daemonSide) Ack(c *inputsock.Conn, m inputsock.Message) {
	_ = c.Send(inputsock.Message{Op: inputsock.OpAcked, PromptID: m.PromptID})
}

func (daemonSide) Gone(*inputsock.Conn) {}

func (daemonSide) Withdraw(c *inputsock.Conn, m inputsock.Message) {
	_ = c.Send(inputsock.Message{Op: inputsock.OpWithdrawn, PromptID: m.PromptID})
}

// callApprove runs the bridge over a pipe, as Claude Code does, and returns the
// text of the result of one call of the tool.
func callApprove(t *testing.T, decision string) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "cfb")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	path := filepath.Join(dir, "b.sock")
	srv, err := inputsock.Listen(path, daemonSide{decision})
	if err != nil {
		t.Fatal(err)
	}
	defer srv.Close()

	inR, inW := io.Pipe()
	outR, outW := io.Pipe()
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() {
		done <- serveMCP(ctx, inR, outW, newSocketAsker(&inputsock.Client{Path: path, Wait: 10 * time.Millisecond}))
		_ = outW.Close()
	}()
	out := bufio.NewScanner(outR)
	send := func(line string) { _, _ = io.WriteString(inW, line+"\n") }

	go func() {
		send(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-03-26"}}`)
		send(`{"jsonrpc":"2.0","method":"notifications/initialized"}`)
		send(`{"jsonrpc":"2.0","id":2,"method":"tools/list"}`)
		send(`{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"approve","arguments":{"tool_name":"Bash","input":{"command":"ls"}}}}`)
	}()
	var text string
	for out.Scan() {
		var r struct {
			ID     int `json:"id"`
			Result struct {
				Tools   []struct{ Name string } `json:"tools"`
				Content []struct{ Text string } `json:"content"`
			} `json:"result"`
		}
		if err := json.Unmarshal(out.Bytes(), &r); err != nil {
			t.Fatal(err)
		}
		if r.ID == 2 && (len(r.Result.Tools) != 1 || r.Result.Tools[0].Name != "approve") {
			t.Errorf("tools = %+v", r.Result.Tools)
		}
		if r.ID == 3 {
			text = r.Result.Content[0].Text
			break
		}
	}
	_ = inW.Close()
	if err := <-done; err != nil {
		t.Errorf("serveMCP: %v", err)
	}
	return text
}

func TestTheBridgeReturnsAnAllowWithTheInputUnchanged(t *testing.T) {
	got := callApprove(t, "allow")
	if !strings.Contains(got, `"behavior":"allow"`) || !strings.Contains(got, `"updatedInput":{"command":"ls"}`) {
		t.Errorf("result = %s", got)
	}
}

func TestTheBridgeReturnsADenyWithItsMessage(t *testing.T) {
	got := callApprove(t, "deny")
	if !strings.Contains(got, `"behavior":"deny"`) || !strings.Contains(got, "no thanks") {
		t.Errorf("result = %s", got)
	}
}

// recorder is a daemon that answers nothing unless answer is set, and records
// what it hears; acks only when acking.
type recorder struct {
	mu        sync.Mutex
	answer    string
	acking    bool
	requests  []string
	acks      []string
	withdrawn []string
}

func (r *recorder) Request(c *inputsock.Conn, m inputsock.Message) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.requests = append(r.requests, m.PromptID)
	if r.answer != "" {
		_ = c.Send(inputsock.Message{Op: inputsock.OpAnswer, PromptID: m.PromptID, Decision: r.answer})
	}
}

func (r *recorder) Ack(c *inputsock.Conn, m inputsock.Message) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if !r.acking {
		_ = c.Close() // the daemon dies before it records the delivery
		return
	}
	r.acks = append(r.acks, m.PromptID)
	_ = c.Send(inputsock.Message{Op: inputsock.OpAcked, PromptID: m.PromptID})
}

func (r *recorder) Withdraw(c *inputsock.Conn, m inputsock.Message) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.withdrawn = append(r.withdrawn, m.PromptID)
	_ = c.Send(inputsock.Message{Op: inputsock.OpWithdrawn, PromptID: m.PromptID})
}

func (*recorder) Gone(*inputsock.Conn) {}

func (r *recorder) seen(f func(*recorder) bool) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return f(r)
}

func bridgeSocket(t *testing.T, h inputsock.Handler) (string, *inputsock.Server) {
	t.Helper()
	dir, err := os.MkdirTemp("", "cfb")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	path := filepath.Join(dir, "b.sock")
	srv, err := inputsock.Listen(path, h)
	if err != nil {
		t.Fatal(err)
	}
	return path, srv
}

func eventually(t *testing.T, what string, ok func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for !ok() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// A call Claude Code cancels withdraws its prompt: nobody is to answer it, and
// it must not hold the input of the harness open.
func TestACancelledCallWithdrawsItsPrompt(t *testing.T) {
	d := &recorder{}
	path, srv := bridgeSocket(t, d)
	defer srv.Close()
	inR, inW := io.Pipe()
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() {
		done <- serveMCP(ctx, inR, io.Discard, newSocketAsker(&inputsock.Client{Path: path, Wait: 10 * time.Millisecond}))
	}()
	_, _ = io.WriteString(inW, `{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"approve","arguments":{"tool_name":"Bash","input":{}}}}`+"\n")
	eventually(t, "the question", func() bool { return d.seen(func(r *recorder) bool { return len(r.requests) == 1 }) })
	_, _ = io.WriteString(inW, `{"jsonrpc":"2.0","method":"notifications/cancelled","params":{"requestId":3}}`+"\n")
	eventually(t, "the withdrawal", func() bool {
		return d.seen(func(r *recorder) bool { return len(r.withdrawn) == 1 && r.withdrawn[0] == r.requests[0] })
	})
	_ = inW.Close()
	if err := <-done; err != nil {
		t.Errorf("serveMCP: %v", err)
	}
}

// The bridge keeps acking an answer until a daemon records it, however long the
// daemon is away: the input of the harness ends only once it is recorded.
func TestTheBridgeAcksUntilADaemonRecordsIt(t *testing.T) {
	d := &recorder{answer: "allow"}
	path, srv := bridgeSocket(t, d)
	a := newSocketAsker(&inputsock.Client{Path: path, Wait: 10 * time.Millisecond})
	ctx, cancel := context.WithCancel(t.Context())
	decision, _, err := a.ask(ctx, "Bash", json.RawMessage(`{}`))
	cancel() // the call is over: its context ends, the ack goes on
	if err != nil || decision != "allow" {
		t.Fatalf("ask: %s %v", decision, err)
	}
	srv.Close() // the daemon goes down before it records the ack
	time.Sleep(100 * time.Millisecond)

	d.mu.Lock()
	d.acking = true
	d.mu.Unlock()
	srv2, err := inputsock.Listen(path, d)
	if err != nil {
		t.Fatal(err)
	}
	defer srv2.Close()
	eventually(t, "the ack", func() bool { return d.seen(func(r *recorder) bool { return len(r.acks) == 1 }) })
	a.wait(time.Second)
}

// Under permissions: ask, Claude Code may hand AskUserQuestion to the bridge as
// well as write it to its stream. The stream is what makes it a prompt (with
// its answer through the FIFO): the bridge lets the tool run without asking the
// daemon, so that one question is one prompt in the inbox.
func TestAQuestionIsNotAskedAgainAsAPermission(t *testing.T) {
	d := &recorder{answer: "deny", acking: true}
	path, srv := bridgeSocket(t, d)
	defer srv.Close()
	inR, inW := io.Pipe()
	outR, outW := io.Pipe()
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() {
		done <- serveMCP(ctx, inR, outW, newSocketAsker(&inputsock.Client{Path: path, Wait: 10 * time.Millisecond}))
		_ = outW.Close()
	}()
	const question = `{"questions":[{"question":"which one?","options":[{"label":"a"},{"label":"b"}]}]}`
	go func() {
		_, _ = io.WriteString(inW, `{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"approve","arguments":{"tool_name":"AskUserQuestion","input":`+question+`}}}`+"\n")
	}()
	out := bufio.NewScanner(outR)
	if !out.Scan() {
		t.Fatalf("no result: %v", out.Err())
	}
	var r struct {
		Result struct {
			Content []struct{ Text string } `json:"content"`
		} `json:"result"`
	}
	if err := json.Unmarshal(out.Bytes(), &r); err != nil || len(r.Result.Content) != 1 {
		t.Fatalf("result %s: %v", out.Bytes(), err)
	}
	var res struct{ Behavior, Message string }
	if err := json.Unmarshal([]byte(r.Result.Content[0].Text), &res); err != nil || res.Behavior != "deny" || !strings.Contains(res.Message, "next user message") {
		t.Errorf("result = %s, want a deny that defers the answer to the next user message", r.Result.Content[0].Text)
	}
	_ = inW.Close()
	if err := <-done; err != nil {
		t.Errorf("serveMCP: %v", err)
	}
	if d.seen(func(r *recorder) bool { return len(r.requests) != 0 }) {
		t.Error("the bridge asked the daemon about a question: it would be a second prompt")
	}
}
