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

// Idempotent runs a synchronous mutation at most once per key (§27.4).
//
// Lookup, fn, and the saving of the key and its response share one
// transaction, so a crash or an error leaves no key behind and a retry
// executes cleanly. A repeated key with the same payload returns the saved
// response without calling fn; with a different payload it fails with
// CF_IDEMPOTENCY_MISMATCH. An empty RequestID disables idempotency.
func (db *DB) Idempotent(ctx context.Context, key IdemKey, payload []byte,
	fn func(*Tx) ([]byte, error),
) ([]byte, error) {
	var response []byte
	if key.RequestID == "" {
		err := db.WithTx(ctx, func(tx *Tx) (err error) {
			response, err = fn(tx)
			return err
		})
		return response, err
	}
	if key.Principal == "" || key.Operation == "" {
		return nil, fmt.Errorf("%w: principal and operation are required with a request_id", ErrInvalidIdempotencyKey)
	}

	digest := sha256.Sum256(payload)
	payloadHash := hex.EncodeToString(digest[:])

	err := db.WithTx(ctx, func(tx *Tx) error {
		saved, found, err := tx.savedResponse(ctx, key, payloadHash)
		if err != nil || found {
			response = saved
			return err
		}
		if response, err = fn(tx); err != nil {
			return err
		}
		if response == nil {
			response = []byte{} // the column is NOT NULL
		}
		_, err = tx.Exec(ctx,
			`INSERT INTO idempotency_keys (principal, operation, request_id, payload_hash, response, created_at)
			 VALUES (?, ?, ?, ?, ?, ?)`,
			key.Principal, key.Operation, key.RequestID, payloadHash, response, tx.db.clock.Now().UnixNano())
		if err != nil {
			return fmt.Errorf("store: saving idempotency key: %w", err)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return response, nil
}

func (t *Tx) savedResponse(ctx context.Context, key IdemKey, payloadHash string) (response []byte, found bool, err error) {
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
