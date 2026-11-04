-- +goose Up

-- The ref of the secret that authenticates the fetch of a Project's origin
-- (§16.7, ADR 0002 D1/D3). NULL when the repo is fetched without credentials.
-- It is policy over the allowlist of the Project, not a copy of it: a Run
-- freezes the ref and the origin URL in its spec when it is created.
ALTER TABLE projects ADD COLUMN origin_secret TEXT;
