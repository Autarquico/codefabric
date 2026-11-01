package claude

import (
	"bytes"
	"context"
	"os/exec"
	"strings"
	"testing"

	"github.com/Autarquico/codefabric/internal/adapters/harness"
)

type fakeHost struct {
	harness.Host
	path map[string]bool
	res  harness.CmdResult
	ran  [][]string
}

func (h *fakeHost) LookPath(f string) (string, error) {
	if h.path[f] {
		return "/bin/" + f, nil
	}
	return "", exec.ErrNotFound
}

func (h *fakeHost) Run(_ context.Context, c harness.Cmd) (harness.CmdResult, error) {
	h.ran = append(h.ran, c.Argv)
	return h.res, nil
}

func TestInstallPlan(t *testing.T) {
	h := &fakeHost{path: map[string]bool{"curl": true, "bash": true}}
	plan, err := Adapter{}.Install(context.Background(), h, harness.InstallOptions{DryRun: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Steps) != 1 || strings.Join(plan.Steps[0].Argv, "|") != "bash|-c|"+InstallScript {
		t.Fatalf("plan = %+v", plan)
	}
	if len(h.ran) != 0 {
		t.Fatalf("DryRun ran %v", h.ran)
	}
}

func TestInstallNeedsCurl(t *testing.T) {
	h := &fakeHost{path: map[string]bool{"bash": true}}
	_, err := Adapter{}.Install(context.Background(), h, harness.InstallOptions{DryRun: true})
	if err == nil || !strings.Contains(err.Error(), "curl") {
		t.Fatalf("err = %v", err)
	}
}

func TestInstallRuns(t *testing.T) {
	h := &fakeHost{path: map[string]bool{"curl": true, "bash": true}, res: harness.CmdResult{Stdout: []byte("ok\n")}}
	var out bytes.Buffer
	if _, err := (Adapter{}).Install(context.Background(), h, harness.InstallOptions{Out: &out}); err != nil {
		t.Fatal(err)
	}
	if len(h.ran) != 1 || out.String() != "ok\n" {
		t.Fatalf("ran %v out %q", h.ran, out.String())
	}
}

func TestInstallFailureSurfacesStderr(t *testing.T) {
	h := &fakeHost{
		path: map[string]bool{"curl": true, "bash": true},
		res:  harness.CmdResult{ExitCode: 22, Stderr: []byte("curl: (22) 404")},
	}
	_, err := Adapter{}.Install(context.Background(), h, harness.InstallOptions{})
	if err == nil || !strings.Contains(err.Error(), "curl: (22) 404") || !strings.Contains(err.Error(), "22") {
		t.Fatalf("err = %v", err)
	}
}

func TestInstallCapability(t *testing.T) {
	if !(Adapter{}).Capabilities().Install {
		t.Fatal("Install not declared")
	}
}
