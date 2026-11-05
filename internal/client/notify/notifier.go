// Package notify tells a person, on their own machine, about the events of the
// Controller that need them (spec §26). It is a client of the event stream: it
// runs inside the `cf` of the user (the TUI, or `cf notify watch`), and the
// desktop and the hook are adapters injected by the binary.
package notify

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"slices"
	"strconv"
	"sync"
	"time"

	v1 "github.com/Autarquico/codefabric/internal/api/v1"
	"github.com/Autarquico/codefabric/internal/client"
	"github.com/Autarquico/codefabric/internal/config"
	"github.com/Autarquico/codefabric/internal/domain"
)

// Bounds of the dedupe state and of the adapters.
const (
	// DedupeWindow is how long the same event type for the same subject notifies once.
	DedupeWindow = 10 * time.Minute
	maxSequences = 4096
	maxSubjects  = 1024

	nativeTimeout = 5 * time.Second
	hookTimeout   = 10 * time.Second
)

// Options compose a Notifier. Everything but Prefs is optional.
type Options struct {
	Prefs config.ClientNotify
	// Native shows desktop notifications; nil means this machine has none (said once).
	Native Native
	// Exec runs the hook; default OSExec.
	Exec Exec
	Now  func() time.Time
	Log  *slog.Logger
}

// Notifier applies the preferences, the dedupe and the delivery to events.
type Notifier struct {
	opts    Options
	types   map[string]bool
	muted   map[string]bool
	quiet   *config.QuietHours
	log     *slog.Logger
	mu      sync.Mutex
	seqs    *seen
	subject *seen
	warned  bool
}

// New builds a Notifier.
func New(o Options) *Notifier {
	if o.Exec == nil {
		o.Exec = OSExec
	}
	if o.Now == nil {
		o.Now = time.Now
	}
	n := &Notifier{
		opts: o, types: map[string]bool{}, muted: map[string]bool{}, quiet: o.Prefs.QuietHours, log: o.Log,
		seqs: newSeen(maxSequences, 0), subject: newSeen(maxSubjects, DedupeWindow),
	}
	if n.log == nil {
		n.log = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	for _, t := range o.Prefs.NotifyEvents() {
		n.types[t] = true
	}
	for _, p := range o.Prefs.MutedProjects {
		n.muted[p] = true
	}
	return n
}

// Types are the event types that notify, for the filter of the stream.
func (n *Notifier) Types() []string {
	out := make([]string, 0, len(n.types))
	for t := range n.types {
		out = append(out, t)
	}
	slices.Sort(out)
	return out
}

// Handle notifies about e unless the preferences or the dedupe say not to. An
// event is the same one if it has the same sequence (a replay after a resync)
// or the same type and subject within DedupeWindow (a repeat). Failures of the
// adapters are logged, not returned: a notification never stops the stream.
func (n *Notifier) Handle(ctx context.Context, e v1.Event) {
	if !n.types[string(e.Type)] || n.muted[e.ProjectID] || !n.firstTime(e) {
		return
	}
	title, body := "CodeFabric: "+Sanitize(string(e.Type)), Sanitize(e.Message)
	n.deliver(ctx, title, body, e)
}

// Test delivers a sample notification, bypassing preferences and dedupe.
func (n *Notifier) Test(ctx context.Context) {
	e := v1.Event{Type: "NotifyTest", Message: "This is a test notification from cf.", Timestamp: n.opts.Now()}
	n.deliver(ctx, "CodeFabric: test", e.Message, e)
}

func (n *Notifier) firstTime(e v1.Event) bool {
	n.mu.Lock()
	defer n.mu.Unlock()
	now := n.opts.Now()
	subject := string(e.Type) + "/" + subjectOf(e)
	seq := strconv.FormatUint(e.Sequence, 10)
	if (e.Sequence != 0 && n.seqs.has(seq, now)) || n.subject.has(subject, now) {
		return false
	}
	if e.Sequence != 0 {
		n.seqs.add(seq, now)
	}
	n.subject.add(subject, now)
	return true
}

func subjectOf(e v1.Event) string {
	for _, id := range []string{e.RunID, e.TaskID, e.WorkerID, e.WorkflowID, e.ProjectID} {
		if id != "" {
			return id
		}
	}
	return ""
}

func (n *Notifier) deliver(ctx context.Context, title, body string, e v1.Event) {
	if !n.quietNow() {
		n.showNative(ctx, title, body)
	}
	if hook := n.opts.Prefs.Hook; len(hook) > 0 {
		n.runHook(ctx, hook, e)
	}
}

func (n *Notifier) showNative(ctx context.Context, title, body string) {
	if n.opts.Native == nil {
		n.mu.Lock()
		first := !n.warned
		n.warned = true
		n.mu.Unlock()
		if first {
			n.log.Warn("notify: this machine has no native notifications (osascript or notify-send)")
		}
		return
	}
	ctx, cancel := context.WithTimeout(ctx, nativeTimeout)
	defer cancel()
	if err := n.opts.Native.Show(ctx, title, body); err != nil {
		n.log.Warn("notify: native notification failed", "err", err)
	}
}

func (n *Notifier) runHook(ctx context.Context, argv []string, e v1.Event) {
	payload, err := json.Marshal(e)
	if err != nil {
		n.log.Warn("notify: hook payload", "err", err)
		return
	}
	ctx, cancel := context.WithTimeout(ctx, hookTimeout)
	defer cancel()
	out, err := n.opts.Exec(ctx, argv[0], argv[1:], payload)
	if err != nil {
		n.log.Warn("notify: hook failed", "command", argv[0], "err", err, "output", Sanitize(string(out)))
		return
	}
	n.log.Info("notify: hook ran", "command", argv[0], "output", Sanitize(string(out)))
}

// quietNow says whether the local time of now is inside the quiet hours.
func (n *Notifier) quietNow() bool {
	if n.quiet == nil {
		return false
	}
	start, err1 := time.Parse("15:04", n.quiet.Start)
	end, err2 := time.Parse("15:04", n.quiet.End)
	if err1 != nil || err2 != nil {
		return false
	}
	now := n.opts.Now().Local()
	at := time.Duration(now.Hour())*time.Hour + time.Duration(now.Minute())*time.Minute
	from := time.Duration(start.Hour())*time.Hour + time.Duration(start.Minute())*time.Minute
	to := time.Duration(end.Hour())*time.Hour + time.Duration(end.Minute())*time.Minute
	if from <= to {
		return at >= from && at < to
	}
	return at >= from || at < to
}

// Source is the part of the SDK client that Watch reads events from.
type Source interface {
	Events(ctx context.Context, q client.EventQuery) (v1.EventPage, error)
	FollowEvents(ctx context.Context, q client.EventQuery, fn func(v1.Event) error) error
}

// Watch notifies about the events that happen from now on, until ctx ends or
// the Controller says following would fail again. A cursor the Controller no
// longer holds resumes from now; replayed events are the dedupe's business.
func (n *Notifier) Watch(ctx context.Context, src Source) error {
	q := client.EventQuery{Types: n.Types()}
	if len(q.Types) == 0 {
		<-ctx.Done()
		return nil
	}
	for {
		err := src.FollowEvents(ctx, q, func(e v1.Event) error {
			q.After = strconv.FormatUint(e.Sequence, 10)
			n.Handle(ctx, e)
			return nil
		})
		var coded *domain.Error
		switch {
		case err == nil, errors.Is(err, context.Canceled):
			return nil
		case errors.As(err, &coded) && coded.Code == domain.CodeCursorExpired:
			page, perr := src.Events(ctx, client.EventQuery{Tail: 1})
			if perr != nil {
				return fmt.Errorf("notify: resuming after a purge: %w", perr)
			}
			q.After = page.Cursor
		default:
			return err
		}
	}
}
