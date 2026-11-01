package scheduler_test

import (
	"strings"
	"testing"

	v1 "github.com/Autarquico/codefabric/internal/api/v1"
	"github.com/Autarquico/codefabric/internal/app/scheduler"
	"github.com/Autarquico/codefabric/internal/domain"
)

func comp(name, status, message string) v1.ComponentHealth {
	return v1.ComponentHealth{Name: name, Status: status, Message: message}
}

func TestFilters_Health(t *testing.T) {
	claude := domain.HarnessCapability("claude")
	fail := v1.ComponentFail
	for _, c := range []struct {
		name       string
		state      domain.WorkerState
		components []v1.ComponentHealth
		harness    string
		eligible   bool
		reason     string // text the failure message holds
	}{
		{"online, nothing failing", domain.WorkerOnline, []v1.ComponentHealth{comp("disk", v1.ComponentOK, "")}, "claude", true, ""},
		{"online without components (old daemon)", domain.WorkerOnline, nil, "claude", true, ""},
		{"degraded without components (old daemon)", domain.WorkerDegraded, nil, "claude", false, "degraded"},
		{"degraded, only warnings", domain.WorkerDegraded, []v1.ComponentHealth{comp("git", v1.ComponentWarn, "old")}, "claude", false, "degraded"},
		{"global failure: disk", domain.WorkerDegraded, []v1.ComponentHealth{comp("disk", fail, "12% libre, umbral 20%")}, "claude", false, "disk: 12% libre, umbral 20%"},
		{"global failure: runtime", domain.WorkerDegraded, []v1.ComponentHealth{comp("runtime", fail, "no tmux")}, "claude", false, "runtime: no tmux"},
		{"global failure: version", domain.WorkerDegraded, []v1.ComponentHealth{comp("version", fail, "out of policy")}, "claude", false, "version: out of policy"},
		{"auth of the Run's harness", domain.WorkerDegraded, []v1.ComponentHealth{comp("auth:claude", fail, "no login")}, "claude", false, "auth:claude: no login"},
		{"harness of the Run's harness", domain.WorkerDegraded, []v1.ComponentHealth{comp("harness:claude", fail, "missing")}, "claude", false, "harness:claude: missing"},
		{"workflow of the Run's harness", domain.WorkerDegraded, []v1.ComponentHealth{comp("workflow:claude", fail, "drift")}, "claude", false, "workflow:claude: drift"},
		{"auth of another harness", domain.WorkerDegraded, []v1.ComponentHealth{comp("auth:codex", fail, "no login")}, "claude", true, ""},
		{"harness and workflow of another harness", domain.WorkerDegraded, []v1.ComponentHealth{comp("harness:codex", fail, "x"), comp("workflow:codex", fail, "y")}, "claude", true, ""},
		{"another harness and a global failure", domain.WorkerDegraded, []v1.ComponentHealth{comp("auth:codex", fail, "x"), comp("disk", fail, "full")}, "claude", false, "disk: full"},
		{"degraded for the daemon's own reason plus another harness", domain.WorkerDegraded, []v1.ComponentHealth{comp("auth:codex", fail, "x"), comp("daemon", fail, "executor unhealthy")}, "claude", false, "daemon: executor unhealthy"},
		{"a Run without a harness ignores harness failures", domain.WorkerDegraded, []v1.ComponentHealth{comp("auth:codex", fail, "x")}, "", true, ""},
		{"online with a failing component of its harness", domain.WorkerOnline, []v1.ComponentHealth{comp("auth:claude", fail, "no login")}, "claude", false, "auth:claude: no login"},
		{"draining stays out", domain.WorkerDraining, []v1.ComponentHealth{comp("auth:codex", fail, "x")}, "claude", false, "draining"},
	} {
		t.Run(c.name, func(t *testing.T) {
			w := worker("w", func(w *scheduler.Worker) {
				w.State, w.Components = c.state, c.components
				w.Capabilities = []string{domain.HarnessModesMarker, claude, domain.HarnessModeCapability("claude", domain.ModeHeadless)}
			})
			req := request(func(r *domain.Placement) { r.Harness, r.Mode = c.harness, domain.ModeHeadless })
			d := scheduler.Decide(req, []scheduler.Worker{w}, now)
			if c.eligible {
				if d.Chosen == nil {
					t.Fatalf("rejected: %+v", d.Verdicts[0].Failed)
				}
				return
			}
			if d.Chosen != nil {
				t.Fatal("chosen")
			}
			var messages []string
			for _, f := range d.Verdicts[0].Failed {
				messages = append(messages, f.Message)
			}
			if !strings.Contains(strings.Join(messages, "\n"), c.reason) {
				t.Errorf("failures %q lack %q", messages, c.reason)
			}
		})
	}
}

func TestFilters_HealthFailureNamesTheComponent(t *testing.T) {
	w := worker("w", func(w *scheduler.Worker) {
		w.State = domain.WorkerDegraded
		w.Components = []v1.ComponentHealth{{Name: "disk", Status: v1.ComponentFail, Message: "12% libre", Action: "libera espacio"}}
	})
	f := verdict(t, scheduler.Decide(request(), []scheduler.Worker{w}, now), "w").Failed
	if len(f) != 1 || f[0].Filter != scheduler.FilterHealth || f[0].Details["component"] != "disk" || f[0].Details["action"] != "libera espacio" {
		t.Errorf("failures = %+v", f)
	}
}
