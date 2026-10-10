-- +goose Up

-- Timestamps are Unix nanoseconds (UTC). cert_serial is the lowercase hex serial
-- of the identity's current certificate: renewing replaces it, which is what
-- invalidates the previous certificate.
CREATE TABLE identities (
    id          TEXT    PRIMARY KEY,
    kind        TEXT    NOT NULL CHECK (kind IN ('client', 'worker')),
    name        TEXT    NOT NULL,
    cert_serial TEXT    NOT NULL UNIQUE,
    not_after   INTEGER NOT NULL,
    revoked_at  INTEGER,
    created_at  INTEGER NOT NULL
) STRICT;

-- Only sha256(token) is stored, never the token.
CREATE TABLE join_tokens (
    hash        TEXT    PRIMARY KEY,
    kind        TEXT    NOT NULL CHECK (kind IN ('client', 'worker')),
    expires_at  INTEGER NOT NULL,
    used_at     INTEGER,
    identity_id TEXT REFERENCES identities (id),
    created_by  TEXT    NOT NULL,
    created_at  INTEGER NOT NULL
) STRICT;
