-- +goose Up

-- Secrets (#23). The Controller holds refs and metadata about secrets, and
-- never a value, a hash of a value or its length (§16.7): no column below can
-- hold one, and a store test pins every column of these tables. backend and
-- denial are validated in Go, like every enum since 01xx.

-- The allowlist of a Project: the exact refs (no globs) its Runs may use.
CREATE TABLE project_secret_allowlist (
    project_id TEXT    NOT NULL REFERENCES projects (id),
    ref        TEXT    NOT NULL CHECK (length(ref) BETWEEN 1 AND 128),
    added_at   INTEGER NOT NULL,
    added_by   TEXT    NOT NULL,
    PRIMARY KEY (project_id, ref)
) STRICT, WITHOUT ROWID;

-- The latest secrets report taken from each Worker, which fences the next one:
-- a report is taken only if its (generation, seq) is above this one, so a late
-- heartbeat of a replaced daemon never restores what it said. Generation 0 is
-- a daemon that predates generations: between two of those, a new epoch is a
-- new daemon. metadata_digest is the sha256 of the metadata in worker_secrets
-- (refs, backends, scopes and timestamps), NULL when it must be rewritten.
CREATE TABLE worker_secret_reports (
    worker_id       TEXT    PRIMARY KEY REFERENCES workers (id),
    generation      INTEGER NOT NULL CHECK (generation >= 0),
    epoch           TEXT    NOT NULL,
    seq             INTEGER NOT NULL CHECK (seq >= 0),
    metadata_digest TEXT,
    reported_at     INTEGER NOT NULL
) STRICT;

-- The secrets a Worker last said it holds, as metadata. projects is a JSON
-- array of Project IDs the secret is scoped to; [] is any Project.
CREATE TABLE worker_secrets (
    worker_id  TEXT    NOT NULL REFERENCES workers (id),
    ref        TEXT    NOT NULL CHECK (length(ref) BETWEEN 1 AND 128),
    backend    TEXT    NOT NULL,
    projects   TEXT    NOT NULL CHECK (json_valid(projects) AND json_type(projects) = 'array'),
    updated_at INTEGER NOT NULL,
    PRIMARY KEY (worker_id, ref)
) STRICT, WITHOUT ROWID;

CREATE INDEX worker_secrets_ref ON worker_secrets (ref);

-- Every grant a Worker asked for, decided anew each time and kept even when
-- denied: a grant is all or nothing (granted), with the verdict of each ref in
-- secret_grant_refs (denial NULL: the ref itself passed). Each ref also has an
-- audit entry written in the same transaction.
CREATE TABLE secret_grants (
    id           INTEGER PRIMARY KEY AUTOINCREMENT,
    run_id       TEXT    NOT NULL REFERENCES runs (id),
    lease_id     TEXT    NOT NULL REFERENCES leases (id),
    lease_epoch  INTEGER NOT NULL CHECK (lease_epoch >= 1),
    worker_id    TEXT    NOT NULL REFERENCES workers (id),
    worker_epoch TEXT    NOT NULL,
    granted      INTEGER NOT NULL CHECK (granted IN (0, 1)),
    decided_at   INTEGER NOT NULL
) STRICT;

CREATE INDEX secret_grants_run_id ON secret_grants (run_id);

CREATE TABLE secret_grant_refs (
    grant_id INTEGER NOT NULL REFERENCES secret_grants (id),
    ref      TEXT    NOT NULL CHECK (length(ref) BETWEEN 1 AND 128),
    denial   TEXT,
    PRIMARY KEY (grant_id, ref)
) STRICT, WITHOUT ROWID;
