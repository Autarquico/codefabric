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
	"github.com/Autarquico/codefabric/internal/support/clock"
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
	// Clock tells the time and runs every timeout and wait; default the system clock.
	Clock clock.Clock
	Log   *slog.Logger
	// Backoff bounds the wait between attempts to reach the Controller; zero
	// values mean 0.5 s growing to 10 s.
	BackoffInitial, BackoffMax time.Duration
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
	if o.BackoffInitial <= 0 {
		o.BackoffInitial = 500 * time.Millisecond
	}
	if o.BackoffMax <= 0 {
		o.BackoffMax = 10 * time.Second
	}
	if o.Clock == nil {
		o.Clock = clock.Real{}
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
	if !n.eligible(e) || !n.firstTime(e) {
		return
	}
	title, body := "CodeFabric: "+Sanitize(string(e.Type)), Sanitize(e.Message)
	if !n.quietNow() {
		n.logFailure(n.showNative(ctx, title, body))
	}
	n.logFailure(n.runHook(ctx, e))
}

// eligible applies the preferences: type, Project and severity.
func (n *Notifier) eligible(e v1.Event) bool {
	if !n.types[string(e.Type)] || n.muted[e.ProjectID] {
		return false
	}
	floor := n.opts.Prefs.MinLevel
	return floor == "" || domain.LevelAtLeast(e.Level, floor)
}

// Test delivers a sample notification through the native channel and the hook,
// bypassing preferences, quiet hours and dedupe, and returns what went wrong:
// a failed delivery, or nothing to deliver with at all.
func (n *Notifier) Test(ctx context.Context) error {
	e := v1.Event{Type: "NotifyTest", Message: "This is a test notification from cf.", Timestamp: n.opts.Clock.Now()}
	hook := len(n.opts.Prefs.Hook) > 0
	if n.opts.Native == nil && !hook {
		return errNothingToDeliver
	}
	var errs []error
	if n.opts.Native != nil {
		errs = append(errs, n.showNative(ctx, "CodeFabric: test", e.Message))
	}
	if hook {
		errs = append(errs, n.runHook(ctx, e))
	}
	return errors.Join(errs...)
}

var errNothingToDeliver = errors.New("no native notifications on this machine (osascript or notify-send) and no hook in client.json")

// logFailure logs a failed delivery; a missing native channel is said once.
func (n *Notifier) logFailure(err error) {
	if err == nil {
		return
	}
	if errors.Is(err, errNoNative) {
		n.mu.Lock()
		first := !n.warned
		n.warned = true
		n.mu.Unlock()
		if !first {
			return
		}
	}
	n.log.Warn("notify: " + err.Error())
}

func (n *Notifier) firstTime(e v1.Event) bool {
	n.mu.Lock()
	defer n.mu.Unlock()
	now := n.opts.Clock.Now()
	subject := string(e.Type) + "/" + subjectOf(e)
	seq := strconv.FormatUint(e.Sequence, 10)
	// Every new sequence is remembered, even if the subject suppresses it, so
	// that its replay after the window is still recognised.
	if e.Sequence != 0 {
		if n.seqs.has(seq, now) {
			return false
		}
		n.seqs.add(seq, now)
	}
	if n.subject.has(subject, now) {
		return false
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

var errNoNative = errors.New("this machine has no native notifications (osascript or notify-send)")

func (n *Notifier) showNative(ctx context.Context, title, body string) error {
	if n.opts.Native == nil {
		return errNoNative
	}
	ctx, cancel := n.withTimeout(ctx, nativeTimeout)
	defer cancel()
	if err := n.opts.Native.Show(ctx, title, body); err != nil {
		return fmt.Errorf("native notification failed: %w", err)
	}
	return nil
}

// withTimeout is context.WithTimeout on the injected clock.
func (n *Notifier) withTimeout(ctx context.Context, d time.Duration) (context.Context, context.CancelFunc) {
	ctx, cancel := context.WithCancelCause(ctx)
	t := n.opts.Clock.NewTimer(d)
	go func() {
		select {
		case <-t.C():
			cancel(context.DeadlineExceeded)
		case <-ctx.Done():
		}
	}()
	return ctx, func() {
		t.Stop()
		cancel(context.Canceled)
	}
}

// runHook runs the configured hook, if any, with e as JSON on its stdin.
func (n *Notifier) runHook(ctx context.Context, e v1.Event) error {
	argv := n.opts.Prefs.Hook
	if len(argv) == 0 {
		return nil
	}
	payload, err := json.Marshal(e)
	if err != nil {
		return fmt.Errorf("hook payload: %w", err)
	}
	ctx, cancel := n.withTimeout(ctx, hookTimeout)
	defer cancel()
	out, err := n.opts.Exec(ctx, argv[0], argv[1:], payload)
	if err != nil {
		return fmt.Errorf("hook %s failed: %w (output: %s)", argv[0], err, Sanitize(string(out)))
	}
	n.log.Info("notify: hook ran", "command", argv[0], "output", Sanitize(string(out)))
	return nil
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
	now := n.opts.Clock.Now().Local()
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
	FollowEvents(ctx context.Context, q client.EventQuery, fn func(v1.Event) error) error
}

// Watch notifies about the events that happen from now on, until ctx ends or
// the Controller says following would fail again (revoked credentials, a bad
// filter). A Controller that cannot be reached, at the start or later, is
// tried again with a backoff, and said once per outage. A cursor the Controller
// no longer holds resumes from now; replayed events are the dedupe's business.
func (n *Notifier) Watch(ctx context.Context, src Source) error {
	q := client.EventQuery{Types: n.Types()}
	if len(q.Types) == 0 {
		<-ctx.Done()
		return nil
	}
	delay, down := n.opts.BackoffInitial, false
	for {
		err := src.FollowEvents(ctx, q, func(e v1.Event) error {
			if down {
				n.log.Info("notify: reconnected to the Controller")
				down = false
			}
			delay = n.opts.BackoffInitial
			q.After = strconv.FormatUint(e.Sequence, 10)
			n.Handle(ctx, e)
			return nil
		})
		var coded *domain.Error
		switch {
		case err == nil, ctx.Err() != nil, errors.Is(err, context.Canceled):
			return nil
		case errors.As(err, &coded) && coded.Code == domain.CodeCursorExpired:
			n.log.Warn("notify: events were purged before they were read; continuing from now", "after", q.After)
			q.After = ""
			continue
		case errors.As(err, &coded) && permanent(coded.Code):
			return err
		}
		if !down {
			n.log.Warn("notify: cannot follow events; trying again", "err", err)
			down = true
		}
		wait := n.opts.Clock.NewTimer(delay)
		select {
		case <-ctx.Done():
			wait.Stop()
			return nil
		case <-wait.C():
		}
		delay = min(delay*2, n.opts.BackoffMax)
	}
}

// permanent: the Controller answered, and asking again would fail the same.
func permanent(code domain.Code) bool {
	switch code {
	case domain.CodeUnauthenticated, domain.CodePolicyDenied, domain.CodeInvalidRequest, domain.CodeNotFound, domain.CodeVersionIncompatible:
		return true
	}
	return false
}
