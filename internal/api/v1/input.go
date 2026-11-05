package v1

import (
	"time"

	"github.com/Autarquico/codefabric/internal/domain"
)

// Limits of what asks for input and answers it.
const (
	MaxPromptIDLen  = 64
	MaxPromptOption = 16  // options of one prompt
	MaxOpenPrompts  = 16  // prompts of one Run left open under one lease epoch
	MaxOptionLen    = 128 // bytes of one option
	MaxAnswerText   = 8 << 10
)

// InputAnswer is what a person answers to a prompt: a permission is allowed or
// denied (Decision), a question or free text is answered with Text.
type InputAnswer struct {
	Decision string `json:"decision,omitempty"`
	Text     string `json:"text,omitempty"`
}

// InputCommand is the order to hand the answer of a prompt to the harness of a
// Run, under the lease epoch the prompt was asked under. Actor is who answered.
type InputCommand struct {
	RunID    string            `json:"run_id"`
	LeaseID  string            `json:"lease_id"`
	Epoch    uint64            `json:"epoch"`
	PromptID string            `json:"prompt_id"`
	Kind     domain.PromptKind `json:"kind"`
	Answer   InputAnswer       `json:"answer"`
	Actor    domain.ActorRef   `json:"actor"`
}

// RespondRequest is POST /v1/tasks/{ref}/respond. PromptID may be left out when
// the Task has exactly one prompt waiting for an answer.
type RespondRequest struct {
	RequestID string      `json:"request_id,omitempty"`
	PromptID  string      `json:"prompt_id,omitempty"`
	Answer    InputAnswer `json:"answer"`
}

// RespondResponse says which prompt of which Run the answer was queued for.
// The Run goes back to running when the Worker confirms it delivered it.
type RespondResponse struct {
	TaskID   string `json:"task_id"`
	RunID    string `json:"run_id"`
	RunShort string `json:"run_short_id"`
	PromptID string `json:"prompt_id"`
	// State is the prompt after the call: queued.
	State domain.PromptState `json:"state"`
}

// Kinds of item of the inbox.
const (
	InboxNeedsInput = "needs_input" // a Task waits for an answer
	InboxFailed     = "failed"      // a Task failed and its policy leaves the decision to a person
)

// InboxItem is something that needs a person. Prompt is set for needs_input;
// FailureCode and EndReason for failed.
type InboxItem struct {
	Kind        string           `json:"kind"`
	Task        InboxTask        `json:"task"`
	Run         InboxRun         `json:"run"`
	Prompt      *InboxPrompt     `json:"prompt,omitempty"`
	EndReason   domain.EndReason `json:"end_reason,omitempty"`
	FailureCode domain.Code      `json:"failure_code,omitempty"`
	Since       time.Time        `json:"since"`
}

// InboxTask names the Task of an item.
type InboxTask struct {
	ID      string `json:"id"`
	Short   string `json:"short_id"`
	Name    string `json:"name"`
	Project string `json:"project_id"`
}

// InboxRun names the Run of an item.
type InboxRun struct {
	ID      string          `json:"id"`
	Short   string          `json:"short_id"`
	State   domain.RunState `json:"state"`
	Harness string          `json:"harness,omitempty"`
	// Respondable is whether `cf task respond` can answer it now, that is,
	// whether its Worker announces structured input for its harness; if not,
	// attach to the Run.
	Respondable bool `json:"respondable"`
}

// InboxPrompt is what a Run asks.
type InboxPrompt struct {
	ID      string            `json:"id"`
	Kind    domain.PromptKind `json:"kind"`
	Summary string            `json:"summary"`
	Options []string          `json:"options,omitempty"`
}

// InboxResponse is GET /v1/inbox: the items that need a person, the oldest first.
type InboxResponse struct {
	Items []InboxItem `json:"items"`
}
