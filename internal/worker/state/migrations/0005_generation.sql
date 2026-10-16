-- +goose Up

-- The generation of this Worker's daemon (#23): every run of the daemon takes
-- the next one before its first heartbeat, and the Controller refuses the
-- heartbeats of a generation lower than the last it took, so a late heartbeat
-- of a replaced run cannot undo what its successor said. One row, or none
-- before the first run that knows of generations.
CREATE TABLE daemon_generation (
    id         INTEGER NOT NULL PRIMARY KEY CHECK (id = 1),
    generation INTEGER NOT NULL CHECK (generation >= 1)
) STRICT;
