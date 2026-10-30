//go:build integration && (linux || darwin)

package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	v1 "github.com/Autarquico/codefabric/internal/api/v1"
	"github.com/Autarquico/codefabric/internal/client"
	"github.com/Autarquico/codefabric/internal/config"
	"github.com/Autarquico/codefabric/internal/domain"
)

const environmentSecret = "s3cr3t-0f-the-environment-7f3a"

// environmentWorld is a repoConfigWorld whose Project defaults to the
// Controller Environment "node", applied from def, and whose Worker holds the
// secret "env/token" (allowed to the Project).
type environmentWorld struct {
	repoConfigWorld
	host config.Paths
}

func newEnvironmentWorld(t *testing.T, def map[string]any) environmentWorld {
	t.Helper()
	w := environmentWorld{repoConfigWorld: newRepoConfigWorld(t, map[string]string{"README.md": "hello\n"})}
	w.host = config.Paths{ConfigDir: os.Getenv("CF_CONFIG_DIR"), DataDir: os.Getenv("CF_DATA_DIR")}

	use(t, w.box)
	if code, _, errOut := setSecret(t, environmentSecret+"\n", "env/token"); code != 0 {
		t.Fatalf("setting the secret: exit %d: %s", code, errOut)
	}
	use(t, w.host)
	eventually(t, "the Worker to offer environment-host:v1 and the secret", func() bool {
		worker, err := w.admin.Worker(t.Context(), "alpha")
		if err != nil || !slices.Contains(worker.Capabilities, domain.EnvironmentHostCapability) {
			return false
		}
		return slices.ContainsFunc(worker.Secrets, func(m v1.SecretMeta) bool { return m.Ref == "env/token" })
	})

	def["name"], def["provider"] = "node", "host"
	raw, err := json.Marshal(def)
	if err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(t.TempDir(), "node.json")
	if err := os.WriteFile(file, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	ok[v1.EnvironmentVersion](t, "environment", "apply", "-f", file)
	ok[v1.Project](t, "project", "secrets", "allow", "backend", "env/token")
	ok[v1.Project](t, "project", "set", "backend", "--environment", "node")
	return w
}

func (w environmentWorld) settled(t *testing.T) {
	t.Helper()
	eventually(t, "the Worker to hold no reservation", func() bool {
		worker, err := w.admin.Worker(t.Context(), "alpha")
		return err == nil && worker.Reservations == 0 && worker.ActiveRuns == 0
	})
}

func (w environmentWorld) events(t *testing.T, runID string, typ domain.EventType) []v1.Event {
	t.Helper()
	page, err := w.admin.Events(t.Context(), client.EventQuery{Run: runID, Types: []string{string(typ)}, Limit: 100})
	if err != nil {
		t.Fatal(err)
	}
	return page.Events
}

// Prepare fallido: the prepare of the Environment exits non-zero, printing a
// secret of the Run. The Run is failed(environment) with publication none, the
// harness never ran, the Worker confirms RunStopped(never_started), the
// reservation is released (the Worker takes another Run), and the failure
// reaches the Controller with the secret redacted.
func TestScenario_RepoConfig_PrepareFallido(t *testing.T) {
	w := newEnvironmentWorld(t, map[string]any{
		"prepare": `echo "installing with $TOKEN"; echo "npm ERR! build failed" >&2; exit 3`,
	})
	ran := filepath.Join(t.TempDir(), "harness-ran")
	created := ok[v1.CreateTaskResponse](t, "do", "touch "+ran, "--harness", "command", "--secret-env", "TOKEN=env/token")
	runID := created.Run.ID

	eventually(t, "the Run to fail", func() bool {
		r, err := w.admin.Run(t.Context(), runID)
		return err == nil && r.State == domain.RunFailed
	})
	eventually(t, "RunStopped", func() bool { return len(w.events(t, runID, domain.EventRunStopped)) == 1 })
	w.settled(t)

	run := ok[v1.Run](t, "run", "show", created.Run.Short)
	if run.EndReason != domain.EndEnvironment || run.Publication != domain.PublicationNone || run.Environment == nil || run.Environment.Name != "node" {
		t.Errorf("Run = state %s, end %q, publication %q, environment %+v", run.State, run.EndReason, run.Publication, run.Environment)
	}
	if stopped := w.events(t, runID, domain.EventRunStopped); stopped[0].Data["never_started"] != true {
		t.Errorf("RunStopped data = %v, want never_started", stopped[0].Data)
	}
	if fileExists(ran) {
		t.Error("the harness ran after a failed prepare")
	}

	// The failure and what the prepare printed reach the Controller; the secret does not.
	failed := w.events(t, runID, domain.EventRunFailed)
	if len(failed) != 1 {
		t.Fatalf("%d RunFailed events", len(failed))
	}
	message, _ := failed[0].Data["message"].(string)
	if !strings.Contains(message, "build failed") {
		t.Errorf("RunFailed message = %q: the failure does not say why", message)
	}
	_, shown, _ := cf(t, "", "events", "--run", runID, "--json")
	_, humanShow, _ := cf(t, "", "run", "show", created.Run.Short)
	for what, text := range map[string]string{"cf events": shown, "cf run show": humanShow, "RunFailed message": message} {
		if strings.Contains(text, environmentSecret) {
			t.Errorf("the secret is in %s:\n%s", what, text)
		}
	}
	if !strings.Contains(message, "[REDACTED:env/token]") {
		t.Errorf("RunFailed message = %q: the secret was printed and should be redacted", message)
	}

	// The reservation is free: with no Environment the same Worker takes another Run to the end.
	ok[v1.Project](t, "project", "set", "backend", "--environment", "")
	next := ok[v1.CreateTaskResponse](t, "do", "exit 0", "--harness", "command")
	eventually(t, "the next Run to complete", func() bool {
		r, err := w.admin.Run(t.Context(), next.Run.ID)
		return err == nil && r.State == domain.RunCompleted
	})
}

// A detached service (start exits 0) whose stop fails: the Run completes, the
// Controller records an EnvironmentStopFailed warning, and the reservation is
// released all the same.
func TestScenario_RepoConfig_ServicioDesacopladoConStopFallido(t *testing.T) {
	w := newEnvironmentWorld(t, map[string]any{
		"prepare": "true",
		"services": []map[string]string{
			{"name": "daemonized", "start": "exit 0", "ready": "true", "stop": "echo 'stop refused' >&2; exit 4"},
		},
	})
	created := ok[v1.CreateTaskResponse](t, "do", "exit 0", "--harness", "command")
	runID := created.Run.ID

	eventually(t, "the Run to complete", func() bool {
		r, err := w.admin.Run(t.Context(), runID)
		return err == nil && r.State == domain.RunCompleted
	})
	eventually(t, "the warning", func() bool { return len(w.events(t, runID, domain.EventEnvironmentStopFailed)) == 1 })
	warn := w.events(t, runID, domain.EventEnvironmentStopFailed)[0]
	if failures := fmt.Sprint(warn.Data["failures"]); warn.Level != domain.LevelWarn || !strings.Contains(failures, "daemonized") || !strings.Contains(failures, "stop refused") {
		t.Errorf("warning = level %s, data %v", warn.Level, warn.Data)
	}
	if n := len(w.events(t, runID, domain.EventRunStopped)); n != 1 {
		t.Errorf("%d RunStopped events, want 1", n)
	}
	w.settled(t)

	next := ok[v1.CreateTaskResponse](t, "do", "exit 0", "--harness", "command")
	eventually(t, "the next Run to complete", func() bool {
		r, err := w.admin.Run(t.Context(), next.Run.ID)
		return err == nil && r.State == domain.RunCompleted
	})
}
