package mcp_test

import (
	"context"
	"errors"
	"testing"

	"github.com/Autarquico/codeflock/internal/domain"
	"github.com/Autarquico/codeflock/internal/mcp"
	"github.com/Autarquico/codeflock/internal/support/clock"
)

// fakeAttacher records what it is asked and answers with err, or with a window
// that opened.
type fakeAttacher struct {
	got []mcp.AttachRequest
	err error
}

func (f *fakeAttacher) Attach(_ context.Context, req mcp.AttachRequest) (mcp.AttachResult, error) {
	f.got = append(f.got, req)
	if f.err != nil {
		return mcp.AttachResult{}, f.err
	}
	return mcp.AttachResult{Opened: "tmux", Run: "run-1"}, nil
}

func TestScenario_RunsAttach_DesdeElMCP(t *testing.T) {
	a := &fakeAttacher{}
	s := connectWith(t, mcp.Execute, &fake{}, a, clock.Real{})
	res := call(t, s, "runs_attach", map[string]any{"ref": "t1", "project": "web"})
	if res.IsError {
		t.Fatalf("runs_attach failed: %v", res.Content)
	}
	got := structured(t, res)
	if got["opened"] != "tmux" || got["run"] != "run-1" {
		t.Errorf("result = %v, want {opened: tmux, run: run-1}", got)
	}
	want := mcp.AttachRequest{Ref: "t1", Project: "web"}
	if len(a.got) != 1 || a.got[0] != want {
		t.Errorf("Attacher received %+v, want %+v", a.got, want)
	}
}

func TestScenario_RunsAttach_MCPDeSoloLectura(t *testing.T) {
	for _, tc := range []struct {
		profile  mcp.Profile
		args     map[string]any
		readOnly bool
	}{
		{mcp.Read, map[string]any{"ref": "r1", "read_only": false}, true},
		{mcp.Read, map[string]any{"ref": "r1"}, true},
		{mcp.Execute, map[string]any{"ref": "r1", "read_only": true}, true},
		{mcp.Execute, map[string]any{"ref": "r1"}, false},
		{mcp.Modify, map[string]any{"ref": "r1", "read_only": false}, false},
	} {
		a := &fakeAttacher{}
		call(t, connectWith(t, tc.profile, &fake{}, a, clock.Real{}), "runs_attach", tc.args)
		if len(a.got) != 1 || a.got[0].ReadOnly != tc.readOnly {
			t.Errorf("%s %v: read-only = %+v, want %v", tc.profile, tc.args, a.got, tc.readOnly)
		}
	}
}

func TestRunsAttachKeepsTheCodeAndDetailsOfTheFallback(t *testing.T) {
	details := map[string]any{"command": "'cflk' 'attach' '--' 'r1'"}
	a := &fakeAttacher{err: domain.NewError(domain.CodeInvalidRequest, "no terminal to open a window in", details)}
	res := call(t, connectWith(t, mcp.Execute, &fake{}, a, clock.Real{}), "runs_attach", map[string]any{"ref": "r1"})
	if !res.IsError {
		t.Fatalf("result = %+v, want a tool error", res)
	}
	got := structured(t, res)
	if got["code"] != string(domain.CodeInvalidRequest) {
		t.Fatalf("error = %v, want %s", got, domain.CodeInvalidRequest)
	}
	d, _ := got["details"].(map[string]any)
	if d["command"] == nil {
		t.Errorf("details = %v, want the command to hand to the person", d)
	}
}

func TestRunsAttachWithoutAnEnvelopeIsInternal(t *testing.T) {
	a := &fakeAttacher{err: errors.New("exit status 1: boom")}
	res := call(t, connectWith(t, mcp.Execute, &fake{}, a, clock.Real{}), "runs_attach", map[string]any{"ref": "r1"})
	if !res.IsError || structured(t, res)["code"] != string(domain.CodeInternal) {
		t.Errorf("result = %+v, want a CF_INTERNAL tool error", res)
	}
}

func TestRunsAttachWithoutAnAttacherIsNotImplemented(t *testing.T) {
	res := call(t, connect(t, mcp.Execute, &fake{}, clock.Real{}), "runs_attach", map[string]any{"ref": "r1"})
	if !res.IsError || structured(t, res)["code"] != string(domain.CodeNotImplemented) {
		t.Errorf("result = %+v, want CF_NOT_IMPLEMENTED", res)
	}
}
