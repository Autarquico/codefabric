-- +goose Up

-- Stop, cancel, lost and reconciliation (§10.4, §10.5, §10.7, §11.7, §27).

-- §11.7: the Run a retry repeats; NULL for the Runs that repeat none.
ALTER TABLE runs ADD COLUMN lineage_parent_id TEXT REFERENCES runs (id);

-- §10.5 / §22.6: whether the harness of the Run was launched. NULL is unknown,
-- 0 is known not to (prepare_failed), 1 is known to (exited). The reason a Run
-- ended (cancelled, lost, worktree_lost) never sets it: only the Worker knows.
ALTER TABLE runs ADD COLUMN harness_ran INTEGER CHECK (harness_ran IN (0, 1));

-- §27.3: a Worker reconciled. reconcile_started_at is when its latest round
-- began; reconciled_epoch and reconciled_at (Controller clock) are written only
-- by a complete round, one that left no lease unaccounted for.
ALTER TABLE workers ADD COLUMN reconcile_started_at INTEGER;
ALTER TABLE workers ADD COLUMN reconciled_epoch TEXT;
ALTER TABLE workers ADD COLUMN reconciled_at INTEGER;

-- §10.7: a stop is ordered once per Worker, Run and lease epoch, whatever its
-- reason (the first stands), and delivered until the Worker acknowledges it.
CREATE UNIQUE INDEX worker_commands_stop_once ON worker_commands (worker_id, run_id, epoch) WHERE type = 'stop';
-- +goose StatementBegin
CREATE TRIGGER worker_commands_stop_names_run BEFORE INSERT ON worker_commands
 WHEN NEW.type = 'stop' AND (NEW.run_id IS NULL OR NEW.lease_id IS NULL OR NEW.epoch IS NULL)
BEGIN
    SELECT RAISE(ABORT, 'a stop names its Run, lease and epoch (§10.7)');
END;
-- +goose StatementEnd

-- §10.4 point 5: an orphan detected, once per Worker, Run and lease epoch.
-- RunOrphaned is written when the row is inserted, apart from the stop, which
-- may exist already (a lost Run). preserved is where its result is kept:
-- worker (its worktree and private ref) or relay (refs/cf/orphans/<run>-e<epoch>),
-- with the commit.
CREATE TABLE run_orphans (
    worker_id      TEXT    NOT NULL REFERENCES workers (id),
    run_id         TEXT    NOT NULL REFERENCES runs (id),
    lease_epoch    INTEGER NOT NULL CHECK (lease_epoch >= 1),
    detected_at    INTEGER NOT NULL,
    preserved      TEXT    NOT NULL DEFAULT 'worker' CHECK (preserved IN ('worker', 'relay')),
    result_commit  TEXT,
    preserved_at   INTEGER,
    PRIMARY KEY (worker_id, run_id, lease_epoch),
    CHECK ((preserved = 'relay') = (preserved_at IS NOT NULL))
) STRICT, WITHOUT ROWID;

-- §27.4 (dedupe): every report entry the Controller applied, by its triple,
-- with the Run, the kind and the digest of what it said. A report that is
-- applied without an event of its own (one that tells what the Controller knew
-- already) is found here when it comes again, as a duplicate, and a triple
-- reused for another report is refused.
CREATE TABLE report_receipts (
    worker_id    TEXT    NOT NULL REFERENCES workers (id),
    worker_epoch TEXT    NOT NULL,
    worker_seq   INTEGER NOT NULL,
    run_id       TEXT    NOT NULL REFERENCES runs (id),
    kind         TEXT    NOT NULL,
    digest       TEXT    NOT NULL,
    applied_at   INTEGER NOT NULL,
    PRIMARY KEY (worker_id, worker_epoch, worker_seq)
) STRICT, WITHOUT ROWID;

-- The lost sweeper reads the Runs that hold an accepted lease and have not ended.
CREATE INDEX runs_live_accepted ON runs (worker_id) WHERE state IN ('preparing', 'running', 'waiting_input', 'checkpointing');

-- Upgrade from git-relay-publication (#12, B5): a Run cancelled after its
-- session started kept publication none, and its result is sealed by nobody.
-- Only the unambiguous ones become pending: still with runtime active (no
-- RunStopped yet) and not failed in preparation. Those whose runtime is
-- stopped already are left for the reconciliation of their Worker, which
-- knows whether the harness ran.
UPDATE runs SET publication = 'pending'
 WHERE state IN ('cancelled', 'lost') AND publication = 'none' AND started_at IS NOT NULL
   AND runtime = 'active' AND COALESCE(end_reason, '') <> 'environment';
