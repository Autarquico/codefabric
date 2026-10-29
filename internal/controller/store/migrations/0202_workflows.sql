-- +goose Up

-- Workflows (#21): versioned compositions of native harness configuration
-- (§8.9). A version is immutable; the head of a Workflow is its highest
-- version, so there is no pointer to keep in step with the versions.
CREATE TABLE workflows (
    id         TEXT    PRIMARY KEY,
    name       TEXT    NOT NULL UNIQUE CHECK (length(name) BETWEEN 1 AND 63),
    created_by TEXT    NOT NULL,
    created_at INTEGER NOT NULL
) STRICT;

-- manifest is the canonical JSON of domain.Manifest and digest its sha256
-- ("sha256:<hex>"); everything else is version metadata, outside the digest.
-- parent_version is NULL for version 1 and version - 1 after: a version is
-- only ever written on top of the head, and UNIQUE (workflow_id, version) is
-- what makes two commits against the same head lose all but one.
-- changed is a JSON array of {harness, name, change} against the parent
-- (change: added, modified or removed), kept so that the history lists the
-- components a version touched without reading any manifest. origin is
-- "client" or the ID of the Worker that took the snapshot.
CREATE TABLE workflow_versions (
    workflow_id    TEXT    NOT NULL REFERENCES workflows (id),
    version        INTEGER NOT NULL CHECK (version >= 1),
    parent_version INTEGER CHECK (parent_version IS NULL OR parent_version = version - 1),
    digest         TEXT    NOT NULL CHECK (digest LIKE 'sha256:%' AND length(digest) = 71),
    manifest       TEXT    NOT NULL CHECK (json_valid(manifest)),
    author_kind    TEXT    NOT NULL,
    author_id      TEXT    NOT NULL,
    origin         TEXT    NOT NULL,
    changelog      TEXT,
    changed        TEXT    NOT NULL CHECK (json_valid(changed) AND json_type(changed) = 'array'),
    created_at     INTEGER NOT NULL,
    PRIMARY KEY (workflow_id, version),
    CHECK ((version = 1) = (parent_version IS NULL))
) STRICT, WITHOUT ROWID;

-- Lookup by digest. Not unique: committing the content of an earlier version
-- (a revert) is a new version with the same digest; the lookup takes the latest.
CREATE INDEX workflow_versions_digest ON workflow_versions (workflow_id, digest);

-- What was uploaded to the blob store (CAS), by digest: for presence checks
-- and for collecting orphans later. The content is not here.
CREATE TABLE blobs_meta (
    digest     TEXT    PRIMARY KEY CHECK (digest LIKE 'sha256:%' AND length(digest) = 71),
    size       INTEGER NOT NULL CHECK (size >= 0),
    created_at INTEGER NOT NULL
) STRICT, WITHOUT ROWID;

-- The pin of a Run to the Workflow version it runs (§8.9, "Congelación"): all
-- three or none (written together by the service; ALTER cannot add a table CHECK).
ALTER TABLE runs ADD COLUMN workflow_id      TEXT REFERENCES workflows (id);
ALTER TABLE runs ADD COLUMN workflow_version INTEGER CHECK (workflow_version IS NULL OR workflow_version >= 1);
ALTER TABLE runs ADD COLUMN workflow_digest  TEXT;
