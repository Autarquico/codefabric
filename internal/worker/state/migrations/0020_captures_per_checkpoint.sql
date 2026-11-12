-- +goose Up

-- A Run that is captured while it runs (interval, manual) is captured again:
-- the capture is of the checkpoint, not of the assignment, so the key gains the
-- checkpoint. The reader takes the open capture first, then the latest.
CREATE TABLE checkpoint_captures_new (
    run_id        TEXT    NOT NULL,
    lease_epoch   INTEGER NOT NULL,
    checkpoint_id TEXT    NOT NULL,
    trigger       TEXT    NOT NULL,
    ref           TEXT    NOT NULL,
    exclude       TEXT    NOT NULL,
    created_at    INTEGER NOT NULL,
    done_at       INTEGER,
    outcome       TEXT,
    native_state  INTEGER NOT NULL DEFAULT 0,
    PRIMARY KEY (run_id, lease_epoch, checkpoint_id)
) WITHOUT ROWID;
INSERT INTO checkpoint_captures_new (run_id, lease_epoch, checkpoint_id, trigger, ref, exclude, created_at, done_at, outcome, native_state)
    SELECT run_id, lease_epoch, checkpoint_id, trigger, ref, exclude, created_at, done_at, outcome, native_state FROM checkpoint_captures;
DROP TABLE checkpoint_captures;
ALTER TABLE checkpoint_captures_new RENAME TO checkpoint_captures;
