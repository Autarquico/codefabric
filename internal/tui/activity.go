package tui

import (
	"cmp"
	"slices"
	"strings"
	"time"
	"unicode"

	v1 "github.com/Autarquico/codefabric/internal/api/v1"
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

// activitySubject is what the event is about, in the words the user knows:
// the name the event carries, else the entity it points at.
func activitySubject(e v1.Event) string {
	if name, ok := e.Data["name"].(string); ok && name != "" {
		return printable(name)
	}
	for _, id := range []string{e.TaskID, e.WorkerID, e.WorkflowID, e.ProjectID, e.RunID, e.Source} {
		if id != "" {
			return printable(id)
		}
	}
	return ""
}

// activityMessage is what happened.
func activityMessage(e v1.Event) string {
	if e.Message != "" {
		return printable(e.Message)
	}
	return string(e.Type)
}

// printable drops control characters from text that came from the server, so
// it cannot move the cursor, recolour the terminal or fake extra lines.
func printable(s string) string {
	return strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return -1
		}
		return r
	}, s)
}
