package state

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// Resolution is the configuration a Run resolved from its repo, and the seq of
// the journal row that reports it.
type Resolution struct {
	Effective  []byte
	Digest     string
	JournalSeq uint64
}

// InsertResolution stores the resolution of an assignment, once: a second one
// is ErrChanged.
func InsertResolution(ctx context.Context, tx *Tx, k Key, r Resolution, at time.Time) error {
	res, err := tx.exec(ctx,
		`INSERT OR IGNORE INTO config_resolutions (run_id, lease_epoch, effective, digest, journal_seq, created_at) VALUES (?, ?, ?, ?, ?, ?)`,
		k.RunID, k.LeaseEpoch, string(r.Effective), r.Digest, r.JournalSeq, at.UnixNano())
	return changedOne(res, err, "storing the resolution of an assignment")
}

// ResolutionOf reads the resolution of an assignment; ok is false if it has none.
func ResolutionOf(ctx context.Context, q Querier, k Key) (r Resolution, ok bool, err error) {
	var effective string
	err = q.QueryRowContext(ctx, `SELECT effective, digest, journal_seq FROM config_resolutions WHERE run_id = ? AND lease_epoch = ?`,
		k.RunID, k.LeaseEpoch).Scan(&effective, &r.Digest, &r.JournalSeq)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return Resolution{}, false, nil
	case err != nil:
		return Resolution{}, false, fmt.Errorf("state: reading the resolution of an assignment: %w", err)
	}
	r.Effective = []byte(effective)
	return r, true, nil
}
