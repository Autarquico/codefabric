-- +goose Up

-- What the harness of a Run answered (its final message), reported by the
-- Worker with the exit of the harness, bounded to 8 KiB (NULL until then, or
-- if it said nothing, or if the Worker is older than the report field).
ALTER TABLE runs ADD COLUMN answer TEXT;
