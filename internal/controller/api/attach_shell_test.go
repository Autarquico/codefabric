package api_test

import (
	"errors"
	"testing"

	v1 "github.com/Autarquico/codefabric/internal/api/v1"
	"github.com/Autarquico/codefabric/internal/domain"
)

// policyOf is the policy a CF_POLICY_DENIED names, or "" if it names none.
func policyOf(t *testing.T, err error) string {
	t.Helper()
	var de *domain.Error
	if !errors.As(err, &de) || de.Code != domain.CodePolicyDenied {
		t.Fatalf("error = %v, want CF_POLICY_DENIED", err)
	}
	p, _ := de.Details["policy"].(string)
	return p
}

// TestAttachOpen_ShellChecksInOrder: a shell needs a client, then the shell of
// the Project, then the shell of the Worker, and each refusal names its policy.
func TestAttachOpen_ShellChecksInOrder(t *testing.T) {
	f := newAttachFixture(t)
	ctx := t.Context()
	open := func(who string) error {
		c := f.owner
		if who == "worker" {
			c = f.alpha
		}
		conn, err := c.OpenAttach(ctx, f.run.ID, "", v1.AttachModeShell)
		if conn != nil {
			_ = conn.Close()
		}
		return err
	}

	// An identity that is not a client is refused first, whatever the policy says.
	if p := policyOf(t, open("worker")); p != "" {
		t.Errorf("a Worker was refused by policy %q, not for not being a client", p)
	}
	// The Project is asked before the Worker: neither key is on.
	if p := policyOf(t, open("client")); p != domain.PolicyProjectShell {
		t.Errorf("policy = %q, want %q", p, domain.PolicyProjectShell)
	}
	// The Project has it on; the Worker does not announce it.
	on := true
	_, err := f.owner.UpdateProject(ctx, "backend", v1.UpdateProjectRequest{Shell: &on})
	must(t, err)
	if p := policyOf(t, open("client")); p != domain.PolicyWorkerAllowShell {
		t.Errorf("policy = %q, want %q", p, domain.PolicyWorkerAllowShell)
	}
	// The Worker announces it, the Project turns it off again: the Project is still the first key.
	hb := relayHeartbeat(true)
	hb.Capabilities = append(hb.Capabilities, domain.AttachShellCapability)
	_, err = f.alpha.Heartbeat(ctx, hb)
	must(t, err)
	off := false
	_, err = f.owner.UpdateProject(ctx, "backend", v1.UpdateProjectRequest{Shell: &off})
	must(t, err)
	if p := policyOf(t, open("client")); p != domain.PolicyProjectShell {
		t.Errorf("policy = %q, want %q", p, domain.PolicyProjectShell)
	}
	// Both on: the grant binds the mode, the session opens and its audit says shell.
	_, err = f.owner.UpdateProject(ctx, "backend", v1.UpdateProjectRequest{Shell: &on})
	must(t, err)
	cc, wc, req := f.join(f.laptop, v1.AttachModeShell)
	if req.Mode != v1.AttachModeShell {
		t.Fatalf("request = %+v", req)
	}
	// A shell types: what the client sends reaches the Worker.
	must(t, cc.WriteData([]byte("ls\n")))
	if got := readData(t, wc, 3); string(got) != "ls\n" {
		t.Errorf("the Worker read %q", got)
	}
	must(t, cc.Detach())
	f.noSessions()
	closed := f.closedEvents(v1.AttachCloseClient)
	if closed[0].Data["mode"] != v1.AttachModeShell || closed[0].Data["duration_ms"] == nil {
		t.Errorf("close = %+v", closed[0].Data)
	}
}
