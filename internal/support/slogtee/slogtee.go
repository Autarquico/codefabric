// Package slogtee keeps a structured copy of what a process logs (§34): a
// slog.Handler that hands each record to the handler it wraps, as before, and
// puts a redacted JSON copy of it on a bounded queue. A background writer
// drains the queue into a sink (a database), apart from the goroutine that
// logged: logging never waits for the sink, and the sink may log through the
// same logger without recursing or deadlocking, because what it logs is only
// queued. What the writer itself has to say goes to a fallback logger, which
// must not be teed.
package slogtee

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode/utf8"

	"github.com/Autarquico/codefabric/internal/support/redact"
)

// Entry is a record as the tee keeps it. Attrs is a JSON object; RunID, TaskID
// and ProjectID are those named by top-level attributes run/run_id,
// task/task_id and project/project_id, when they are canonical IDs.
type Entry struct {
	Time      time.Time
	Level     string // debug, info, warn or error
	Msg       string
	Attrs     json.RawMessage
	RunID     string
	TaskID    string
	ProjectID string
}

// Sink stores a batch of entries, in order.
type Sink func(ctx context.Context, entries []Entry) error

// Defaults of Options.
const (
	DefaultCapacity        = 4096
	DefaultMaxMessageBytes = 4 << 10
	DefaultMaxAttrsBytes   = 16 << 10
)

const (
	batchSize    = 256
	drainTimeout = 2 * time.Second
	redacted     = "<redacted>"
)

// Options parameterise New. The zero value takes the defaults.
type Options struct {
	Capacity        int
	MaxMessageBytes int
	MaxAttrsBytes   int
	// Redact, if set, runs on the message and on every string value before the
	// blunt redaction of redact.Text: the place for the needles of known secrets.
	Redact func(string) string
}

// Tee is the queue and the writer that drains it.
type Tee struct {
	queue    chan Entry
	dropped  atomic.Uint64
	closed   atomic.Bool
	extra    func(string) string
	maxMsg   int
	maxAttrs int

	mu   sync.Mutex // the encoder: buf and cur are shared by every handler of the tee
	buf  bytes.Buffer
	cur  *ids
	null ids
}

type ids struct{ run, task, project string }

// New makes a tee; nothing is written until Run.
func New(o Options) *Tee {
	t := &Tee{
		queue: make(chan Entry, positive(o.Capacity, DefaultCapacity)), extra: o.Redact,
		maxMsg: positive(o.MaxMessageBytes, DefaultMaxMessageBytes), maxAttrs: positive(o.MaxAttrsBytes, DefaultMaxAttrsBytes),
	}
	t.cur = &t.null
	return t
}

func positive(n, def int) int {
	if n > 0 {
		return n
	}
	return def
}

// Dropped is how many records did not fit the queue, or were lost by a sink
// that failed, since the tee was made.
func (t *Tee) Dropped() uint64 { return t.dropped.Load() }

// Wrap tees inner: inner keeps getting every record, unchanged. A record is
// encoded under a lock of the tee, so the LogValue, Error and String of its
// values, and Options.Redact, must not log through the teed logger.
func (t *Tee) Wrap(inner slog.Handler) slog.Handler {
	enc := slog.NewJSONHandler(writerFunc(t.buf.Write), &slog.HandlerOptions{Level: slog.LevelDebug - 8, ReplaceAttr: t.replace})
	return &handler{tee: t, inner: inner, enc: enc}
}

type writerFunc func([]byte) (int, error)

func (w writerFunc) Write(p []byte) (int, error) { return w(p) }

type handler struct {
	tee   *Tee
	inner slog.Handler
	enc   slog.Handler // a JSON handler with the attributes and groups of this one
	ids   ids          // the IDs its WithAttrs named
}

func (h *handler) Enabled(ctx context.Context, l slog.Level) bool { return h.inner.Enabled(ctx, l) }

func (h *handler) Handle(ctx context.Context, r slog.Record) error {
	err := h.inner.Handle(ctx, r)
	if h.tee.closed.Load() {
		return err
	}
	e := h.tee.entry(ctx, h, r)
	select {
	case h.tee.queue <- e:
	default:
		h.tee.dropped.Add(1)
	}
	return err
}

func (h *handler) WithAttrs(as []slog.Attr) slog.Handler {
	n := *h
	n.inner = h.inner.WithAttrs(as)
	h.tee.mu.Lock()
	h.tee.cur = &n.ids // the JSON handler formats them now, through replace
	n.enc = h.enc.WithAttrs(as)
	h.tee.cur = &h.tee.null
	h.tee.mu.Unlock()
	return &n
}

func (h *handler) WithGroup(name string) slog.Handler {
	n := *h
	n.inner, n.enc = h.inner.WithGroup(name), h.enc.WithGroup(name)
	return &n
}

// entry encodes the attributes of r, redacted, with those of the handler.
func (t *Tee) entry(ctx context.Context, h *handler, r slog.Record) Entry {
	e := Entry{Time: r.Time, Level: levelName(r.Level), Msg: cut(t.redact(r.Message), t.maxMsg)}
	if e.Time.IsZero() {
		e.Time = time.Now()
	}
	// No time and no message: the JSON handler leaves the first out, replace the level and an empty message.
	rec := slog.NewRecord(time.Time{}, r.Level, "", 0)
	r.Attrs(func(a slog.Attr) bool { rec.AddAttrs(a); return true })

	t.mu.Lock()
	found := h.ids
	t.cur = &found
	t.buf.Reset()
	encErr := h.enc.Handle(ctx, rec)
	attrs := bytes.Clone(bytes.TrimSpace(t.buf.Bytes()))
	t.cur = &t.null
	t.mu.Unlock()

	switch {
	case encErr != nil || !json.Valid(attrs):
		attrs = []byte(`{"attrs_unencodable":true}`)
	case len(attrs) > t.maxAttrs:
		attrs = fmt.Appendf(nil, `{"attrs_truncated_bytes":%d}`, len(attrs))
	}
	e.Attrs = attrs
	e.RunID, e.TaskID, e.ProjectID = found.run, found.task, found.project
	return e
}

// sensitiveKey names attributes whose value is never kept, whatever it is.
var sensitiveKey = regexp.MustCompile(`(?i)(secret|token|passw|authorization|credential|private.?key|api.?key|cookie)`)

// replace is the ReplaceAttr of the encoder: it drops the level and the empty
// message the encoder adds, redacts every value, and notes the IDs.
func (t *Tee) replace(groups []string, a slog.Attr) slog.Attr {
	if len(groups) == 0 {
		switch a.Key {
		case slog.LevelKey:
			if _, ok := a.Value.Any().(slog.Level); ok {
				return slog.Attr{}
			}
		case slog.MessageKey:
			if a.Value.Kind() == slog.KindString && a.Value.String() == "" {
				return slog.Attr{}
			}
		}
	}
	if sensitiveKey.MatchString(a.Key) {
		return slog.String(a.Key, redacted)
	}
	switch a.Value.Kind() {
	case slog.KindString:
		s := a.Value.String()
		if len(groups) == 0 {
			t.note(a.Key, s)
		}
		return slog.String(a.Key, t.redact(s))
	case slog.KindAny:
		return slog.Attr{Key: a.Key, Value: t.redactAny(a.Value.Any())}
	}
	return a
}

func (t *Tee) redactAny(v any) slog.Value {
	switch v := v.(type) {
	case error:
		return slog.StringValue(t.redact(v.Error()))
	case fmt.Stringer:
		return slog.StringValue(t.redact(v.String()))
	}
	raw, err := json.Marshal(v)
	if err != nil {
		return slog.StringValue(t.redact(fmt.Sprint(v)))
	}
	s := t.redact(string(raw))
	if json.Valid([]byte(s)) {
		return slog.AnyValue(json.RawMessage(s))
	}
	return slog.StringValue(s)
}

func (t *Tee) note(key, value string) {
	switch {
	case (key == "run" || key == "run_id") && strings.HasPrefix(value, "run_"):
		t.cur.run = value
	case (key == "task" || key == "task_id") && strings.HasPrefix(value, "task_"):
		t.cur.task = value
	case (key == "project" || key == "project_id") && strings.HasPrefix(value, "project_"):
		t.cur.project = value
	}
}

func (t *Tee) redact(s string) string {
	if t.extra != nil {
		s = t.extra(s)
	}
	return redact.Text(s)
}

// cut keeps at most n bytes of s, at a character boundary.
func cut(s string, n int) string {
	if len(s) <= n {
		return s
	}
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n]
}

func levelName(l slog.Level) string {
	switch {
	case l < slog.LevelInfo:
		return "debug"
	case l < slog.LevelWarn:
		return "info"
	case l < slog.LevelError:
		return "warn"
	}
	return "error"
}

// Run writes what is queued to sink until ctx ends; then it stops taking
// records, writes what is left within a short time of its own, and returns.
// A write under way when ctx ends is not cut short: the sink gets a context
// that ctx does not cancel. The overflow of the queue is reported, as a warn
// entry before the next batch and on fallback; so is a batch the sink failed
// to store, which is lost.
func (t *Tee) Run(ctx context.Context, sink Sink, fallback *slog.Logger) {
	w := writer{tee: t, sink: sink, log: fallback}
	detached := context.WithoutCancel(ctx)
	for {
		select {
		case <-ctx.Done():
			t.closed.Store(true)
			drain, cancel := context.WithTimeout(detached, drainTimeout)
			for {
				batch := t.collect(nil)
				if len(batch) == 0 {
					break
				}
				w.write(drain, batch)
			}
			w.write(drain, nil) // the overflow of the last moments
			cancel()
			return
		case e := <-t.queue:
			w.write(detached, t.collect([]Entry{e}))
		}
	}
}

// collect takes what is queued, up to a batch, without waiting.
func (t *Tee) collect(batch []Entry) []Entry {
	for len(batch) < batchSize {
		select {
		case e := <-t.queue:
			batch = append(batch, e)
		default:
			return batch
		}
	}
	return batch
}

type writer struct {
	tee      *Tee
	sink     Sink
	log      *slog.Logger
	reported uint64
}

func (w *writer) write(ctx context.Context, batch []Entry) {
	if dropped := w.tee.dropped.Load(); dropped > w.reported {
		n := dropped - w.reported
		w.reported = dropped
		w.log.WarnContext(ctx, "log records were dropped: the queue of the structured logs was full", "dropped", n)
		batch = append([]Entry{{
			Time: time.Now(), Level: "warn", Msg: "log records were dropped: the queue of the structured logs was full",
			Attrs: fmt.Appendf(nil, `{"dropped":%d}`, n),
		}}, batch...)
	}
	if len(batch) == 0 {
		return
	}
	if err := w.sink(ctx, batch); err != nil {
		w.tee.dropped.Add(uint64(len(batch)))
		w.reported += uint64(len(batch)) // said here, not again as an overflow
		w.log.WarnContext(ctx, "storing the structured logs failed; they are lost", "entries", len(batch), "err", err)
	}
}
