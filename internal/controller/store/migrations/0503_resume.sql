-- +goose Up

-- The checkpoint a Run of lineage `continues` starts from (§11.7 resume): the
-- Worker prepares its source from the relay at that checkpoint and imports the
-- native state it holds. NULL for every other Run.
ALTER TABLE runs ADD COLUMN lineage_checkpoint_id TEXT REFERENCES checkpoints (id);

-- The name of the Worker a resume chose (`cf task resume --worker`): the
-- scheduler offers the Run to that Worker only. NULL when the user chose none.
ALTER TABLE runs ADD COLUMN pinned_worker TEXT;
