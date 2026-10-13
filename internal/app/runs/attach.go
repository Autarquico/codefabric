package runs

import (
	"context"
	"encoding/json"
	"errors"

	v1 "github.com/Autarquico/codefabric/internal/api/v1"
	"github.com/Autarquico/codefabric/internal/app/refs"
	"github.com/Autarquico/codefabric/internal/controller/pki"
	"github.com/Autarquico/codefabric/internal/controller/store"
	"github.com/Autarquico/codefabric/internal/domain"
)

// reasonWorkerOffline is the reason `cf ps` gives for a Run on a Worker that
// is offline or removed; `cf attach` answers it with CF_WORKER_OFFLINE.
const reasonWorkerOffline = "worker_offline"

// attachCheck is why a Run cannot be attached, or that it can.
type attachCheck struct {
	ok     bool
	reason string
	code   domain.Code
	handle v1.RuntimeHandle
	target string
	binary string
}

// attachable is the one decision `cf attach` and `cf ps` share. In order: a
// headless Run has no terminal; a Run that is over, or whose session is not
// active or whose handle is not known, is not running; a Worker that is offline
// or removed cannot be reached; and the Worker must have announced a valid
// ssh target and a valid binary for the runtime of the handle. A Worker that
// is draining or degraded can be attached to: it is reachable.
func attachable(r store.Run, w *store.Worker) attachCheck {
	no := func(code domain.Code, reason string) attachCheck { return attachCheck{code: code, reason: reason} }
	if specMode(r.Spec) == domain.ModeHeadless {
		return no(domain.CodeRunNotAttachable, v1.NotAttachableHeadless)
	}
	var handle v1.RuntimeHandle
	if r.State.IsTerminal() || r.Runtime != domain.RuntimeActive || r.RuntimeHandle == "" ||
		json.Unmarshal([]byte(r.RuntimeHandle), &handle) != nil || w == nil {
		return no(domain.CodeRunNotAttachable, v1.NotAttachableNotRunning)
	}
	if !w.State.Reachable() {
		return no(domain.CodeWorkerOffline, reasonWorkerOffline)
	}
	if !domain.ValidSSHTarget(w.Report.SSHTarget) {
		return no(domain.CodeRunNotAttachable, v1.NotAttachableNoSSHTarget)
	}
	binary := w.Report.AttachBinaries[handle.Runtime]
	if !domain.ValidAttachRuntime(handle.Runtime) || !domain.ValidAttachBinary(binary) {
		return no(domain.CodeRunNotAttachable, v1.NotAttachableRuntimeUnsupported)
	}
	return attachCheck{ok: true, handle: handle, target: w.Report.SSHTarget, binary: binary}
}

func (c attachCheck) err() error {
	return domain.NewError(c.code, "this Run cannot be attached: "+c.reason, map[string]any{"reason": c.reason})
}

// AttachTarget answers GET /v1/runs/{ref}/attach: where and how to open the
// session of a Run. A reference that reads as a Run (an ID or a short ID) is
// that Run; any other is a Task, and the Run is its latest one that is not over.
func (s *Service) AttachTarget(ctx context.Context, by pki.Session, ref, project string) (v1.AttachTarget, error) {
	var out v1.AttachTarget
	err := s.db.Read(ctx, func(q store.Querier) error {
		run, err := s.attachRun(ctx, q, by, ref, project)
		if err != nil {
			return err
		}
		var worker *store.Worker
		if run.WorkerID != "" {
			w, err := store.WorkerByID(ctx, q, run.WorkerID)
			if err != nil {
				return err
			}
			worker = &w
		}
		check := attachable(run, worker)
		if !check.ok {
			return check.err()
		}
		out = v1.AttachTarget{
			RunID: string(run.ID), RunShort: shortID(run.Short), LeaseEpoch: run.LeaseEpoch,
			Worker:    v1.WorkerRef{ID: string(worker.ID), Name: worker.Name},
			SSHTarget: check.target, Handle: check.handle, Binary: check.binary,
		}
		return nil
	})
	return out, err
}

func (s *Service) attachRun(ctx context.Context, q store.Querier, by pki.Session, ref, project string) (store.Run, error) {
	if readsAsRun(ref) {
		return s.runOf(ctx, q, by, ref)
	}
	if _, err := pki.RequireCaller(ctx, q, by, s.clock.Now()); err != nil {
		return store.Run{}, err
	}
	scope, err := refs.Scope(ctx, q, project)
	if err != nil {
		return store.Run{}, err
	}
	task, err := refs.Task(ctx, q, ref, scope)
	if err != nil {
		return store.Run{}, err
	}
	run, err := store.LiveRunOfTask(ctx, q, task)
	if errors.Is(err, store.ErrNotFound) {
		return store.Run{}, attachCheck{code: domain.CodeRunNotAttachable, reason: v1.NotAttachableNotRunning}.err()
	}
	return run, err
}

// readsAsRun: names of Tasks never look like IDs (§3.13), so the form decides.
func readsAsRun(ref string) bool {
	if kind, _, err := domain.ParseID(ref); err == nil {
		return kind == domain.KindRun
	}
	short, err := domain.ParseShortID(ref)
	return err == nil && short.Kind == domain.KindRun
}

// Ps answers GET /v1/ps: the Runs that have not ended, and those that ended with
// a session still active, each with its Task, its Worker and whether `cf attach`
// would open it, those of one Project if it is named. Everything is read in one snapshot.
func (s *Service) Ps(ctx context.Context, by pki.Session, project string) (v1.PsResponse, error) {
	out := v1.PsResponse{Runs: []v1.PsEntry{}}
	err := s.db.Read(ctx, func(q store.Querier) error {
		if _, err := pki.RequireCaller(ctx, q, by, s.clock.Now()); err != nil {
			return err
		}
		scope, err := refs.Scope(ctx, q, project)
		if err != nil {
			return err
		}
		rows, tasks, err := store.PsRuns(ctx, q)
		if err != nil {
			return err
		}
		all, err := store.Workers(ctx, q, true)
		if err != nil {
			return err
		}
		workers := make(map[domain.ID]store.Worker, len(all))
		for _, w := range all {
			workers[w.ID] = w
		}
		for _, r := range rows {
			entry := v1.PsEntry{Run: view(r, false)}
			t := tasks[r.TaskID]
			if scope != "" && t.Project != scope {
				continue
			}
			entry.Task = v1.PsTask{ID: string(t.ID), Short: domain.ShortID{Kind: domain.KindTask, N: t.Short}.String(), Name: t.Name}
			var worker *store.Worker
			if w, ok := workers[r.WorkerID]; ok && r.WorkerID != "" {
				worker = &w
				reachable := w.State.Reachable()
				entry.WorkerReachable = &reachable
				entry.Worker = &v1.PsWorker{ID: string(w.ID), Name: w.Name, State: w.State}
			}
			if check := attachable(r, worker); check.ok {
				entry.Attachable = true
			} else {
				entry.NotAttachable = check.reason
			}
			out.Runs = append(out.Runs, entry)
		}
		return nil
	})
	return out, err
}
