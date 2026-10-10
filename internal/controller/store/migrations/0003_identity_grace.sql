-- +goose Up

-- After a renewal the previous certificate keeps one narrow right until
-- previous_valid_until: renewing again. It lets a client whose renewal
-- response was lost recover with the credentials it still holds.
ALTER TABLE identities ADD COLUMN previous_serial TEXT;
ALTER TABLE identities ADD COLUMN previous_valid_until INTEGER;

CREATE INDEX identities_previous_serial ON identities (previous_serial) WHERE previous_serial IS NOT NULL;
