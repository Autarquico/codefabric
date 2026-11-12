-- +goose Up

-- session_id is the native session of the harness of a Run, as Launch.SessionID
-- said it when the Run was prepared. It is given back in Handle.SessionID, so a
-- checkpoint exports that session and does not guess it.
ALTER TABLE local_runs ADD COLUMN session_id TEXT;

-- checkpoint_captures is the capture the Controller ordered of a Run: it is
-- written, with the phase `checkpointing`, in the transaction that takes the
-- order, so a daemon that dies knows on restart what it was doing. The Run is
-- not settled (settle removes its ConfigDir) until the capture is over: done_at
-- is set, with the outcome, by reporting it or by aborting it.
CREATE TABLE checkpoint_captures (
    run_id        TEXT    NOT NULL,
    lease_epoch   INTEGER NOT NULL,
    checkpoint_id TEXT    NOT NULL,
    trigger       TEXT    NOT NULL,
    ref           TEXT    NOT NULL,
    exclude       TEXT    NOT NULL,
    created_at    INTEGER NOT NULL,
    done_at       INTEGER,
    outcome       TEXT,
    PRIMARY KEY (run_id, lease_epoch)
) WITHOUT ROWID;
