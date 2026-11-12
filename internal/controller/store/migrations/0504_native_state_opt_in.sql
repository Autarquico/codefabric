-- +goose Up

-- Whether the checkpoints of a Project capture the harness's own state (§23.1).
-- Off unless the Project opts in: the native state may hold what the harness
-- was shown, so it is the owner's decision.
ALTER TABLE projects ADD COLUMN native_state INTEGER NOT NULL DEFAULT 0 CHECK (native_state IN (0, 1));

-- What the Worker warned about while capturing (a secret in the session, say): a JSON
-- array of short texts, which the user reads next to the checkpoint.
ALTER TABLE checkpoints ADD COLUMN warnings TEXT NOT NULL DEFAULT '[]';
