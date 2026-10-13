-- +goose Up

-- Observation: attach, ps, events and output (#14).

-- What a Worker announces with its heartbeat for `cf attach`: the ssh target
-- (user@host[:port]) and, as JSON, {"<runtime>": "<absolute path>"}. NULL is
-- not announced. Both are text from a Worker and are validated before they are kept.
ALTER TABLE workers ADD COLUMN ssh_target TEXT;
ALTER TABLE workers ADD COLUMN attach     TEXT;

-- The tail of the output of a Run under a lease epoch: at most 1 MiB kept, in
-- chunks of at most 64 KiB, until the log storage of Phase 4 replaces it. The
-- output is append-only: [start_offset, end_offset) is what is held, and every
-- byte of it is contiguous. closed_at, final_offset and note are the Worker
-- saying that no more will come, where it ends, and why it ended there when the
-- file was replaced or truncated.
CREATE TABLE run_output (
    run_id       TEXT    NOT NULL REFERENCES runs (id),
    lease_epoch  INTEGER NOT NULL CHECK (lease_epoch >= 1),
    start_offset INTEGER NOT NULL CHECK (start_offset >= 0),
    end_offset   INTEGER NOT NULL CHECK (end_offset >= start_offset),
    closed_at    INTEGER,
    final_offset INTEGER,
    note         TEXT,
    updated_at   INTEGER NOT NULL,
    PRIMARY KEY (run_id, lease_epoch),
    CHECK ((closed_at IS NULL) = (final_offset IS NULL))
) STRICT;

CREATE TABLE run_output_chunks (
    run_id       TEXT    NOT NULL,
    lease_epoch  INTEGER NOT NULL,
    chunk_offset INTEGER NOT NULL CHECK (chunk_offset >= 0),
    data         BLOB    NOT NULL CHECK (length(data) BETWEEN 1 AND 65536),
    PRIMARY KEY (run_id, lease_epoch, chunk_offset),
    FOREIGN KEY (run_id, lease_epoch) REFERENCES run_output (run_id, lease_epoch)
) STRICT;

-- GET /v1/events?since filters on the Controller's clock.
CREATE INDEX events_received_at ON events (received_at);
