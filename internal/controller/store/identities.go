package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/Autarquico/codefabric/internal/domain"
)

// ErrNotFound is returned when a lookup matches no row.
var ErrNotFound = errors.New("store: not found")

// Identity is a machine allowed to call the API (§16.6). RevokedAt is the zero
// time while the identity is valid.
type Identity struct {
	ID         domain.ID
	Kind       domain.Kind
	Name       string
	CertSerial string
	NotAfter   time.Time
	CreatedAt  time.Time
	RevokedAt  time.Time
}

// Revoked reports whether the identity was revoked.
func (i Identity) Revoked() bool { return !i.RevokedAt.IsZero() }

const identityColumns = `id, kind, name, cert_serial, not_after, created_at, COALESCE(revoked_at, 0)`

type rowScanner interface{ Scan(dest ...any) error }

func scanIdentity(row rowScanner) (Identity, error) {
	var (
		i                          Identity
		id, kind                   string
		notAfter, created, revoked int64
	)
	if err := row.Scan(&id, &kind, &i.Name, &i.CertSerial, &notAfter, &created, &revoked); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return Identity{}, ErrNotFound
		}
		return Identity{}, fmt.Errorf("store: scanning identity: %w", err)
	}
	i.ID, i.Kind = domain.ID(id), domain.Kind(kind)
	i.NotAfter, i.CreatedAt = time.Unix(0, notAfter).UTC(), time.Unix(0, created).UTC()
	if revoked != 0 {
		i.RevokedAt = time.Unix(0, revoked).UTC()
	}
	return i, nil
}

// IdentityBySerial finds the identity whose current certificate has this serial.
func (db *DB) IdentityBySerial(ctx context.Context, serial string) (Identity, error) {
	return scanIdentity(db.reader.QueryRowContext(ctx,
		`SELECT `+identityColumns+` FROM identities WHERE cert_serial = ?`, serial))
}

// Identities lists the identities of one kind, oldest first.
func (db *DB) Identities(ctx context.Context, kind domain.Kind) ([]Identity, error) {
	rows, err := db.reader.QueryContext(ctx,
		`SELECT `+identityColumns+` FROM identities WHERE kind = ? ORDER BY created_at, id`, string(kind))
	if err != nil {
		return nil, fmt.Errorf("store: listing identities: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []Identity
	for rows.Next() {
		i, err := scanIdentity(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, i)
	}
	return out, rows.Err()
}

// IdentityByID reads an identity inside the transaction.
func (t *Tx) IdentityByID(ctx context.Context, id domain.ID) (Identity, error) {
	return scanIdentity(t.QueryRow(ctx, `SELECT `+identityColumns+` FROM identities WHERE id = ?`, string(id)))
}

// InsertIdentity stores a new identity.
func (t *Tx) InsertIdentity(ctx context.Context, i Identity) error {
	_, err := t.Exec(ctx,
		`INSERT INTO identities (id, kind, name, cert_serial, not_after, created_at) VALUES (?, ?, ?, ?, ?, ?)`,
		string(i.ID), string(i.Kind), i.Name, i.CertSerial, i.NotAfter.UnixNano(), i.CreatedAt.UnixNano())
	if err != nil {
		return fmt.Errorf("store: inserting identity: %w", err)
	}
	return nil
}

// ReplaceCertificate swaps the certificate of a non-revoked identity, but only
// while it still holds oldSerial. It reports whether the swap happened; the
// previous certificate stops authenticating the moment it does.
func (t *Tx) ReplaceCertificate(ctx context.Context, id domain.ID, oldSerial, newSerial string, notAfter time.Time) (bool, error) {
	res, err := t.Exec(ctx,
		`UPDATE identities SET cert_serial = ?, not_after = ?
		 WHERE id = ? AND cert_serial = ? AND revoked_at IS NULL`,
		newSerial, notAfter.UnixNano(), string(id), oldSerial)
	if err != nil {
		return false, fmt.Errorf("store: replacing certificate: %w", err)
	}
	return rowsChanged(res)
}

// Revoke marks a non-revoked identity of the given kind as revoked.
func (t *Tx) Revoke(ctx context.Context, id domain.ID, kind domain.Kind, at time.Time) error {
	_, err := t.Exec(ctx,
		`UPDATE identities SET revoked_at = ? WHERE id = ? AND kind = ? AND revoked_at IS NULL`,
		at.UnixNano(), string(id), string(kind))
	if err != nil {
		return fmt.Errorf("store: revoking identity: %w", err)
	}
	return nil
}

// CountUsableIdentities counts the identities of one kind that can still
// authenticate at now: not revoked and not expired.
func (t *Tx) CountUsableIdentities(ctx context.Context, kind domain.Kind, now time.Time) (int, error) {
	var n int
	err := t.QueryRow(ctx,
		`SELECT COUNT(*) FROM identities WHERE kind = ? AND revoked_at IS NULL AND not_after > ?`,
		string(kind), now.UnixNano()).Scan(&n)
	if err != nil {
		return 0, fmt.Errorf("store: counting identities: %w", err)
	}
	return n, nil
}

// InsertJoinToken stores the hash of a freshly issued token.
func (t *Tx) InsertJoinToken(ctx context.Context, hash string, kind domain.Kind, createdBy string, created, expires time.Time) error {
	_, err := t.Exec(ctx,
		`INSERT INTO join_tokens (hash, kind, expires_at, created_by, created_at) VALUES (?, ?, ?, ?, ?)`,
		hash, string(kind), expires.UnixNano(), createdBy, created.UnixNano())
	if err != nil {
		return fmt.Errorf("store: inserting join token: %w", err)
	}
	return nil
}

// ConsumeJoinToken marks an unused, unexpired token as used and returns its
// kind. ok is false when the token is unknown, used or expired. The single
// conditional UPDATE is what makes concurrent redemptions pick one winner.
func (t *Tx) ConsumeJoinToken(ctx context.Context, hash string, now time.Time) (kind domain.Kind, ok bool, err error) {
	var k string
	err = t.QueryRow(ctx,
		`UPDATE join_tokens SET used_at = ?
		 WHERE hash = ? AND used_at IS NULL AND expires_at > ?
		 RETURNING kind`, now.UnixNano(), hash, now.UnixNano()).Scan(&k)
	if errors.Is(err, sql.ErrNoRows) {
		return "", false, nil
	}
	if err != nil {
		return "", false, fmt.Errorf("store: consuming join token: %w", err)
	}
	return domain.Kind(k), true, nil
}

// LinkJoinToken records which identity a consumed token created.
func (t *Tx) LinkJoinToken(ctx context.Context, hash string, id domain.ID) error {
	if _, err := t.Exec(ctx, `UPDATE join_tokens SET identity_id = ? WHERE hash = ?`, string(id), hash); err != nil {
		return fmt.Errorf("store: linking join token: %w", err)
	}
	return nil
}

func rowsChanged(res sql.Result) (bool, error) {
	n, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("store: rows affected: %w", err)
	}
	return n > 0, nil
}
