package store

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"time"

	"github.com/Autarquico/codefabric/internal/domain"
)

// ErrInvalidIdempotencyKey is returned when a request_id comes without the
// principal or operation that scope it (§27.4).
var ErrInvalidIdempotencyKey = errors.New("invalid idempotency key")

// IdemKey scopes a request_id: two principals may reuse the same ID.
type IdemKey struct {
	Principal string
	Operation string
	RequestID string
}

// SavedResponse looks up the response saved for key (§27.4). found is false
// for a key not seen before and for an empty RequestID, which disables
// idempotency. The same key with another payload fails with
// CF_IDEMPOTENCY_MISMATCH. Looking up, running the mutation and SaveResponse
// must share one transaction, so a crash or an error leaves no key behind and
// a retry executes cleanly.
func (t *Tx) SavedResponse(ctx context.Context, key IdemKey, payload []byte) (response []byte, found bool, err error) {
	if key.RequestID == "" {
		return nil, false, nil
	}
	if key.Principal == "" || key.Operation == "" {
		return nil, false, fmt.Errorf("%w: principal and operation are required with a request_id", ErrInvalidIdempotencyKey)
	}
	payloadHash := hashOf(payload)
	var savedHash string
	err = t.QueryRow(ctx,
		`SELECT payload_hash, response FROM idempotency_keys
		 WHERE principal = ? AND operation = ? AND request_id = ?`,
		key.Principal, key.Operation, key.RequestID).Scan(&savedHash, &response)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return nil, false, nil
	case err != nil:
		return nil, false, fmt.Errorf("store: looking up idempotency key: %w", err)
	case savedHash != payloadHash:
		return nil, false, domain.NewError(domain.CodeIdempotencyMismatch,
			"request_id was already used with a different payload",
			map[string]any{"operation": key.Operation, "request_id": key.RequestID})
	}
	return response, true, nil
}

// SaveResponse records response as the answer of key, for SavedResponse to
// return when the request repeats. It does nothing for an empty RequestID.
func (t *Tx) SaveResponse(ctx context.Context, key IdemKey, payload, response []byte) error {
	if key.RequestID == "" {
		return nil
	}
	if response == nil {
		response = []byte{} // the column is NOT NULL
	}
	_, err := t.Exec(ctx,
		`INSERT INTO idempotency_keys (principal, operation, request_id, payload_hash, response, created_at)
		 VALUES (?, ?, ?, ?, ?, ?)`,
		key.Principal, key.Operation, key.RequestID, hashOf(payload), response, t.db.clock.Now().UnixNano())
	if err != nil {
		return fmt.Errorf("store: saving idempotency key: %w", err)
	}
	return nil
}

func hashOf(payload []byte) string {
	digest := sha256.Sum256(payload)
	return hex.EncodeToString(digest[:])
}

// PurgeIdempotency deletes keys created before olderThan (idempotency.ttl,
// §27.4) and reports how many it removed.
func (db *DB) PurgeIdempotency(ctx context.Context, olderThan time.Time) (int64, error) {
	var purged int64
	err := db.WithTx(ctx, func(tx *Tx) error {
		res, err := tx.Exec(ctx, `DELETE FROM idempotency_keys WHERE created_at < ?`, olderThan.UnixNano())
		if err != nil {
			return err
		}
		purged, err = res.RowsAffected()
		return err
	})
	if err != nil {
		return 0, fmt.Errorf("store: purging idempotency keys: %w", err)
	}
	return purged, nil
}
