package state

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// SetConfigDigest stores the digest of the configuration materialised for an
// assignment, replacing the one of an earlier prepare of it.
func SetConfigDigest(ctx context.Context, tx *Tx, k Key, digest string, at time.Time) error {
	res, err := tx.exec(ctx,
		`INSERT OR REPLACE INTO config_digests (run_id, lease_epoch, digest, created_at) VALUES (?, ?, ?, ?)`,
		k.RunID, k.LeaseEpoch, digest, at.UnixNano())
	return changedOne(res, err, "storing the config digest of an assignment")
}

// ConfigDigestOf reads the digest stored for an assignment; ok is false if
// there is none.
func ConfigDigestOf(ctx context.Context, q Querier, k Key) (digest string, ok bool, err error) {
	err = q.QueryRowContext(ctx, `SELECT digest FROM config_digests WHERE run_id = ? AND lease_epoch = ?`,
		k.RunID, k.LeaseEpoch).Scan(&digest)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return "", false, nil
	case err != nil:
		return "", false, fmt.Errorf("state: reading the config digest of an assignment: %w", err)
	}
	return digest, true, nil
}
