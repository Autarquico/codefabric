package cli

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"github.com/spf13/cobra"

	v1 "github.com/Autarquico/codefabric/internal/api/v1"
)

type fakeProjectUpdater struct{ got []v1.UpdateProjectRequest }

func (f *fakeProjectUpdater) UpdateProject(_ context.Context, _ string, req v1.UpdateProjectRequest) (v1.Project, error) {
	f.got = append(f.got, req)
	p := v1.Project{Name: "backend"}
	if req.OriginSecret != nil {
		p.OriginSecret = *req.OriginSecret
	}
	return p, nil
}

func runProjectSet(t *testing.T, f *fakeProjectUpdater, args ...string) string {
	t.Helper()
	opts := Options{projectUpdater: func(*cobra.Command) (projectUpdater, error) { return f, nil }}
	var out, errOut bytes.Buffer
	if code := RunContextWith(context.Background(), opts, append([]string{"project", "set", "backend"}, args...), strings.NewReader(""), &out, &errOut); code != 0 {
		t.Fatalf("exit %d: %s", code, errOut.String())
	}
	return out.String()
}

func TestProjectSet_OriginSecretSetsAndClears(t *testing.T) {
	f := &fakeProjectUpdater{}
	if out := runProjectSet(t, f, "--origin-secret", "origin/token"); !strings.Contains(out, "Origin auth  origin/token") {
		t.Errorf("output lacks the origin auth line:\n%s", out)
	}
	if out := runProjectSet(t, f, "--origin-secret", ""); !strings.Contains(out, "Origin auth  -") {
		t.Errorf("a cleared origin secret is not shown as -:\n%s", out)
	}
	if v := f.got[0].OriginSecret; v == nil || *v != "origin/token" {
		t.Errorf("set sent %v", v)
	}
	if v := f.got[1].OriginSecret; v == nil || *v != "" {
		t.Errorf("clear sent %v, want a pointer to the empty string", v)
	}
}

func TestProjectSet_WithoutTheFlagLeavesTheOriginSecretAlone(t *testing.T) {
	f := &fakeProjectUpdater{}
	runProjectSet(t, f, "--default-branch", "dev")
	if f.got[0].OriginSecret != nil {
		t.Errorf("sent origin_secret %q without the flag", *f.got[0].OriginSecret)
	}
}

func TestProjectSet_NativeStateOnOff(t *testing.T) {
	f := &fakeProjectUpdater{}
	runProjectSet(t, f, "--native-state", "on")
	runProjectSet(t, f, "--native-state", "off")
	runProjectSet(t, f, "--default-branch", "dev")
	if v := f.got[0].NativeState; v == nil || !*v {
		t.Errorf("on sent %v", v)
	}
	if v := f.got[1].NativeState; v == nil || *v {
		t.Errorf("off sent %v", v)
	}
	if f.got[2].NativeState != nil {
		t.Errorf("sent native_state without the flag")
	}
	opts := Options{projectUpdater: func(*cobra.Command) (projectUpdater, error) { return f, nil }}
	var out, errOut bytes.Buffer
	args := []string{"project", "set", "backend", "--native-state", "maybe"}
	if code := RunContextWith(context.Background(), opts, args, strings.NewReader(""), &out, &errOut); code == 0 {
		t.Error("a value other than on or off was accepted")
	}
}
