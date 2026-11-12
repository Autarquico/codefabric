-- +goose Up

-- pushed_commit is the commit the capture pushed to its immutable ref ('' until it did),
-- and pushed_excluded the paths its sealing left out, as JSON: a retry of the capture
-- (the upload of the native state, the report) reuses them instead of sealing the
-- worktree anew, which would give a commit the ref refuses.
ALTER TABLE checkpoint_captures ADD COLUMN pushed_commit TEXT NOT NULL DEFAULT '';
ALTER TABLE checkpoint_captures ADD COLUMN pushed_excluded TEXT NOT NULL DEFAULT '[]';
