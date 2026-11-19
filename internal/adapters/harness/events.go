package harness

import "time"

// Event is one thing a Run said in its output (§7.2). Only the field of its
// Kind is set.
type Event struct {
	Kind EventKind
	// At is when it happened, by the clock of the harness; zero when the
	// source does not say.
	At        time.Time
	SessionID string
	Model     string
	// Usage is set on EventUsage.
	Usage *UsageEvent
	// Result is set on EventResult.
	Result *Result
	// Message is set on EventError, EventInputRequired and EventAgentMessage.
	Message string
	// ID names what the harness asks on EventInputRequired when it has a name
	// for it (the tool use of a question): the same ask read twice has the same
	// ID. Options are the answers it offers, if any.
	ID      string
	Options []string
}

// EventKind is the kind of an Event.
type EventKind string

// The kinds of event.
const (
	EventSessionStarted EventKind = "session_started"
	EventUsage          EventKind = "usage"
	EventInputRequired  EventKind = "input_required"
	EventResult         EventKind = "result"
	// EventAgentMessage is a message of the agent that may turn out to be the
	// answer of the turn: a harness whose result line carries no text (Codex)
	// says it here, so that a reader that is given the stream in pieces can
	// pair it with a result that arrives in a later piece.
	EventAgentMessage EventKind = "agent_message"
	EventError        EventKind = "error"
	// EventTurnActivity says that the agent is working on a turn (a message, a
	// tool use or its result, a partial message). It carries nothing: whoever
	// watches for the end of a turn learns from it that a result before it is
	// not the last word.
	EventTurnActivity EventKind = "turn_activity"
)

// UsageEvent is the usage of one model request (§14.4). A nil field is
// unknown, which is not the same as 0: the harness did not report it, and it
// is never filled with a guess. Cost is not here: it is derived where
// there is pricing (§14.6).
type UsageEvent struct {
	At               time.Time
	Provider         string
	Model            string
	InputTokens      *int64
	OutputTokens     *int64
	CacheReadTokens  *int64
	CacheWriteTokens *int64
	RequestCount     *int64
	LatencyMS        *int64
}

// Result is how the harness reports the end of its work.
type Result struct {
	// Failed is the harness saying its work failed; the exit code of the
	// process is still what ends the Run.
	Failed  bool
	Summary string
}

// Checkpoint is native state a harness exports to resume from (§7.2).
type Checkpoint struct {
	// Harness is the ID of the adapter that made it.
	Harness string
	// Format names the layout of Data, owned by the adapter.
	Format string
	Data   []byte
}
