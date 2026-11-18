-- +goose Up

-- What the harness answered, kept with the cursor of its structured stream so
-- that the exit report can carry it. answer is the text of the last result the
-- adapter read (bounded, printable). agent_message is the last message of the
-- agent not yet followed by a result: a harness whose result line has no text
-- (Codex) says it a drain before the result may arrive.
ALTER TABLE event_cursors ADD COLUMN answer TEXT NOT NULL DEFAULT '';
ALTER TABLE event_cursors ADD COLUMN agent_message TEXT NOT NULL DEFAULT '';
