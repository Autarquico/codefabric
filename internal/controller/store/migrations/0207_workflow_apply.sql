-- +goose Up

-- Applying a Workflow to a Worker is a durable operation (§8.6, §27.4): one row
-- per order. Per Worker and harness at most one real order and one dry run are
-- alive (pending or running), each in its own lane: a new order supersedes the
-- live one of its lane (a real one also a pending dry run) and raises generation, which fences the
-- result of the older order out (a late report from a superseded order is
-- ignored). components is a JSON array of component names, NULL for all of
-- the harness. result is what the Worker reported (diff, errors) as JSON.
CREATE TABLE workflow_applies (
    id          TEXT    PRIMARY KEY,
    worker_id   TEXT    NOT NULL REFERENCES workers (id),
    harness     TEXT    NOT NULL,
    workflow_id TEXT    NOT NULL REFERENCES workflows (id),
    version     INTEGER NOT NULL CHECK (version >= 1),
    digest      TEXT    NOT NULL,
    components  TEXT    CHECK (components IS NULL OR (json_valid(components) AND json_type(components) = 'array')),
    mode        TEXT    NOT NULL CHECK (mode IN ('apply', 'rollback')),
    forced      INTEGER NOT NULL CHECK (forced IN (0, 1)),
    dry_run     INTEGER NOT NULL CHECK (dry_run IN (0, 1)),
    expected_preview TEXT NOT NULL DEFAULT '',
    generation  INTEGER NOT NULL CHECK (generation >= 1),
    state       TEXT    NOT NULL CHECK (state IN ('pending', 'running', 'applied', 'failed', 'superseded')),
    result      TEXT    CHECK (result IS NULL OR json_valid(result)),
    created_at  INTEGER NOT NULL,
    updated_at  INTEGER NOT NULL
) STRICT;

CREATE UNIQUE INDEX workflow_applies_generation ON workflow_applies (worker_id, harness, generation);
CREATE UNIQUE INDEX workflow_applies_live ON workflow_applies (worker_id, harness, dry_run) WHERE state IN ('pending', 'running');

-- What the Controller knows a Worker holds of a harness: the version last
-- applied (not a dry-run), the one before it for rollback (0 = none), the
-- components of that apply (NULL = all), the drift the Worker reported and
-- the components still not applied because they were skipped for drift (a
-- JSON array; non-empty = partial).
CREATE TABLE worker_workflow_state (
    worker_id           TEXT    NOT NULL REFERENCES workers (id),
    harness             TEXT    NOT NULL,
    applied_workflow_id TEXT    REFERENCES workflows (id),
    applied_version     INTEGER NOT NULL DEFAULT 0 CHECK (applied_version >= 0),
    applied_digest      TEXT    NOT NULL DEFAULT '',
    components          TEXT    CHECK (components IS NULL OR (json_valid(components) AND json_type(components) = 'array')),
    previous_version    INTEGER NOT NULL DEFAULT 0 CHECK (previous_version >= 0),
    drift               TEXT    NOT NULL DEFAULT '[]' CHECK (json_valid(drift) AND json_type(drift) = 'array'),
    skipped             TEXT    NOT NULL DEFAULT '[]' CHECK (json_valid(skipped) AND json_type(skipped) = 'array'),
    updated_at          INTEGER NOT NULL,
    PRIMARY KEY (worker_id, harness)
) STRICT, WITHOUT ROWID;
