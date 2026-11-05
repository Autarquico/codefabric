-- +goose Up

-- The questions a Run's harness asked and the answers ordered for them
-- (needs-input). A prompt is written in the transaction of its input_requested
-- report, once: its ID is the harness's (a tool use) or the bridge's, so that
-- the same ask read twice, or asked again after the daemon restarted, finds its
-- row. It is open until the answer reached the harness (acknowledged), or
-- until the harness stopped waiting for it (withdrawn).
CREATE TABLE input_prompts (
    run_id      TEXT    NOT NULL,
    lease_epoch INTEGER NOT NULL CHECK (lease_epoch >= 1),
    prompt_id   TEXT    NOT NULL,
    kind        TEXT    NOT NULL CHECK (kind IN ('permission', 'question', 'text')),
    state       TEXT    NOT NULL CHECK (state IN ('open', 'acknowledged', 'withdrawn')),
    created_at  INTEGER NOT NULL,
    PRIMARY KEY (run_id, lease_epoch, prompt_id)
) STRICT;

-- The `input` orders of the Controller, stored with the cursor of the queue.
-- state: queued; delivering (the answer is being written to the FIFO of the
-- harness: written at most once, so an order found delivering after a restart
-- is taken as delivered); done; ignored (its Run was over, or the lease was not
-- the live one, or the prompt unknown).
CREATE TABLE input_orders (
    command_seq INTEGER PRIMARY KEY,
    run_id      TEXT    NOT NULL,
    lease_epoch INTEGER NOT NULL,
    lease_id    TEXT    NOT NULL,
    prompt_id   TEXT    NOT NULL,
    kind        TEXT    NOT NULL,
    decision    TEXT    NOT NULL DEFAULT '',
    answer_text TEXT    NOT NULL DEFAULT '',
    state       TEXT    NOT NULL CHECK (state IN ('queued', 'delivering', 'done', 'ignored')),
    created_at  INTEGER NOT NULL
) STRICT;

CREATE INDEX input_orders_run ON input_orders (run_id, lease_epoch, state);
