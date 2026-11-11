-- +goose Up

-- Where a Run in `checkpointing` returns to when the capture is kept or fails
-- with the harness alive (§11.7): running or waiting_input. NULL in any other state.
ALTER TABLE runs ADD COLUMN checkpoint_origin TEXT;

-- A checkpoint row is reserved when the capture is requested (the Worker needs it
-- to get a token for its ref) and published when the Worker reports it done.
-- NULL while it is only reserved; a reservation is dropped if the capture is
-- aborted, a published checkpoint never is.
ALTER TABLE checkpoints ADD COLUMN published_at INTEGER;

-- The digest of the native state a Worker uploaded for a reservation: what
-- Complete accepts as the native state of the capture. The Worker that holds
-- the Run is the only one that sets it, and a second upload replaces the first.
ALTER TABLE checkpoints ADD COLUMN uploaded_digest TEXT;
