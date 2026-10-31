-- +goose Up

-- The digest of the configuration the Worker materialised for the Run from its
-- frozen Workflow (treedigest/v1 of the tree without auth, secret markers
-- unrendered), reported with `started`. NULL for a Run without a Workflow, or
-- one that has not started.
ALTER TABLE runs ADD COLUMN effective_config_digest TEXT;
