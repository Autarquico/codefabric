-- +goose Up

-- The MCP session behind an actor of kind 'mcp' (§15, §16): the ID a `cf mcp
-- serve` process declared. NULL for every other actor and for the rows that
-- existed, which keep their actor_kind and actor_id as they were.
ALTER TABLE events ADD COLUMN actor_session TEXT;
ALTER TABLE audit_entries ADD COLUMN actor_session TEXT;
