package domain

import "slices"

// PromptKind is what a Run that waits for input asks (§11.6).
type PromptKind string

// Prompt kinds.
const (
	PromptPermission PromptKind = "permission" // allow or deny a tool use
	PromptQuestion   PromptKind = "question"   // a question, with options or not
	PromptText       PromptKind = "text"       // free text
)

// IsValid reports whether k is a prompt kind.
func (k PromptKind) IsValid() bool {
	return slices.Contains([]PromptKind{PromptPermission, PromptQuestion, PromptText}, k)
}

// PromptState is where a prompt of a Run is on its way to being answered.
type PromptState string

// Prompt states. Delivered is reserved for a Worker that confirms it holds the
// order before it acts on it; nothing writes it yet.
const (
	PromptPending      PromptState = "pending"      // asked, nobody answered
	PromptQueued       PromptState = "queued"       // answered; the input order is queued
	PromptDelivered    PromptState = "delivered"    // the Worker holds the order
	PromptAcknowledged PromptState = "acknowledged" // the Worker says the answer reached the harness
)

// Decisions of the answer to a permission prompt.
const (
	AnswerAllow = "allow"
	AnswerDeny  = "deny"
)
