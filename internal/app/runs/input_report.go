package runs

import (
	"context"
	"errors"
	"regexp"
	"strconv"

	v1 "github.com/Autarquico/codefabric/internal/api/v1"
	"github.com/Autarquico/codefabric/internal/app/input"
	"github.com/Autarquico/codefabric/internal/app/mutation"
	"github.com/Autarquico/codefabric/internal/controller/store"
	"github.com/Autarquico/codefabric/internal/domain"
)

var promptIDPattern = regexp.MustCompile(`^[A-Za-z0-9._-]{1,` + strconv.Itoa(v1.MaxPromptIDLen) + `}$`)

// inputPlan is what an input_requested or input_delivered does to the Controller.
// quiet: the entry says nothing new, so it leaves no event of its own. moves:
// the Run changed state, so the Task is derived again even when quiet.
// followUp is an event to write after the one of the report.
type inputPlan struct {
	apply    func() error
	quiet    bool
	moves    bool
	followUp *domain.Event
}

func noop() error { return nil }

// harnessOf is the harness a Run was frozen with.
func harnessOf(run store.Run) (string, error) {
	placement, err := PlacementOf(run.Spec)
	return placement.Harness, err
}

// requireStructuredInput is the gate of the report kinds of input: a Worker
// that does not announce domain.StructuredInputCapability for the harness of the
// Run never gets them applied, so an older or simpler Worker cannot move a Run
// into a state nobody can answer it in.
func requireStructuredInput(w store.Worker, run store.Run) error {
	harness, err := harnessOf(run)
	if err != nil {
		return err
	}
	if !domain.HasStructuredInput(w.Report.Capabilities, harness) {
		return input.Invalid("this Worker does not announce %s for the harness of the Run", domain.HarnessStructuredInputCapability(harness))
	}
	return nil
}

// inputRequested decides an input_requested: the Run goes from running to
// waiting_input with the prompt recorded. A prompt told again (a Worker that
// restarted and re-read it) changes nothing.
func inputRequested(ctx context.Context, tx *store.Tx, act mutation.Act, ev *domain.Event, task store.Task, run store.Run, e v1.RunReportEntry) (inputPlan, error) {
	if _, err := store.PromptByID(ctx, tx, run.ID, e.LeaseEpoch, e.PromptID); err == nil {
		return inputPlan{apply: noop, quiet: true}, nil
	} else if !errors.Is(err, store.ErrNotFound) {
		return inputPlan{}, err
	}
	if run.State != domain.RunWaitingInput {
		if _, err := domain.NextRunState(domain.RunStatus{State: run.State}, domain.RunEventInputRequested); err != nil {
			return inputPlan{}, err
		}
	}
	ev.Type, ev.Message = domain.EventRunWaitingInput, "run waiting for input"
	ev.Data["prompt_id"], ev.Data["prompt_kind"], ev.Data["summary"] = e.PromptID, string(e.PromptKind), e.Summary

	needs := act.Event(domain.EventTaskNeedsInput, "task needs input")
	needs.Timestamp = e.At
	needs.ProjectID, needs.TaskID, needs.RunID, needs.WorkerID = task.ProjectID, task.ID, run.ID, ev.WorkerID
	needs.Data = map[string]any{
		"prompt_id": e.PromptID, "prompt_kind": string(e.PromptKind), "summary": e.Summary,
		"lease_epoch": e.LeaseEpoch, "short_id": shortID(run.Short),
	}
	return inputPlan{
		apply: func() error {
			if _, err := tx.InsertPrompt(ctx, store.InputPrompt{
				RunID: run.ID, LeaseEpoch: e.LeaseEpoch, PromptID: e.PromptID, Kind: e.PromptKind,
				Summary: e.Summary, Options: e.Options, RequestedAt: e.At,
			}); err != nil {
				return err
			}
			if run.State == domain.RunWaitingInput {
				return nil
			}
			return tx.SetRunWaiting(ctx, run, domain.RunWaitingInput)
		},
		followUp: &needs,
	}, nil
}

// inputDelivered decides an input_delivered: the answer of the prompt reached
// the harness. The Run goes back to running once no prompt of it is left open.
// It leaves no event of its own: InputProvided was written by the respond, and
// the Task's return to running is its TaskStateChanged.
func inputDelivered(ctx context.Context, tx *store.Tx, run store.Run, e v1.RunReportEntry) (inputPlan, error) {
	p, err := store.PromptByID(ctx, tx, run.ID, e.LeaseEpoch, e.PromptID)
	if errors.Is(err, store.ErrNotFound) {
		return inputPlan{}, input.Invalid("prompt_id names no prompt of this Run under this lease")
	}
	if err != nil {
		return inputPlan{}, err
	}
	switch p.State {
	case domain.PromptAcknowledged:
		return inputPlan{apply: noop, quiet: true}, nil
	case domain.PromptPending:
		return inputPlan{}, domain.NewError(domain.CodeInvalidTransition, "no answer was queued for this prompt",
			map[string]any{"prompt_id": e.PromptID})
	}
	open, err := store.OpenPrompts(ctx, tx, run.ID, e.LeaseEpoch)
	if err != nil {
		return inputPlan{}, err
	}
	resumes := run.State == domain.RunWaitingInput && len(open) == 1
	if resumes {
		if _, err := domain.NextRunState(domain.RunStatus{State: run.State}, domain.RunEventInputProvided); err != nil {
			return inputPlan{}, err
		}
	}
	return inputPlan{
		apply: func() error {
			if err := tx.AcknowledgePrompt(ctx, run.ID, e.LeaseEpoch, e.PromptID, e.At); err != nil {
				return err
			}
			if !resumes {
				return nil
			}
			return tx.SetRunWaiting(ctx, run, domain.RunRunning)
		},
		quiet: true, moves: resumes,
	}, nil
}
