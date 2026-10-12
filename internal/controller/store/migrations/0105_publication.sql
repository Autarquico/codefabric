-- +goose Up

-- Publication of the result of a Run (§22.6).
--
-- branch is frozen when the Run is created (domain.BranchOf). It is NULL for the
-- Runs that exist before this migration: whoever reads one derives the same
-- name with domain.BranchOf, so that there is a single rule and no backfill.
-- result_commit is the commit the Worker sealed; once it is set it never changes.
-- result_excluded is JSON ({"paths": [...], "truncated": bool}): the untracked
-- paths the sealing left out, as metadata of the Run.
ALTER TABLE runs ADD COLUMN branch TEXT;
ALTER TABLE runs ADD COLUMN result_commit TEXT;
ALTER TABLE runs ADD COLUMN result_excluded TEXT;

-- A Run that completed has a result to publish or a publication that failed: a
-- completed Run with publication none would be a result nobody looks after.
-- The exit of the Worker sets pending in the statement that completes the Run;
-- this is the database refusing anything else.
-- +goose StatementBegin
CREATE TRIGGER runs_completed_has_publication_upd BEFORE UPDATE OF state, publication ON runs
 WHEN NEW.state = 'completed' AND NEW.publication = 'none'
BEGIN
    SELECT RAISE(ABORT, 'completed run with publication none (§22.6)');
END;
-- +goose StatementEnd
-- +goose StatementBegin
CREATE TRIGGER runs_completed_has_publication_ins BEFORE INSERT ON runs
 WHEN NEW.state = 'completed' AND NEW.publication = 'none'
BEGIN
    SELECT RAISE(ABORT, 'completed run with publication none (§22.6)');
END;
-- +goose StatementEnd

-- At most one publish order is waiting for a Worker for a Run. The CHECK of
-- worker_commands covers assign only; publish carries run, lease and epoch too,
-- which the Go that enqueues it guarantees.
CREATE UNIQUE INDEX worker_commands_publish_open ON worker_commands (run_id)
 WHERE type = 'publish' AND acked_at IS NULL AND cancelled_at IS NULL;
