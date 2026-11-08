-- +goose Up

-- The ranges of an output that the Worker discarded and will never send (its
-- journal grew past journal.max_size, §1.3), between bytes the Controller holds.
-- A gap never overlaps or touches another. A reader jumps over it and says so.
CREATE TABLE run_output_gaps (
    run_id      TEXT    NOT NULL,
    lease_epoch INTEGER NOT NULL,
    from_offset INTEGER NOT NULL CHECK (from_offset >= 0),
    to_offset   INTEGER NOT NULL CHECK (to_offset > from_offset),
    PRIMARY KEY (run_id, lease_epoch, from_offset),
    FOREIGN KEY (run_id, lease_epoch) REFERENCES run_output (run_id, lease_epoch)
) STRICT;
