-- +goose Up

-- What the Worker has sent of the output of a Run (#14). sent is the offset up
-- to which the Controller has acknowledged output.log; dev and ino are the
-- identity of the file it was read from, so that a log that was replaced is not
-- taken for the same one; done says that nothing more is to be sent (the
-- Controller refused the lease, or the output was closed); eof_sent that the
-- Controller acknowledged the end of the output.
CREATE TABLE output_cursors (
    run_id      TEXT    NOT NULL,
    lease_epoch INTEGER NOT NULL CHECK (lease_epoch >= 1),
    sent        INTEGER NOT NULL DEFAULT 0,
    done        INTEGER NOT NULL DEFAULT 0 CHECK (done IN (0, 1)),
    dev         INTEGER,
    ino         INTEGER,
    eof_sent    INTEGER NOT NULL DEFAULT 0 CHECK (eof_sent IN (0, 1)),
    PRIMARY KEY (run_id, lease_epoch)
) STRICT;
