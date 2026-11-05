package daemon

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
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
