package checkpoints

import (
	"context"
	"fmt"
	"math"
	"strings"
	"unicode/utf8"

	"github.com/Autarquico/codefabric/internal/controller/relay"
	"github.com/Autarquico/codefabric/internal/controller/store"
	"github.com/Autarquico/codefabric/internal/domain"
)

// Bounds of the mechanical summary. Every list says how much it left out, so a
// reader knows the summary is a cut and not the whole story.
const (
	maxSectionBytes   = 4096
	maxPromptRunes    = 1000
	maxSubjectRunes   = 120
	maxCommits        = 20
	maxFilesPerCommit = 8
	maxExcluded       = 20
	maxEvents         = 15
	eventScan         = 200 // newest events of the Run looked at to pick the relevant ones
)

// History is what the summary asks of the git relay: the commits a Run made on
// top of the one it started from. *relay.Relay implements it.
type History interface {
	Log(ctx context.Context, project domain.ID, base, tip string, limit int) ([]relay.Commit, bool, error)
}

// WithHistory gives the service the history it summarizes the commits of a
// Run from, when it is not the relay given to WithRelay. Without one, the summary says the commits are unavailable.
func WithHistory(h History) Option { return func(s *Service) { s.history = h } }

// captured is what a checkpoint's summary is made of: everything the Controller
// already has when the Worker reports the capture done.
type captured struct {
	Task        store.Task
	Run         store.Run
	Checkpoint  store.Checkpoint
	Origin      domain.RunState
	Commit      string
	Commits     []relay.Commit
	MoreCommits bool
	NoHistory   bool
	Events      []domain.Event
	Excluded    []string
	NativeState *domain.NativeState
}

// summarize reads what the summary needs and composes it. A history the relay
// cannot give is no reason to refuse the checkpoint: the summary says so.
func (s *Service) summarize(ctx context.Context, q store.Querier, task store.Task, run store.Run, cp store.Checkpoint, origin domain.RunState, c Completion) (domain.CheckpointSummary, error) {
	in := captured{
		Task: task, Run: run, Checkpoint: cp, Origin: origin, Commit: c.Commit,
		Excluded: c.Excluded, NativeState: c.NativeState,
	}
	if s.history == nil || !domain.ValidCommitID(run.SourceCommit) {
		in.NoHistory = true
	} else if commits, more, err := s.history.Log(ctx, task.ProjectID, run.SourceCommit, c.Commit, maxCommits); err != nil {
		in.NoHistory = true
	} else {
		in.Commits, in.MoreCommits = commits, more
	}
	events, err := store.EventsTail(ctx, q, math.MaxInt64, eventScan, store.EventFilter{RunID: run.ID})
	if err != nil {
		return domain.CheckpointSummary{}, err
	}
	in.Events = events
	return compose(in), nil
}

// compose is the summary: markdown, deterministic, and cut to fixed bounds.
// Progress is what the Run did to the code, Next what it was after, Decisions
// what the capture left out or kept, Commands the trail of events.
func compose(in captured) domain.CheckpointSummary {
	return domain.CheckpointSummary{
		Progress:  limitBytes(progress(in)),
		Next:      limitBytes(next(in)),
		Decisions: limitBytes(decisions(in)),
		Commands:  limitBytes(trail(in)),
	}
}

func progress(in captured) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Checkpoint %d of the Run (%s) at commit %s.\n", in.Checkpoint.Seq, in.Checkpoint.Trigger, short(in.Commit))
	switch {
	case in.NoHistory:
		b.WriteString("\nThe commit history of the Run is not available.\n")
	case len(in.Commits) == 0:
		fmt.Fprintf(&b, "\nNo commits since %s: the work is in the files, not in commits.\n", short(in.Run.SourceCommit))
	default:
		fmt.Fprintf(&b, "\nCommits since %s, newest first:\n", short(in.Run.SourceCommit))
		for _, c := range in.Commits {
			fmt.Fprintf(&b, "- %s %s%s\n", short(c.ID), clip(c.Subject, maxSubjectRunes), files(c.Files))
		}
		if in.MoreCommits {
			b.WriteString("- ... and older commits, left out\n")
		}
	}
	return b.String()
}

func files(paths []string) string {
	if len(paths) == 0 {
		return ""
	}
	shown := paths[:min(len(paths), maxFilesPerCommit)]
	out := " (" + strings.Join(shown, ", ")
	if more := len(paths) - len(shown); more > 0 {
		out += fmt.Sprintf(", and %d more", more)
	}
	return out + ")"
}

func next(in captured) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Task %q.\n", clip(in.Task.Name, maxSubjectRunes))
	if p := strings.TrimSpace(in.Task.Prompt); p != "" {
		fmt.Fprintf(&b, "\nPrompt:\n\n%s\n", quote(clip(p, maxPromptRunes)))
	}
	if in.Origin == domain.RunWaitingInput {
		b.WriteString("\nThe Run was waiting for input when it was captured.\n")
	} else if in.Origin != "" {
		fmt.Fprintf(&b, "\nThe Run was %s when it was captured.\n", in.Origin)
	}
	return b.String()
}

func decisions(in captured) string {
	var b strings.Builder
	if n := in.NativeState; n != nil {
		fmt.Fprintf(&b, "Native state of %s %s was kept.\n", n.Adapter, n.AdapterVersion)
	} else {
		b.WriteString("No native state: only the source and this summary carry over, and the agent's own context is lost.\n")
	}
	if len(in.Excluded) == 0 {
		b.WriteString("\nNothing was left out of the capture.\n")
		return b.String()
	}
	b.WriteString("\nLeft out of the capture:\n")
	for _, p := range in.Excluded[:min(len(in.Excluded), maxExcluded)] {
		fmt.Fprintf(&b, "- %s\n", clip(p, maxSubjectRunes))
	}
	if more := len(in.Excluded) - maxExcluded; more > 0 {
		fmt.Fprintf(&b, "- ... and %d more\n", more)
	}
	return b.String()
}

// relevant says which events tell of what happened to the Run: where its state
// changed, and anything that warned or failed.
func relevant(e domain.Event) bool {
	switch e.Type {
	case domain.EventRunStarted, domain.EventRunWaitingInput, domain.EventRunSuspended, domain.EventRunSealed,
		domain.EventRunCheckpointed, domain.EventRunCheckpointFailed, domain.EventRunContinued, domain.EventRunForked:
		return true
	}
	return e.Level == domain.LevelWarn || e.Level == domain.LevelError
}

func trail(in captured) string {
	var picked []domain.Event
	for _, e := range in.Events {
		if relevant(e) {
			picked = append(picked, e)
		}
	}
	var b strings.Builder
	b.WriteString("The Controller does not record the commands the harness ran.\n")
	if len(picked) == 0 {
		return b.String()
	}
	b.WriteString("\nLatest events of the Run:\n")
	omitted := max(0, len(picked)-maxEvents)
	if omitted > 0 {
		fmt.Fprintf(&b, "- ... %d earlier events left out\n", omitted)
	}
	for _, e := range picked[omitted:] {
		fmt.Fprintf(&b, "- %s %s %s: %s\n", e.ReceivedAt.UTC().Format("2006-01-02T15:04:05Z"), e.Level, e.Type, clip(e.Message, maxSubjectRunes))
	}
	return b.String()
}

func short(oid string) string {
	if len(oid) > 7 {
		return oid[:7]
	}
	return oid
}

// clip keeps at most n runes of s and marks the cut.
func clip(s string, n int) string {
	if n > 0 && utf8.RuneCountInString(s) > n {
		return string([]rune(s)[:n]) + "..."
	}
	return s
}

// quote indents text as a markdown block quote so that a prompt cannot open
// headings or lists of the summary.
func quote(s string) string {
	return "> " + strings.ReplaceAll(strings.TrimSpace(s), "\n", "\n> ")
}

// limitBytes cuts a section at maxSectionBytes, at a rune boundary, and says so.
func limitBytes(s string) string {
	if len(s) <= maxSectionBytes {
		return s
	}
	cut := maxSectionBytes
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut] + "\n... truncated\n"
}
