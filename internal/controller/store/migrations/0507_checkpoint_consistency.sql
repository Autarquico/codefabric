-- +goose Up

-- consistency says how the capture was taken: 'quiesced' with the harness
-- stopped (the worktree and the session are of the same instant) or 'live'
-- while it ran (interval and manual captures: the session is cut at its last
-- complete line). The checkpoints made before this column were all quiesced.
ALTER TABLE checkpoints ADD COLUMN consistency TEXT NOT NULL DEFAULT 'quiesced' CHECK (consistency IN ('quiesced', 'live'));
