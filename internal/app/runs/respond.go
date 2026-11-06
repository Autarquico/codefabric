package runs

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"

	v1 "github.com/Autarquico/codefabric/internal/api/v1"
	"github.com/Autarquico/codefabric/internal/app/input"
	"github.com/Autarquico/codefabric/internal/app/mutation"
	"github.com/Autarquico/codefabric/internal/app/refs"
	"github.com/Autarquico/codefabric/internal/controller/pki"
	"github.com/Autarquico/codefabric/internal/controller/store"
	"github.com/Autarquico/codefabric/internal/domain"
)

// Respond answers the prompt a Task's Run waits on (§11.6): in one transaction
// it finds the Run in waiting_input and the pending prompt, revalidates the
// lease and that the Worker can be answered for the harness, claims the prompt
// so that no competing answer wins too, queues the `input` order and writes
// InputProvided with the actor. The Run goes back to running when the Worker
// reports input_delivered, not here. Repeating a request_id delivers once.
//
// A Task whose Run cannot be answered (not waiting, ended, or a Worker without
// structured input for its harness) is CF_RUN_NOT_RESPONDABLE with the way to
// attach in the details.
func (s *Service) Respond(ctx context.Context, by pki.Session, ref, project string, req v1.RespondRequest) (v1.RespondResponse, error) {
	return mutation.Do(ctx, s.db, s.clock, by, "task.respond", req.RequestID,
		struct {
			Ref, Project string
			Request      v1.RespondRequest
		}{ref, project, req},
		func(tx *store.Tx, act mutation.Act) (v1.RespondResponse, error) {
			scope, err := refs.Scope(ctx, tx, project)
			if err != nil {
				return v1.RespondResponse{}, err
			}
			taskID, err := refs.Task(ctx, tx, ref, scope)
			if err != nil {
				return v1.RespondResponse{}, err
			}
			task, err := store.TaskByID(ctx, tx, taskID)
			if err != nil {
				return v1.RespondResponse{}, err
			}
			run, prompt, err := promptToAnswer(ctx, tx, task, req.Run, req.PromptID)
			if err != nil {
				return v1.RespondResponse{}, err
			}
			if err := validateAnswer(prompt.Kind, req.Answer); err != nil {
				return v1.RespondResponse{}, err
			}
			l, err := store.LeaseByRunEpoch(ctx, tx, run.ID, run.LeaseEpoch)
			if err != nil {
				return v1.RespondResponse{}, err
			}
			if l.State != domain.LeaseAccepted {
				return v1.RespondResponse{}, notRespondable("the lease of the Run is no longer live", "stale_lease", &run)
			}
			answer, err := json.Marshal(req.Answer)
			if err != nil {
				return v1.RespondResponse{}, err
			}
			if err := tx.ClaimPrompt(ctx, run.ID, prompt.LeaseEpoch, prompt.PromptID, answer, act.Actor, act.Now); err != nil {
				if errors.Is(err, store.ErrNoRowChanged) {
					return v1.RespondResponse{}, answeredAlready(prompt)
				}
				return v1.RespondResponse{}, err
			}
			if err := orderInputIn(ctx, tx, act, run, l, prompt, req.Answer); err != nil {
				return v1.RespondResponse{}, err
			}
			ev := act.Event(domain.EventInputProvided, "input provided")
			ev.ProjectID, ev.TaskID, ev.RunID, ev.WorkerID = task.ProjectID, task.ID, run.ID, run.WorkerID
			ev.Data = map[string]any{
				"prompt_id": prompt.PromptID, "prompt_kind": string(prompt.Kind), "lease_epoch": prompt.LeaseEpoch,
				"short_id": shortID(run.Short),
			}
			if req.Answer.Decision != "" {
				ev.Data["decision"] = req.Answer.Decision
			}
			if _, _, err := tx.AppendEvent(ctx, ev); err != nil {
				return v1.RespondResponse{}, err
			}
			return v1.RespondResponse{
				TaskID: string(task.ID), RunID: string(run.ID), RunShort: shortID(run.Short),
				PromptID: prompt.PromptID, State: domain.PromptQueued,
			}, nil
		})
}

// orderInputIn queues, in tx, the order that the Worker of the lease hand the
// answer to the harness of the Run. It is the only place that queues one, and it
// refuses unless the Worker announces structured input for the harness: an
// order the Worker does not know would block its queue.
func orderInputIn(ctx context.Context, tx *store.Tx, act mutation.Act, run store.Run, l store.Lease, prompt store.InputPrompt, answer v1.InputAnswer) error {
	worker, err := store.WorkerByID(ctx, tx, l.WorkerID)
	if err != nil {
		return err
	}
	harness, err := harnessOf(run)
	if err != nil {
		return err
	}
	if !domain.HasStructuredInput(worker.Report.Capabilities, harness) {
		return notRespondable(fmt.Sprintf("the Worker cannot answer the %q harness while it runs", harness), "no_structured_input", &run)
	}
	payload, err := orderJSON(v1.InputCommand{
		RunID: string(run.ID), LeaseID: string(l.ID), Epoch: l.Epoch, PromptID: prompt.PromptID,
		Kind: prompt.Kind, Answer: answer, Actor: act.Actor,
	})
	if err != nil {
		return err
	}
	_, err = tx.EnqueueCommand(ctx, store.WorkerCommand{
		WorkerID: l.WorkerID, Type: domain.CommandInput, RunID: run.ID, LeaseID: l.ID, Epoch: l.Epoch,
		Payload: payload, CreatedAt: act.Now,
	})
	return err
}

// promptToAnswer finds the Run of the Task that waits for input and the prompt
// the answer is for. A prompt_id is only unique within a Run, so when more than
// one Run has it open the answer must name the Run; without a prompt_id the Task
// (or the named Run) must have exactly one pending.
func promptToAnswer(ctx context.Context, tx *store.Tx, task store.Task, runRef, promptID string) (store.Run, store.InputPrompt, error) {
	all, err := store.Runs(ctx, tx, store.RunFilter{TaskID: task.ID})
	if err != nil {
		return store.Run{}, store.InputPrompt{}, err
	}
	var (
		waiting []store.Run
		live    *store.Run
	)
	for i, r := range all {
		switch {
		case r.State == domain.RunWaitingInput:
			waiting = append(waiting, r)
			live = &all[i]
		case !r.State.IsTerminal():
			live = &all[i]
		}
	}
	if len(waiting) == 0 {
		return store.Run{}, store.InputPrompt{}, notRespondable(
			fmt.Sprintf("task %s is not waiting for input", task.Name), "not_waiting", live)
	}
	if runRef != "" {
		id, err := refs.Run(ctx, tx, runRef)
		if err != nil {
			return store.Run{}, store.InputPrompt{}, err
		}
		i := slices.IndexFunc(waiting, func(r store.Run) bool { return r.ID == id })
		if i < 0 {
			return store.Run{}, store.InputPrompt{}, notRespondable(
				fmt.Sprintf("run %s of task %s is not waiting for input", runRef, task.Name), "not_waiting", live)
		}
		waiting = []store.Run{waiting[i]}
	}
	var open []store.InputPrompt
	runOf := map[string]store.Run{}
	for _, r := range waiting {
		prompts, err := store.OpenPrompts(ctx, tx, r.ID, r.LeaseEpoch)
		if err != nil {
			return store.Run{}, store.InputPrompt{}, err
		}
		for _, p := range prompts {
			open = append(open, p)
			runOf[string(p.RunID)+"/"+p.PromptID] = r
		}
	}
	if promptID != "" {
		var matches []store.InputPrompt
		for _, p := range open {
			if p.PromptID == promptID {
				matches = append(matches, p)
			}
		}
		switch len(matches) {
		case 0:
			return store.Run{}, store.InputPrompt{}, domain.NewError(domain.CodeNotFound, "the Task has no open prompt "+promptID, nil)
		case 1:
			return runOf[string(matches[0].RunID)+"/"+promptID], matches[0], nil
		}
		shorts := make([]string, len(matches))
		for i, p := range matches {
			shorts[i] = shortID(runOf[string(p.RunID)+"/"+promptID].Short)
		}
		return store.Run{}, store.InputPrompt{}, domain.NewError(domain.CodeInvalidRequest,
			fmt.Sprintf("prompt %s is open in more than one Run (%s): name the Run with --run", promptID, strings.Join(shorts, ", ")),
			map[string]any{"prompt_id": promptID, "runs": shorts})
	}
	pending := slices.DeleteFunc(slices.Clone(open), func(p store.InputPrompt) bool { return p.State != domain.PromptPending })
	switch len(pending) {
	case 0:
		return store.Run{}, store.InputPrompt{}, answeredAlready(open[0])
	case 1:
		return runOf[string(pending[0].RunID)+"/"+pending[0].PromptID], pending[0], nil
	}
	ids := make([]string, len(pending))
	for i, p := range pending {
		ids[i] = p.PromptID
	}
	return store.Run{}, store.InputPrompt{}, domain.NewError(domain.CodeInvalidRequest,
		"the Task has more than one prompt waiting: name it with --prompt", map[string]any{"prompts": ids})
}

// answeredAlready is the failure of the answer that lost the claim of a prompt
// to another one.
func answeredAlready(p store.InputPrompt) error {
	return domain.NewError(domain.CodeInvalidTransition, "the prompt was answered already",
		map[string]any{"prompt_id": p.PromptID, "state": string(p.State)})
}

// notRespondable is CF_RUN_NOT_RESPONDABLE. When there is a live Run the details
// say how to act on it directly.
func notRespondable(message, reason string, run *store.Run) error {
	details := map[string]any{"reason": reason}
	if run != nil {
		details["run"] = shortID(run.Short)
		details["attach"] = "cf attach " + shortID(run.Short)
	}
	return domain.NewError(domain.CodeRunNotRespondable, message, details)
}

// validateAnswer checks the answer fits the kind of the prompt: a permission is
// allowed or denied, a question or free text is answered with text.
func validateAnswer(kind domain.PromptKind, a v1.InputAnswer) error {
	if len(a.Text) > v1.MaxAnswerText {
		return input.Invalid("the answer text is at most %d bytes", v1.MaxAnswerText)
	}
	switch kind {
	case domain.PromptPermission:
		if a.Decision != domain.AnswerAllow && a.Decision != domain.AnswerDeny {
			return input.Invalid("a permission is answered with --allow or --deny")
		}
		if a.Text != "" {
			return input.Invalid("a permission is answered with --allow or --deny, not with text")
		}
	default:
		if a.Decision != "" || a.Text == "" {
			return input.Invalid("a %s is answered with --text", kind)
		}
	}
	return nil
}
