-- +goose Up

-- What the Worker had to discard of logs and output when its journal grew past
-- journal.max_size (§1.3). Reports are never discarded, so a truncation is only
-- ever a range of log lines and a count of output bytes. At most one row is open
-- (sealed = 0): it accumulates what is discarded. A row is sealed when the journal
-- reaches journal.hard_max, and only a sealed row is reported to the Controller
-- (in the heartbeat) and kept until the Controller acknowledges its id.
CREATE TABLE journal_truncations (
    id           INTEGER PRIMARY KEY AUTOINCREMENT,
    sealed       INTEGER NOT NULL DEFAULT 0 CHECK (sealed IN (0, 1)),
    at           INTEGER NOT NULL,
    logs_from    INTEGER NOT NULL DEFAULT 0,
    logs_to      INTEGER NOT NULL DEFAULT 0,
    log_lines    INTEGER NOT NULL DEFAULT 0,
    output_bytes INTEGER NOT NULL DEFAULT 0
) STRICT;
