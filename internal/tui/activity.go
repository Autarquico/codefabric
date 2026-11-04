package tui

import (
	"cmp"
	"slices"
	"strings"
	"time"
	"unicode"

	v1 "github.com/Autarquico/codefabric/internal/api/v1"
	"github.com/Autarquico/codefabric/internal/domain"
)

// maxActivity bounds what the log keeps: the Controller never serves more than
// this in a snapshot (v1.OverviewMaxActivity).
const maxActivity = v1.OverviewMaxActivity

// activityLog is the newest events first, each once. A snapshot repeats events
// that the stream already delivered (and the other way round), so the sequence
// is the identity.
type activityLog struct {
	events []v1.Event
}

// merge adds events, drops the ones already held and keeps the newest.
func (l *activityLog) merge(events ...v1.Event) {
	for _, e := range events {
		if !slices.ContainsFunc(l.events, func(h v1.Event) bool { return h.Sequence == e.Sequence }) {
			l.events = append(l.events, e)
		}
	}
	slices.SortFunc(l.events, func(a, b v1.Event) int { return cmp.Compare(b.Sequence, a.Sequence) })
	if len(l.events) > maxActivity {
		l.events = l.events[:maxActivity]
	}
}

// reset forgets what was held: after a resync the history may be another one.
func (l *activityLog) reset(events ...v1.Event) {
	l.events = nil
	l.merge(events...)
}

// latest is at most n of the newest events.
func (l *activityLog) latest(n int) []v1.Event {
	return l.events[:min(n, len(l.events))]
}

// activityTime is when an event happened, as the home shows it: the time of
// day for today, the date for earlier.
func activityTime(at, now time.Time, loc *time.Location) string {
	at, now = at.In(loc), now.In(loc)
	if at.YearDay() == now.YearDay() && at.Year() == now.Year() {
		return at.Format("15:04")
	}
	return at.Format("Jan 02")
}

// subjectKeys are the fields of an event's data that name its subject, the
// short ID last.
var subjectKeys = []string{"name", "title", "task_name", "worker_name", "project_name", shortIDKey}

const shortIDKey = "short_id"

// activitySubject is what the event is about, in the words the user knows: the
// name the event carries; else the short ID it carries; else the entity it
// points at, shortened so that it reads as an identifier. named says which of
// the first two it was; the caller mutes the rest.
func activitySubject(e v1.Event) (subject string, named bool) {
	for _, key := range subjectKeys {
		if text, ok := e.Data[key].(string); ok && printable(text) != "" {
			return printable(text), key != shortIDKey
		}
	}
	for _, id := range []string{e.TaskID, e.WorkerID, e.WorkflowID, e.ProjectID, e.RunID, e.Source} {
		if id != "" {
			return shortRef(printable(id)), false
		}
	}
	return "", false
}

// shortRef cuts "<kind>_<ULID>" to "<kind>…<last 6>". Text that is not an ID
// (a Source such as "controller") stays as it is.
func shortRef(id string) string {
	kind, ulid, ok := strings.Cut(id, "_")
	if !ok || len(ulid) < ulidLength {
		return id
	}
	return kind + "…" + ulid[len(ulid)-shortRefLength:]
}

const (
	ulidLength     = 26
	shortRefLength = 6
)

// activityMessage is what happened: the move of a Task, else the message the
// Controller wrote, else the type of the event in words.
func activityMessage(e v1.Event) string {
	if e.Type == domain.EventTaskStateChanged {
		if to, ok := e.Data["to"].(string); ok && printable(to) != "" {
			return "task → " + printable(to)
		}
	}
	if e.Message != "" {
		return printable(e.Message)
	}
	return humanize(printable(string(e.Type)))
}

// humanize turns "RunPublicationFailed" into "run publication failed".
func humanize(camel string) string {
	var b strings.Builder
	for i, r := range camel {
		if unicode.IsUpper(r) && i > 0 {
			b.WriteByte(' ')
		}
		b.WriteRune(unicode.ToLower(r))
	}
	return b.String()
}

// printable drops from text that came from the server what could act on the
// terminal or on the reading of the line: control characters (cursor moves,
// escape sequences, fake lines) and format characters (bidi overrides, zero
// width), which the CLI sanitizer and the Controller's validation also refuse.
func printable(s string) string {
	return strings.Map(func(r rune) rune {
		if unicode.IsControl(r) || unicode.Is(unicode.Cf, r) {
			return -1
		}
		return r
	}, s)
}
