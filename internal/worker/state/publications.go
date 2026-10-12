package state

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
)

// PubState is where the publication of a result stands on this Worker. It is
// local: the Controller's publication states are another thing (§22.6).
//
// sealing: the intent is stored and the commit is not made. sealed: the commit
// exists and its `sealed` report is in the journal. pushed: the relay accepted
// the push and the `published` report is in the journal; it stays until the
// Controller's view of the Run confirms it. published and failed are final.
type PubState string

// Publication states.
const (
	PubSealing   PubState = "sealing"
	PubSealed    PubState = "sealed"
	PubPushed    PubState = "pushed"
	PubPublished PubState = "published"
	PubFailed    PubState = "failed"
)

// Publication is a row of publications.
type Publication struct {
	Key
	State             PubState
	ParentOID         string    // the intent: HEAD of the worktree at the exit ("" if there was no repository)
	SealedAt          time.Time // the intent: when the harness exited
	ResultCommit      string
	Excluded          []string
	ExcludedTruncated bool
	Generation        int
	PublishedSeq      uint64 // the journal seq of the `published` report of a push waiting to be confirmed; 0 if none
	Attempts          int
	NextAttemptAt     time.Time
	LastError         string
	CreatedAt         time.Time
	UpdatedAt         time.Time
}

type excludedDoc struct {
	Paths     []string `json:"paths"`
	Truncated bool     `json:"truncated"`
}

const pubColumns = `run_id, lease_epoch, state, parent_oid, sealed_at, COALESCE(result_commit, ''), COALESCE(excluded, ''),
    generation, COALESCE(published_seq, 0), attempts, next_attempt_at, COALESCE(last_error, ''), created_at, updated_at`

func scanPublication(row scanner) (Publication, error) {
	var (
		p                              Publication
		state, excluded                string
		sealed, next, created, updated int64
	)
	if err := row.Scan(&p.RunID, &p.LeaseEpoch, &state, &p.ParentOID, &sealed, &p.ResultCommit, &excluded,
		&p.Generation, &p.PublishedSeq, &p.Attempts, &next, &p.LastError, &created, &updated); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return Publication{}, ErrNotFound
		}
		return Publication{}, fmt.Errorf("state: scanning a publication: %w", err)
	}
	p.State = PubState(state)
	p.SealedAt, p.NextAttemptAt = time.Unix(0, sealed).UTC(), time.Unix(0, next).UTC()
	p.CreatedAt, p.UpdatedAt = time.Unix(0, created).UTC(), time.Unix(0, updated).UTC()
	if excluded != "" {
		var doc excludedDoc
		if err := json.Unmarshal([]byte(excluded), &doc); err != nil {
			return Publication{}, fmt.Errorf("state: a publication has a corrupt excluded list: %w", err)
		}
		p.Excluded, p.ExcludedTruncated = doc.Paths, doc.Truncated
	}
	return p, nil
}

// InsertPublication stores the sealing intent of a Run that executed. It is
// written once: an intent that exists is kept, since it is what makes sealing
// repeatable. inserted says which happened.
func InsertPublication(ctx context.Context, tx *Tx, p Publication) (inserted bool, err error) {
	res, err := tx.exec(ctx,
		`INSERT INTO publications (run_id, lease_epoch, state, parent_oid, sealed_at, next_attempt_at, created_at, updated_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?) ON CONFLICT (run_id, lease_epoch) DO NOTHING`,
		p.RunID, p.LeaseEpoch, string(PubSealing), p.ParentOID, p.SealedAt.UnixNano(),
		p.CreatedAt.UnixNano(), p.CreatedAt.UnixNano(), p.CreatedAt.UnixNano())
	if err != nil {
		return false, fmt.Errorf("state: storing a sealing intent: %w", err)
	}
	n, err := res.RowsAffected()
	return n == 1, err
}

// PublicationOf returns ErrNotFound for a Run with no publication.
func PublicationOf(ctx context.Context, q Querier, k Key) (Publication, error) {
	return scanPublication(q.QueryRowContext(ctx, `SELECT `+pubColumns+` FROM publications WHERE run_id = ? AND lease_epoch = ?`, k.RunID, k.LeaseEpoch))
}

func collectPublications(ctx context.Context, q Querier, query string, args ...any) ([]Publication, error) {
	rows, err := q.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("state: querying publications: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []Publication
	for rows.Next() {
		p, err := scanPublication(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// DuePublications lists those that have something to do now: sealing, or
// sealed and waiting for a push, whose time has come, oldest first.
func DuePublications(ctx context.Context, q Querier, now time.Time, limit int) ([]Publication, error) {
	return collectPublications(ctx, q,
		`SELECT `+pubColumns+` FROM publications WHERE state IN ('sealing', 'sealed') AND next_attempt_at <= ?
		 ORDER BY next_attempt_at, created_at, run_id LIMIT ?`, now.UnixNano(), limit)
}

// SetParent completes an intent that had no parent because the worktree was not
// a repository when the harness exited, for a new try that finds it is one now.
// Compare-and-set on the generation and the state sealing.
func SetParent(ctx context.Context, tx *Tx, p Publication, parent string, at time.Time) error {
	res, err := tx.exec(ctx,
		`UPDATE publications SET parent_oid = ?, updated_at = ?
		 WHERE run_id = ? AND lease_epoch = ? AND generation = ? AND state = ? AND parent_oid = ''`,
		parent, at.UnixNano(), p.RunID, p.LeaseEpoch, p.Generation, string(PubSealing))
	return changedOne(res, err, "completing a sealing intent")
}

// MarkSealed records the commit of the sealing. It is a compare-and-set on the
// generation of the try and on the state sealing.
func MarkSealed(ctx context.Context, tx *Tx, p Publication, commit string, excluded []string, truncated bool, at time.Time) error {
	raw, err := json.Marshal(excludedDoc{Paths: excluded, Truncated: truncated})
	if err != nil {
		return fmt.Errorf("state: encoding the excluded paths: %w", err)
	}
	res, err := tx.exec(ctx,
		`UPDATE publications SET state = ?, result_commit = ?, excluded = ?, attempts = 0, next_attempt_at = ?, updated_at = ?
		 WHERE run_id = ? AND lease_epoch = ? AND generation = ? AND state = ?`,
		string(PubSealed), commit, string(raw), at.UnixNano(), at.UnixNano(), p.RunID, p.LeaseEpoch, p.Generation, string(PubSealing))
	return changedOne(res, err, "sealing a publication")
}

// MarkPushed records that the relay took the push, and the seq of its `published`
// report. Compare-and-set on the generation, the state sealed and the attempt it
// was made in.
func MarkPushed(ctx context.Context, tx *Tx, p Publication, seq uint64, at time.Time) error {
	res, err := tx.exec(ctx,
		`UPDATE publications SET state = ?, published_seq = ?, updated_at = ?
		 WHERE run_id = ? AND lease_epoch = ? AND generation = ? AND attempts = ? AND state = ?`,
		string(PubPushed), seq, at.UnixNano(), p.RunID, p.LeaseEpoch, p.Generation, p.Attempts, string(PubSealed))
	return changedOne(res, err, "recording a push")
}

// Defer postpones the next try of a publication that cannot move now, without
// counting an attempt. Compare-and-set on the generation, the state and the
// attempts it was read with: the postponement of an older try does not delay a newer one.
func Defer(ctx context.Context, tx *Tx, p Publication, next time.Time, why string, at time.Time) error {
	res, err := tx.exec(ctx,
		`UPDATE publications SET next_attempt_at = ?, last_error = ?, updated_at = ?
		 WHERE run_id = ? AND lease_epoch = ? AND generation = ? AND attempts = ? AND state = ?`,
		next.UnixNano(), why, at.UnixNano(), p.RunID, p.LeaseEpoch, p.Generation, p.Attempts, string(p.State))
	return changedOne(res, err, "postponing a publication")
}

// Retry schedules the next try of a sealed (or sealing) publication. counted says
// whether this one counts against the attempts allowed; the new count is returned.
// Compare-and-set on the generation, the state and the attempts it was read with.
func Retry(ctx context.Context, tx *Tx, p Publication, counted bool, next time.Time, why string, at time.Time) (attempts int, err error) {
	attempts = p.Attempts
	if counted {
		attempts++
	}
	res, err := tx.exec(ctx,
		`UPDATE publications SET attempts = ?, next_attempt_at = ?, last_error = ?, updated_at = ?
		 WHERE run_id = ? AND lease_epoch = ? AND generation = ? AND attempts = ? AND state = ?`,
		attempts, next.UnixNano(), why, at.UnixNano(), p.RunID, p.LeaseEpoch, p.Generation, p.Attempts, string(p.State))
	return attempts, changedOne(res, err, "scheduling a publication")
}

// FailPublication gives up: the publication is final until a publish order.
// Compare-and-set on the generation and the state the caller read.
func FailPublication(ctx context.Context, tx *Tx, p Publication, why string, at time.Time) error {
	res, err := tx.exec(ctx,
		`UPDATE publications SET state = ?, last_error = ?, updated_at = ?
		 WHERE run_id = ? AND lease_epoch = ? AND generation = ? AND state = ?`,
		string(PubFailed), why, at.UnixNano(), p.RunID, p.LeaseEpoch, p.Generation, string(p.State))
	return changedOne(res, err, "failing a publication")
}

// ConfirmPublication ends a pushed publication with what the Controller's view
// of the Run says: published or failed. It does so only if the `published` report
// of that push is one of the seqs the Controller just settled; a publication that
// is not pushed any more, or whose push is another, is left as it is, since the
// view is about an older try.
func ConfirmPublication(ctx context.Context, tx *Tx, k Key, confirmed []uint64, to PubState, at time.Time) (bool, error) {
	if len(confirmed) == 0 {
		return false, nil
	}
	args := []any{string(to), at.UnixNano(), k.RunID, k.LeaseEpoch, string(PubPushed)}
	for _, seq := range confirmed {
		args = append(args, seq)
	}
	marks := strings.TrimSuffix(strings.Repeat("?,", len(confirmed)), ",")
	res, err := tx.exec(ctx,
		`UPDATE publications SET state = ?, updated_at = ? WHERE run_id = ? AND lease_epoch = ? AND state = ? AND published_seq IN (`+marks+`)`, args...) //nolint:gosec // only placeholders are concatenated
	if err != nil {
		return false, fmt.Errorf("state: confirming a publication: %w", err)
	}
	n, err := res.RowsAffected()
	return n == 1, err
}

// RequeuePublication starts a new try on a publish order: the same commit if
// there is one, a new sealing if there is none. It is a no-op for a publication
// that is already trying. found is false for a Run with no publication.
func RequeuePublication(ctx context.Context, tx *Tx, k Key, at time.Time) (requeued bool, err error) {
	res, err := tx.exec(ctx,
		`UPDATE publications SET state = CASE WHEN result_commit IS NULL THEN ? ELSE ? END, generation = generation + 1,
		        published_seq = NULL, attempts = 0, next_attempt_at = ?, last_error = NULL, updated_at = ?
		 WHERE run_id = ? AND lease_epoch = ? AND state IN ('failed', 'pushed', 'published')`,
		string(PubSealing), string(PubSealed), at.UnixNano(), at.UnixNano(), k.RunID, k.LeaseEpoch)
	if err != nil {
		return false, fmt.Errorf("state: requeueing a publication: %w", err)
	}
	n, err := res.RowsAffected()
	return n == 1, err
}

// UnsettledPublications lists the publications the Controller has not settled
// here: sealing, sealed or pushed.
func UnsettledPublications(ctx context.Context, q Querier) ([]Publication, error) {
	return collectPublications(ctx, q, `SELECT `+pubColumns+` FROM publications WHERE state IN ('sealing', 'sealed', 'pushed') ORDER BY run_id, lease_epoch`)
}

// MarkPreserved ends the publication of an orphan whose commit the relay keeps
// at the ref of the orphan: published here, since nothing more is to be done
// with it. Compare-and-set on the generation, the attempts and the state sealed.
func MarkPreserved(ctx context.Context, tx *Tx, p Publication, at time.Time) error {
	res, err := tx.exec(ctx,
		`UPDATE publications SET state = ?, updated_at = ?
		 WHERE run_id = ? AND lease_epoch = ? AND generation = ? AND attempts = ? AND state = ?`,
		string(PubPublished), at.UnixNano(), p.RunID, p.LeaseEpoch, p.Generation, p.Attempts, string(PubSealed))
	return changedOne(res, err, "preserving an orphan")
}
