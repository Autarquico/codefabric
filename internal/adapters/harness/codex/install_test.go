package codex

import (
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

func TestInstallPlans(t *testing.T) {
	tests := []struct {
		name    string
		goos    string
		path    map[string]bool
		want    string
		wantErr bool
	}{
		{"npm present", "linux", map[string]bool{"npm": true}, "npm install -g @openai/codex", false},
		{"npm wins over brew", "darwin", map[string]bool{"npm": true, "brew": true}, "npm install -g @openai/codex", false},
		{"brew only on darwin", "darwin", map[string]bool{"brew": true}, "brew install codex", false},
		{"brew only on linux", "linux", map[string]bool{"brew": true}, "", true},
		{"none on darwin", "darwin", nil, "", true},
		{"none on linux", "linux", nil, "", true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			h := &fakeHost{path: tc.path}
			plan, err := Adapter{GOOS: tc.goos}.Install(context.Background(), h, harness.InstallOptions{DryRun: true})
			if len(h.ran) != 0 {
				t.Fatalf("DryRun ran %v", h.ran)
			}
			if tc.wantErr {
				if err == nil || !strings.Contains(err.Error(), "npm install -g") {
					t.Fatalf("err = %v, want manual instructions", err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if got := strings.Join(plan.Steps[0].Argv, " "); got != tc.want {
				t.Fatalf("argv = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestInstallRunsPlan(t *testing.T) {
	h := &fakeHost{path: map[string]bool{"npm": true}}
	if _, err := (Adapter{}).Install(context.Background(), h, harness.InstallOptions{}); err != nil {
		t.Fatal(err)
	}
	if len(h.ran) != 1 || h.ran[0][0] != "npm" {
		t.Fatalf("ran %v", h.ran)
	}
}

func TestInstallFailureSurfacesStderr(t *testing.T) {
	h := &fakeHost{
		path: map[string]bool{"npm": true},
		res:  harness.CmdResult{ExitCode: 1, Stderr: []byte("npm ERR! EACCES")},
	}
	_, err := Adapter{}.Install(context.Background(), h, harness.InstallOptions{})
	if err == nil || !strings.Contains(err.Error(), "EACCES") {
		t.Fatalf("err = %v", err)
	}
}

func TestInstallCapability(t *testing.T) {
	if !(Adapter{}).Capabilities().Install {
		t.Fatal("Install not declared")
	}
}
