-- +goose Up

-- Whether a shell may be opened in the worktree of the Runs of a Project (live-attach).
-- Off unless the owner opts in: the shell has the authority of the OS user of the Worker.
ALTER TABLE projects ADD COLUMN shell INTEGER NOT NULL DEFAULT 0 CHECK (shell IN (0, 1));
