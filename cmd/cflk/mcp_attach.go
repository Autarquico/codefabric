package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"strings"

	v1 "github.com/Autarquico/codeflock/internal/api/v1"
	"github.com/Autarquico/codeflock/internal/domain"
	"github.com/Autarquico/codeflock/internal/mcp"
)

// execAttacher is the mcp.Attacher of `cflk mcp serve`: it runs this very
// binary as `cflk attach --json`, which knows how to open a window in the
// terminal of the session, and reports what it says. The child inherits the
// environment of the server (TMUX, TERM_PROGRAM) and has no stdin, so it never
// takes the server's JSON-RPC stream.
type execAttacher struct {
	exe        string // empty: this binary
	controller string
}

func newExecAttacher(controller string) execAttacher { return execAttacher{controller: controller} }

func (a execAttacher) argv(req mcp.AttachRequest) []string {
	args := []string{"attach", "--json"}
	if a.controller != "" {
		args = append(args, "--controller", a.controller)
	}
	if req.Project != "" {
		args = append(args, "--project", req.Project)
	}
	if req.ReadOnly {
		args = append(args, "--read-only")
	}
	return append(args, "--", req.Ref)
}

func (a execAttacher) Attach(ctx context.Context, req mcp.AttachRequest) (mcp.AttachResult, error) {
	exe := a.exe
	if exe == "" {
		self, err := os.Executable()
		if err != nil {
			return mcp.AttachResult{}, err
		}
		exe = self
	}
	var stdout, stderr bytes.Buffer
	cmd := exec.CommandContext(ctx, exe, a.argv(req)...) //nolint:gosec // this binary, with the flags built above
	cmd.Stdout, cmd.Stderr = &stdout, &stderr            // Stdin nil is /dev/null
	runErr := cmd.Run()
	if runErr == nil {
		var res mcp.AttachResult
		if err := json.Unmarshal(stdout.Bytes(), &res); err != nil || res.Run == "" {
			return mcp.AttachResult{}, fmt.Errorf("cflk attach printed no result: %q", strings.TrimSpace(stdout.String()))
		}
		return res, nil
	}
	if ctx.Err() != nil {
		return mcp.AttachResult{}, ctx.Err()
	}
	var env v1.ErrorEnvelope
	if err := json.Unmarshal(stdout.Bytes(), &env); err == nil && env.Error.Code != "" {
		return mcp.AttachResult{}, domain.NewError(env.Error.Code, env.Error.Message, env.Error.Details)
	}
	return mcp.AttachResult{}, fmt.Errorf("cflk attach: %w: %s", runErr, strings.TrimSpace(stderr.String()))
}
