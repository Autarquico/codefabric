package main

import (
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/Autarquico/codeflock/internal/cli"
	"github.com/Autarquico/codeflock/internal/domain"
	"github.com/Autarquico/codeflock/internal/mcp"
)

// fakeCflk writes an executable that records its argv, TERM_PROGRAM and whether
// stdin is empty, then runs body. It returns the executable and the record.
func fakeCflk(t *testing.T, body string) (exe, record string) {
	t.Helper()
	dir := t.TempDir()
	record = filepath.Join(dir, "record")
	exe = filepath.Join(dir, "cflk")
	script := "#!/bin/sh\n" +
		"for a in \"$@\"; do printf '%s\\n' \"$a\"; done > " + record + "\n" +
		"printf 'env=%s\\n' \"$TERM_PROGRAM\" >> " + record + "\n" +
		"printf 'stdin=%s\\n' \"$(cat)\" >> " + record + "\n" +
		body + "\n"
	err := os.WriteFile(exe, []byte(script), 0o700) //nolint:gosec // a script the test must be able to execute
	if err != nil {
		t.Fatal(err)
	}
	return exe, record
}

func TestExecAttacher_RunsAttachJSONWithTheSessionEnvironment(t *testing.T) {
	t.Setenv("TERM_PROGRAM", "iTerm.app")
	exe, record := fakeCflk(t, `echo '{"opened":"iterm2","run":"run-1"}'`)
	a := execAttacher{exe: exe, controller: "ctl.example:7443"}
	got, err := a.Attach(t.Context(), mcp.AttachRequest{Ref: "t1", Project: "web", ReadOnly: true})
	if err != nil || got != (mcp.AttachResult{Opened: "iterm2", Run: "run-1"}) {
		t.Fatalf("Attach = %+v, %v", got, err)
	}
	b, _ := os.ReadFile(record) //nolint:gosec // a path of the test's own temp dir
	lines := strings.Split(strings.TrimSpace(string(b)), "\n")
	want := []string{"attach", "--json", "--controller", "ctl.example:7443", "--project", "web", "--read-only", "--", "t1", "env=iTerm.app", "stdin="}
	if !slices.Equal(lines, want) {
		t.Errorf("child saw %q, want %q", lines, want)
	}
}

func TestExecAttacher_OmitsWhatWasNotAsked(t *testing.T) {
	exe, record := fakeCflk(t, `echo '{"opened":"tmux","run":"r"}'`)
	if _, err := (execAttacher{exe: exe}).Attach(t.Context(), mcp.AttachRequest{Ref: "-x"}); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(record) //nolint:gosec // a path of the test's own temp dir
	if got := strings.Join(strings.Split(string(b), "\n")[:4], " "); got != "attach --json -- -x" {
		t.Errorf("argv = %q, want a ref after -- and no flags", got)
	}
}

func TestExecAttacher_FailureKeepsTheEnvelopeOfTheCLI(t *testing.T) {
	exe, _ := fakeCflk(t, `echo '{"error":{"code":"CF_INVALID_REQUEST","message":"no terminal","details":{"command":"'cflk' 'attach' '--' 'r1'"}}}'; exit 2`)
	_, err := (execAttacher{exe: exe}).Attach(t.Context(), mcp.AttachRequest{Ref: "r1"})
	var coded *domain.Error
	if !errors.As(err, &coded) || coded.Code != domain.CodeInvalidRequest || coded.Message != "no terminal" || coded.Details["command"] == nil {
		t.Fatalf("err = %#v, want the coded error of the envelope with its command", err)
	}
}

func TestExecAttacher_WithoutAValidEnvelopeIsNotCoded(t *testing.T) {
	for name, body := range map[string]string{
		"garbage on failure": `echo garbage; echo boom >&2; exit 1`,
		"garbage on success": `echo garbage`,
		"empty on success":   `true`,
	} {
		exe, _ := fakeCflk(t, body)
		_, err := (execAttacher{exe: exe}).Attach(t.Context(), mcp.AttachRequest{Ref: "r1"})
		var coded *domain.Error
		if err == nil || errors.As(err, &coded) {
			t.Errorf("%s: err = %#v, want an uncoded error", name, err)
		}
	}
}

func TestExecAttacher_ActsForTheMCPSession(t *testing.T) {
	const session = "0123456789abcdef0123456789abcdef"
	t.Setenv(cli.MCPSessionEnv, "inherited")
	exe, record := fakeCflk(t, `printf 'session=%s\n' "$CFLK_MCP_SESSION" >> "$(dirname "$0")/record"; echo '{"opened":"tmux","run":"r"}'`)
	if _, err := (execAttacher{exe: exe, session: session}).Attach(t.Context(), mcp.AttachRequest{Ref: "r1"}); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(record) //nolint:gosec // a path of the test's own temp dir
	if !strings.Contains(string(b), "session="+session+"\n") {
		t.Errorf("child saw %q, want the session of the server", b)
	}
}

func TestExecAttacher_WithoutASessionLeavesTheEnvironmentAlone(t *testing.T) {
	t.Setenv(cli.MCPSessionEnv, "")
	exe, record := fakeCflk(t, `printf 'session=%s\n' "$CFLK_MCP_SESSION" >> "$(dirname "$0")/record"; echo '{"opened":"tmux","run":"r"}'`)
	if _, err := (execAttacher{exe: exe}).Attach(t.Context(), mcp.AttachRequest{Ref: "r1"}); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(record) //nolint:gosec // a path of the test's own temp dir
	if !strings.Contains(string(b), "session=\n") {
		t.Errorf("child saw %q", b)
	}
}
