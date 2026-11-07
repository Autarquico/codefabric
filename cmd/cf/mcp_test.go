package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"io"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/Autarquico/codefabric/internal/cli"
	"github.com/Autarquico/codefabric/internal/mcp"
)

// noClient stands for the Controller: serving never calls it in these tests.
type noClient struct{ mcp.Client }

func runMCP(ctx context.Context, open func(session, controller string) (mcp.Client, error), stdin io.Reader, stdout, stderr io.Writer, args ...string) int {
	return cli.RunContextWith(ctx, cli.Options{}, append([]string{"mcp", "serve"}, args...), stdin, stdout, stderr, newMCPCommand(open))
}

func TestMCPServeWritesOnlyJSONRPCToStdout(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
	defer cancel()

	var session string
	open := func(s, _ string) (mcp.Client, error) { session = s; return noClient{}, nil }
	stdinR, stdinW := io.Pipe()
	stdoutR, stdoutW := io.Pipe()
	var stderr bytes.Buffer
	done := make(chan int, 1)
	go func() {
		done <- runMCP(ctx, open, stdinR, stdoutW, &stderr)
		_ = stdoutW.Close()
	}()

	send := func(msg string) {
		if _, err := io.WriteString(stdinW, msg+"\n"); err != nil {
			t.Fatal(err)
		}
	}
	lines := bufio.NewScanner(stdoutR)
	var seen []string
	next := func() map[string]any {
		t.Helper()
		if !lines.Scan() {
			t.Fatalf("stdout ended: %v", lines.Err())
		}
		seen = append(seen, lines.Text())
		var frame map[string]any
		if err := json.Unmarshal(lines.Bytes(), &frame); err != nil {
			t.Fatalf("stdout carries something that is not JSON: %q", lines.Text())
		}
		return frame
	}

	send(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"t","version":"0"}}}`)
	next()
	send(`{"jsonrpc":"2.0","method":"notifications/initialized"}`)
	send(`{"jsonrpc":"2.0","id":2,"method":"tools/list"}`)
	list := next()
	_ = stdinW.Close()
	for lines.Scan() {
		seen = append(seen, lines.Text())
	}
	if code := <-done; code != 0 {
		t.Fatalf("exit %d, stderr %q", code, stderr.String())
	}

	for _, line := range seen {
		var frame struct {
			JSONRPC string `json:"jsonrpc"`
		}
		if err := json.Unmarshal([]byte(line), &frame); err != nil || frame.JSONRPC != "2.0" {
			t.Errorf("stdout line is not JSON-RPC: %q", line)
		}
	}
	tools, _ := list["result"].(map[string]any)["tools"].([]any)
	if len(tools) == 0 {
		t.Errorf("tools/list = %v", list)
	}
	if !strings.Contains(stderr.String(), "serving profile execute") {
		t.Errorf("the diagnostics should go to stderr: %q", stderr.String())
	}
	if !regexp.MustCompile(`^[A-Za-z0-9-]{8,64}$`).MatchString(session) {
		t.Errorf("session %q is not one the Controller accepts", session)
	}
}

func TestMCPServeRejectsWhatItCannotServe(t *testing.T) {
	open := func(string, string) (mcp.Client, error) { return noClient{}, nil }
	for name, args := range map[string][]string{
		"json":    {"--json"},
		"profile": {"--profile", "root"},
	} {
		var out, errOut bytes.Buffer
		code := runMCP(t.Context(), open, strings.NewReader(""), &out, &errOut, args...)
		if code != 2 {
			t.Errorf("%s: exit %d, want 2 (stderr %q)", name, code, errOut.String())
		}
		if strings.Contains(out.String(), `"jsonrpc"`) {
			t.Errorf("%s: JSON-RPC on stdout: %q", name, out.String())
		}
	}
}

// The binary's serve and the CLI's install|uninstall meet under one mcp command.
func TestMCPCommandListsServeInstallAndUninstall(t *testing.T) {
	var out, errOut bytes.Buffer
	code := cli.RunContextWith(t.Context(), cli.Options{}, []string{"mcp", "--help"}, strings.NewReader(""), &out, &errOut, commands()...)
	if code != 0 {
		t.Fatalf("exit %d: %s", code, errOut.String())
	}
	for _, sub := range []string{"serve", "install", "uninstall"} {
		if !regexp.MustCompile(`(?m)^\s+` + sub + `\s`).MatchString(out.String()) {
			t.Errorf("cf mcp --help does not list %q:\n%s", sub, out.String())
		}
	}
}
