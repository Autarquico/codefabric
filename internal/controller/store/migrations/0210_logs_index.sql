-- +goose Up

-- Durable output (spec §20.3, §20.5, §34). The output of a Run under a lease
-- epoch is cut into immutable gzip segments in the blob store (CAS); this index
-- says which bytes each one holds. run_output / run_output_chunks stay as the
-- hot tail: the bytes [sealed_offset, end_offset) not yet sealed.
--
-- first_offset and last_offset are the raw (uncompressed) range of the segment,
-- last_offset exclusive; raw_bytes is its length. digest is that of the stored
-- gzip bytes and is also recorded in blobs_meta. The segments of an output are
-- contiguous from run_output.start_offset; rows below it are what the Worker
-- skipped past (a gap restarts the output): kept, never read. The segments are
-- not recorded in blobs_meta: this index is their only reference count.
CREATE TABLE logs_index (
    run_id       TEXT    NOT NULL,
    lease_epoch  INTEGER NOT NULL,
    seg_no       INTEGER NOT NULL CHECK (seg_no >= 0),
    first_offset INTEGER NOT NULL CHECK (first_offset >= 0),
    last_offset  INTEGER NOT NULL,
    raw_bytes    INTEGER NOT NULL,
    digest       TEXT    NOT NULL CHECK (digest LIKE 'sha256:%' AND length(digest) = 71),
    PRIMARY KEY (run_id, lease_epoch, seg_no),
    FOREIGN KEY (run_id, lease_epoch) REFERENCES run_output (run_id, lease_epoch),
    CHECK (last_offset > first_offset AND raw_bytes = last_offset - first_offset)
) STRICT;

CREATE UNIQUE INDEX logs_index_offsets ON logs_index (run_id, lease_epoch, first_offset);

-- Where the sealed bytes end. A ring written before this migration has nothing
-- sealed: its hot tail starts at start_offset, and the Controller seals it at startup.
ALTER TABLE run_output ADD COLUMN sealed_offset INTEGER NOT NULL DEFAULT 0;
UPDATE run_output SET sealed_offset = start_offset;

-- The bytes the Controller has accepted for the output, for the cap per output:
-- they only grow, so a Worker that skips ahead over and over does not get more.
ALTER TABLE run_output ADD COLUMN accepted_bytes INTEGER NOT NULL DEFAULT 0;
UPDATE run_output SET accepted_bytes = end_offset - start_offset;
