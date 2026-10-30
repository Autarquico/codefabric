-- +goose Up

-- The model a Run's harness said it ran with, reported by the Worker with the
-- exit of the harness (NULL until then, or if it did not say).
ALTER TABLE runs ADD COLUMN model TEXT;
