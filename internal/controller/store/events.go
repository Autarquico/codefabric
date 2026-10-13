package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/Autarquico/codefabric/internal/domain"
)

// EventSchemaVersion is stamped on events that do not carry one.
const EventSchemaVersion = 1

const (
	sourceController = "controller"
	defaultEventPage = 500
)

// MaxEventBytes bounds the message and the data of an event. Serialised for the
// API, with every character escaped at its worst, an event stays under
// v1.EventMaxBytes (256 KiB): a page of events is bounded by bytes only if no
// single event can be larger than the bound.
const MaxEventBytes = 24 << 10

// ErrInvalidEvent is returned by AppendEvent for events that break §20.1/§20.2.
var ErrInvalidEvent = errors.New("invalid event")

const insertEvent = `
INSERT INTO events (id, schema_version, ts, received_at, source, worker_epoch, worker_seq,
    request_id, type, level, actor_kind, actor_id, project_id, plan_id, task_id, run_id,
    worker_id, workflow_id, message, data)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
ON CONFLICT (worker_id, worker_epoch, worker_seq) DO NOTHING
RETURNING sequence`

// AppendEvent adds e to the event store and returns its global sequence. A
// Worker event is identified by (WorkerID, WorkerEpoch, WorkerSeq): appending
// the same triple again stores nothing and returns the original sequence with
// dup=true. The event's own Sequence and ReceivedAt are ignored; ReceivedAt
// comes from the store's clock and Data is stored as JSON.
func (t *Tx) AppendEvent(ctx context.Context, e domain.Event) (seq uint64, dup bool, err error) {
	if err := validateEvent(e); err != nil {
		return 0, false, err
	}
	data, err := marshalData(e.Data)
	if err != nil {
		return 0, false, err
	}
	size := len(e.Message)
	if doc, ok := data.(string); ok {
		size += len(doc)
	}
	if size > MaxEventBytes {
		return 0, false, fmt.Errorf("%w: message and data are %d bytes, at most %d", ErrInvalidEvent, size, MaxEventBytes)
	}

	receivedAt := t.db.clock.Now()
	if e.Timestamp.IsZero() {
		e.Timestamp = receivedAt
	}
	if e.ID == "" {
		e.ID = t.db.ids.New(domain.KindEvent)
	}
	if e.SchemaVersion == 0 {
		e.SchemaVersion = EventSchemaVersion
	}
	if e.Source == "" {
		e.Source = sourceController
		if e.WorkerSeq != nil {
			e.Source = "worker:" + string(e.WorkerID)
		}
	}
	var workerSeq any // uint64 is accepted by database/sql while it fits in int64, checked above
	if e.WorkerSeq != nil {
		workerSeq = *e.WorkerSeq
	}

	err = t.QueryRow(ctx, insertEvent,
		string(e.ID), e.SchemaVersion, e.Timestamp.UnixNano(), receivedAt.UnixNano(), e.Source,
		nullable(e.WorkerEpoch), workerSeq, nullable(e.RequestID), string(e.Type), e.Level,
		string(e.Actor.Kind), e.Actor.ID,
		nullable(string(e.ProjectID)), nullable(string(e.PlanID)), nullable(string(e.TaskID)),
		nullable(string(e.RunID)), nullable(string(e.WorkerID)), nullable(string(e.WorkflowID)),
		e.Message, data,
	).Scan(&seq)

	switch {
	case err == nil:
		t.maxSeq = max(t.maxSeq, seq)
		return seq, false, nil
	case errors.Is(err, sql.ErrNoRows) && e.WorkerSeq != nil:
		// DO NOTHING returns no row: the triple exists already.
		err = t.QueryRow(ctx,
			`SELECT sequence FROM events WHERE worker_id = ? AND worker_epoch = ? AND worker_seq = ?`,
			string(e.WorkerID), e.WorkerEpoch, workerSeq).Scan(&seq)
		if err != nil {
			return 0, false, fmt.Errorf("store: finding duplicate event: %w", err)
		}
		return seq, true, nil
	default:
		return 0, false, fmt.Errorf("store: appending event: %w", err)
	}
}

// validateEvent enforces that the dedup key is whole and that heartbeats stay
// out of the store (§20.1). Controller events may still name the Worker they
// are about in WorkerID (WorkerLost); only WorkerEpoch and WorkerSeq mark the
// event as emitted by that Worker.
func validateEvent(e domain.Event) error {
	switch {
	case e.Type == "":
		return fmt.Errorf("%w: missing type", ErrInvalidEvent)
	case e.Type == domain.EventWorkerHeartbeat:
		return fmt.Errorf("%w: %s is never persisted", ErrInvalidEvent, e.Type)
	case (e.WorkerEpoch == "") != (e.WorkerSeq == nil):
		return fmt.Errorf("%w: WorkerEpoch and WorkerSeq go together", ErrInvalidEvent)
	case e.WorkerSeq != nil && e.WorkerID == "":
		return fmt.Errorf("%w: a Worker event needs WorkerID", ErrInvalidEvent)
	case e.WorkerSeq != nil && *e.WorkerSeq > math.MaxInt64:
		return fmt.Errorf("%w: WorkerSeq out of range", ErrInvalidEvent)
	}
	return nil
}

func marshalData(data map[string]any) (any, error) {
	if len(data) == 0 {
		return nil, nil
	}
	raw, err := json.Marshal(data)
	if err != nil {
		return nil, fmt.Errorf("%w: data is not JSON: %w", ErrInvalidEvent, err)
	}
	return string(raw), nil
}

// nullable maps the empty string to SQL NULL.
func nullable(s string) any {
	if s == "" {
		return nil
	}
	return s
}

// EventFilter narrows Events. Zero fields match everything. MinLevel keeps the
// events at that level or above; Since those the Controller received at or
// after that instant.
type EventFilter struct {
	ProjectID domain.ID
	TaskID    domain.ID
	RunID     domain.ID
	WorkerID  domain.ID
	Types     []domain.EventType
	MinLevel  string
	Since     time.Time
}

const eventColumns = `sequence, id, schema_version, ts, received_at, source,
    COALESCE(worker_epoch, ''), worker_seq, COALESCE(request_id, ''), type, level, actor_kind, actor_id,
    COALESCE(project_id, ''), COALESCE(plan_id, ''), COALESCE(task_id, ''), COALESCE(run_id, ''),
    COALESCE(worker_id, ''), COALESCE(workflow_id, ''), message, data`

// where is the WHERE clause of the filter, with its arguments, for events with
// after < sequence <= through.
func (f EventFilter) where(after, through uint64) (string, []any) {
	var (
		where = []string{"sequence > ?", "sequence <= ?"}
		args  = []any{after, through}
	)
	for _, c := range []struct {
		column string
		value  domain.ID
	}{
		{"project_id", f.ProjectID},
		{"task_id", f.TaskID},
		{"run_id", f.RunID},
		{"worker_id", f.WorkerID},
	} {
		if c.value != "" {
			where = append(where, c.column+" = ?")
			args = append(args, string(c.value))
		}
	}
	if len(f.Types) > 0 {
		where = append(where, "type IN (?"+strings.Repeat(", ?", len(f.Types)-1)+")")
		for _, ty := range f.Types {
			args = append(args, string(ty))
		}
	}
	if levels := domain.LevelsFrom(f.MinLevel); len(levels) > 0 && len(levels) < 4 {
		where = append(where, "level IN (?"+strings.Repeat(", ?", len(levels)-1)+")")
		for _, l := range levels {
			args = append(args, l)
		}
	}
	if !f.Since.IsZero() {
		where = append(where, "received_at >= ?")
		args = append(args, f.Since.UnixNano())
	}
	return strings.Join(where, " AND "), args
}

// Events returns up to limit events with Sequence > after, oldest first. It is
// how every consumer reads the stream: the Notifier only says when to look.
func (db *DB) Events(ctx context.Context, after uint64, limit int, filter EventFilter) ([]domain.Event, error) {
	var out []domain.Event
	err := db.Read(ctx, func(q Querier) (err error) {
		out, err = EventsAfter(ctx, q, after, math.MaxInt64, limit, filter)
		return err
	})
	return out, err
}

// EventsAfter returns up to limit events with after < Sequence <= through,
// oldest first. The ceiling is the high-water mark a reader took before it
// looked: an event above it is announced by the Notifier a moment later.
func EventsAfter(ctx context.Context, q Querier, after, through uint64, limit int, filter EventFilter) ([]domain.Event, error) {
	if limit <= 0 {
		limit = defaultEventPage
	}
	where, args := filter.where(after, through)
	// Only constant column names and placeholders are concatenated; values are bound.
	return readEvents(ctx, q, `SELECT `+eventColumns+` FROM events WHERE `+where+` ORDER BY sequence LIMIT ?`, //nolint:gosec // constants only
		append(args, limit)...)
}

// EventsTail returns the last limit events with Sequence <= through that match
// the filter, oldest first.
func EventsTail(ctx context.Context, q Querier, through uint64, limit int, filter EventFilter) ([]domain.Event, error) {
	if limit <= 0 {
		limit = defaultEventPage
	}
	where, args := filter.where(0, through)
	events, err := readEvents(ctx, q, `SELECT `+eventColumns+` FROM events WHERE `+where+` ORDER BY sequence DESC LIMIT ?`, //nolint:gosec // constants only
		append(args, limit)...)
	slices.Reverse(events)
	return events, err
}

func readEvents(ctx context.Context, q Querier, query string, args ...any) ([]domain.Event, error) {
	rows, err := q.Query(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("store: querying events: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var events []domain.Event
	for rows.Next() {
		e, err := scanEvent(rows)
		if err != nil {
			return nil, err
		}
		events = append(events, e)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: reading events: %w", err)
	}
	return events, nil
}

// purgedThroughKey is the controller_meta key of the highest sequence that the
// retention of events (Phase 4) has deleted; absent means none.
const purgedThroughKey = "events_purged_through"

// EventBounds are the highest sequence stored and the highest one purged, as
// the snapshot q sees them. A cursor outside [purged, latest] is not one this
// history can resume.
func EventBounds(ctx context.Context, q Querier) (latest, purgedThrough uint64, err error) {
	if err := q.QueryRow(ctx, `SELECT COALESCE(MAX(sequence), 0) FROM events`).Scan(&latest); err != nil {
		return 0, 0, fmt.Errorf("store: reading the latest sequence: %w", err)
	}
	var raw string
	switch err := q.QueryRow(ctx, `SELECT value FROM controller_meta WHERE key = ?`, purgedThroughKey).Scan(&raw); {
	case errors.Is(err, sql.ErrNoRows):
		return latest, 0, nil
	case err != nil:
		return 0, 0, fmt.Errorf("store: reading %s: %w", purgedThroughKey, err)
	}
	if purgedThrough, err = strconv.ParseUint(raw, 10, 63); err != nil {
		return 0, 0, fmt.Errorf("store: %s is not a sequence: %w", purgedThroughKey, err)
	}
	return latest, purgedThrough, nil
}

func scanEvent(rows *sql.Rows) (domain.Event, error) {
	var (
		e                    domain.Event
		ts, receivedAt       int64
		workerSeq            sql.Null[uint64]
		eventType, actorKind string
		ids                  [6]string
		data                 sql.NullString
	)
	err := rows.Scan(&e.Sequence, &e.ID, &e.SchemaVersion, &ts, &receivedAt, &e.Source,
		&e.WorkerEpoch, &workerSeq, &e.RequestID, &eventType, &e.Level, &actorKind, &e.Actor.ID,
		&ids[0], &ids[1], &ids[2], &ids[3], &ids[4], &ids[5], &e.Message, &data)
	if err != nil {
		return domain.Event{}, fmt.Errorf("store: scanning event: %w", err)
	}
	e.Timestamp = time.Unix(0, ts).UTC()
	e.ReceivedAt = time.Unix(0, receivedAt).UTC()
	e.Type = domain.EventType(eventType)
	e.Actor.Kind = domain.ActorKind(actorKind)
	e.ProjectID, e.PlanID, e.TaskID = domain.ID(ids[0]), domain.ID(ids[1]), domain.ID(ids[2])
	e.RunID, e.WorkerID, e.WorkflowID = domain.ID(ids[3]), domain.ID(ids[4]), domain.ID(ids[5])
	if workerSeq.Valid {
		e.WorkerSeq = &workerSeq.V
	}
	if data.Valid {
		if err := json.Unmarshal([]byte(data.String), &e.Data); err != nil {
			return domain.Event{}, fmt.Errorf("store: event %s has corrupt data: %w", e.ID, err)
		}
	}
	return e, nil
}
