-- +goose Up

-- The session of a client that seeds a Run (`cf do|task resume --session`,
-- lineage.native_seed): the blob a client uploaded for a Project, before any Run
-- names it. Served only through the seed route of the Run that holds it, never as
-- a plain blob, and kept alive by the blob retention as the native state of a
-- checkpoint is.
CREATE TABLE native_seeds (
    digest      TEXT NOT NULL,
    project_id  TEXT NOT NULL REFERENCES projects (id),
    uploaded_by TEXT NOT NULL,
    created_at  INTEGER NOT NULL,
    PRIMARY KEY (digest, project_id)
);

-- What the Run that carries a seed runs from. All three are NULL for a Run
-- without one.
ALTER TABLE runs ADD COLUMN seed_adapter TEXT;
ALTER TABLE runs ADD COLUMN seed_adapter_version TEXT;
ALTER TABLE runs ADD COLUMN seed_digest TEXT;

-- The marker `cf task pull` leaves on a Task (TaskPulled): the checkout of the user
-- holds the work now, so a resume would make two writers. client is the identity
-- of the certificate that pulled. A seed from that client, or --force, consumes it.
CREATE TABLE task_pulls (
    id          INTEGER PRIMARY KEY,
    task_id     TEXT NOT NULL REFERENCES tasks (id),
    client      TEXT NOT NULL,
    pulled_at   INTEGER NOT NULL,
    consumed_at INTEGER
);
CREATE INDEX task_pulls_task ON task_pulls (task_id, pulled_at);
