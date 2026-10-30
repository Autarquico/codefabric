-- +goose Up

-- Environments (#24): global, versioned definitions of how to prepare a
-- worktree for a Run (§25.3). A version is immutable; the head of an
-- Environment is its highest version, as for Workflows.
-- id is the name: Environments are never renamed, and a Run pins it as
-- "environment_id".
CREATE TABLE environments (
    id         TEXT    PRIMARY KEY CHECK (length(id) BETWEEN 1 AND 63),
    created_by TEXT    NOT NULL,
    created_at INTEGER NOT NULL
) STRICT, WITHOUT ROWID;

-- definition is the canonical JSON of domain.EnvironmentDef and digest its
-- sha256 ("sha256:<hex>"). The digest is not unique: applying the definition
-- of an earlier version (a revert) is a new version with the same digest;
-- the lookup takes the latest. Only the head is refused as nothing to commit.
-- parent_version is NULL for version 1 and version - 1 after.
CREATE TABLE environment_versions (
    environment_id TEXT    NOT NULL REFERENCES environments (id),
    version        INTEGER NOT NULL CHECK (version >= 1),
    parent_version INTEGER CHECK (parent_version IS NULL OR parent_version = version - 1),
    digest         TEXT    NOT NULL CHECK (digest LIKE 'sha256:%' AND length(digest) = 71),
    definition     TEXT    NOT NULL CHECK (json_valid(definition)),
    author_kind    TEXT    NOT NULL,
    author_id      TEXT    NOT NULL,
    created_at     INTEGER NOT NULL,
    PRIMARY KEY (environment_id, version),
    CHECK ((version = 1) = (parent_version IS NULL))
) STRICT, WITHOUT ROWID;

CREATE INDEX environment_versions_digest ON environment_versions (environment_id, digest);

-- Whether the repo of a Project may carry .codefabric/project.yaml (§3.1).
ALTER TABLE projects ADD COLUMN repo_config TEXT NOT NULL DEFAULT 'allow' CHECK (repo_config IN ('allow', 'deny'));

-- What a Run pinned of its Environment, and what it resolved to. The pin
-- (id, version and digest) is written together by the service; effective_config
-- is the JSON the Worker resolved and effective_digest its digest; failure_code
-- is the code a failed preparation reported. All of them are NULL until set.
ALTER TABLE runs ADD COLUMN environment_id      TEXT REFERENCES environments (id);
ALTER TABLE runs ADD COLUMN environment_version INTEGER CHECK (environment_version IS NULL OR environment_version >= 1);
ALTER TABLE runs ADD COLUMN environment_digest  TEXT;
ALTER TABLE runs ADD COLUMN effective_config    TEXT CHECK (effective_config IS NULL OR json_valid(effective_config));
ALTER TABLE runs ADD COLUMN effective_digest    TEXT;
ALTER TABLE runs ADD COLUMN failure_code        TEXT;
