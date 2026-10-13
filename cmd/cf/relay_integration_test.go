//go:build integration && (linux || darwin)

package main

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	"github.com/Autarquico/codefabric/internal/adapters/runtime/tmux"
	v1 "github.com/Autarquico/codefabric/internal/api/v1"
	"github.com/Autarquico/codefabric/internal/client"
	"github.com/Autarquico/codefabric/internal/config"
	"github.com/Autarquico/codefabric/internal/controller/daemon"
	"github.com/Autarquico/codefabric/internal/domain"
	"github.com/Autarquico/codefabric/internal/support/gittest"
	"github.com/Autarquico/codefabric/internal/worker/execwrap"
)

// publicationOf reads the Worker's own record of a publication from state.db.
func publicationOf(t *testing.T, box config.Paths, run string) (state string, attempts int, found bool) {
	t.Helper()
	db, err := sql.Open("sqlite", "file:"+box.WorkerStateFile()+"?mode=ro")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	err = db.QueryRowContext(t.Context(), `SELECT state, attempts FROM publications WHERE run_id = ?`, run).Scan(&state, &attempts)
	if err != nil {
		return "", 0, false
	}
	return state, attempts, true
}

// TestScenario_GitRelayPublication_ControllerCaido: the Controller is down when
// the harness ends and while the Worker seals and tries to push. When it comes
// back the reports arrive, the push is made and the Run is published, once, with
// no attempt spent on the outage.
func TestScenario_GitRelayPublication_ControllerCaido(t *testing.T) {
	requireTmux(t)
	gittest.Require(t)
	host, _, _ := initController(t)
	stopController := serve(t, host, daemon.Options{})
	invite := workerInvite(t, host)
	box := machine(t)
	ok[map[string]any](t, "worker", "join", invite, "--name", "alpha")
	t.Cleanup(func() {
		matches, _ := filepath.Glob(filepath.Join(box.WorkerDataDir(), "runs", "*", execwrap.WrapperFile))
		for _, m := range matches {
			var w execwrap.Wrapper
			if found, _ := execwrap.Read(filepath.Dir(m), execwrap.WrapperFile, &w); found {
				_ = syscall.Kill(-w.PGID, syscall.SIGKILL)
			}
		}
		if rt, err := tmux.New(tmux.Options{Socket: tmux.SocketName(box.WorkerDataDir())}); err == nil {
			_ = rt.KillServer(context.WithoutCancel(t.Context()))
		}
	})
	admin, err := client.Open(host.CredentialsDir())
	if err != nil {
		t.Fatal(err)
	}
	worker := startDaemon(t, box)
	eventually(t, "the Worker to offer the harness", func() bool {
		w, err := admin.Worker(t.Context(), "alpha")
		return err == nil && w.State == domain.WorkerOnline && len(w.Capabilities) == 1
	})

	use(t, host)
	newOrigin(t).clone(t)
	ok[v1.Project](t, "project", "create", "backend")
	script := `touch ready
n=0
while [ ! -e go ]; do
  n=$((n+1)); [ "$n" -gt 1200 ] && exit 99
  sleep 0.05
done
echo work > result.txt
exit 0`
	created := ok[v1.CreateTaskResponse](t, "do", script, "--harness", "command")
	runID := created.Run.ID
	worktree := box.WorkerWorktreeDir(runID, 1)
	eventually(t, "the harness to be ready", func() bool { return fileExists(filepath.Join(worktree, "ready")) })
	eventually(t, "the Run to be running", func() bool {
		r, err := admin.Run(t.Context(), runID)
		return err == nil && r.State == domain.RunRunning
	})

	// The Controller goes down; the harness ends; the Worker seals and cannot push.
	stopController()
	if err := writeFile(filepath.Join(worktree, "go")); err != nil {
		t.Fatal(err)
	}
	eventually(t, "the Worker to seal", func() bool {
		state, _, found := publicationOf(t, box, runID)
		return found && state == "sealed"
	})
	// The publisher asked for a token and found nobody: that is logged once, and spends no attempt.
	eventually(t, "the Worker to try the push with the Controller down", func() bool {
		return strings.Contains(worker.out.String(), "asking for a token failed")
	})
	if state, attempts, _ := publicationOf(t, box, runID); state != "sealed" || attempts != 0 {
		t.Fatalf("publication = %s after %d attempts: an outage spends none", state, attempts)
	}

	serve(t, host, daemon.Options{})
	eventually(t, "the Run to be published", func() bool {
		r, err := admin.Run(t.Context(), runID)
		return err == nil && r.Publication == domain.PublicationPublished
	})
	run, err := admin.Run(t.Context(), runID)
	if err != nil {
		t.Fatal(err)
	}
	if run.State != domain.RunCompleted || run.ResultCommit == "" || run.Branch == "" || run.Runtime != domain.RuntimeStopped {
		t.Fatalf("Run = %+v", run)
	}
	for typ, want := range map[domain.EventType]int{domain.EventRunPublished: 1, domain.EventRunSealed: 1, domain.EventRunCompleted: 1} {
		if n := countEvents(t, host, runID, typ); n != want {
			t.Errorf("%d %s events, want %d", n, typ, want)
		}
	}
	repo := filepath.Join(host.RelayDir(), created.Task.ProjectID+".git")
	if tip := gittest.Run(t, repo, "rev-parse", "refs/heads/"+run.Branch); tip != run.ResultCommit {
		t.Errorf("the relay has %s on the branch, want %s", tip, run.ResultCommit)
	}
	if content := gittest.Run(t, repo, "show", run.ResultCommit+":result.txt"); content != "work" {
		t.Errorf("result.txt in the commit = %q", content)
	}
	eventually(t, "the Worker to confirm the publication", func() bool {
		state, _, _ := publicationOf(t, box, runID)
		return state == "published"
	})

	// The CLI shows it, and a published Run cannot be published again.
	code, out, errOut := cf(t, "", "run", "show", runID)
	if code != 0 || !strings.Contains(out, "Publication  published") || !strings.Contains(out, "Runtime      stopped") || !strings.Contains(out, "Lineage      initial") || !strings.Contains(out, run.Branch) {
		t.Errorf("run show: exit %d\n%s%s", code, out, errOut)
	}
	fails(t, 4, domain.CodeInvalidTransition, "run", "publish", runID)
}

func writeFile(path string) error { return os.WriteFile(path, nil, 0o600) }
