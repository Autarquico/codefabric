-- +goose Up

-- Search (§37). search_docs says what each entry is and where it belongs;
-- search_index is the FTS5 text, with the same rowid. Both are written by
-- triggers on the entity tables, so every mutation path, present or future,
-- indexes in the transaction of the change itself. Only names and descriptive
-- text are indexed: never secrets, values, or environment contents.
-- project_id is the Project an entry belongs to ('' for Workers and Workflows,
-- which are global; a Project's own ID for the Project). archived covers an
-- archived Task and a removed Worker: kept so a canonical ID still finds them,
-- left out of text matches.
CREATE TABLE search_docs (
    rowid      INTEGER PRIMARY KEY,
    kind       TEXT    NOT NULL,
    id         TEXT    NOT NULL UNIQUE,
    short_id   TEXT    NOT NULL,
    project_id TEXT    NOT NULL,
    archived   INTEGER NOT NULL CHECK (archived IN (0, 1))
) STRICT;
CREATE INDEX search_docs_project ON search_docs (project_id);

CREATE VIRTUAL TABLE search_index USING fts5 (name, body, tokenize = 'unicode61 remove_diacritics 2');

-- project
INSERT INTO search_docs (kind, id, short_id, project_id, archived)
SELECT 'project', x.id, 'project-' || x.short_n, x.id, 0 FROM projects x;
INSERT INTO search_index (rowid, name, body)
SELECT s.rowid, x.name, x.repo || ' ' || x.default_branch FROM projects x JOIN search_docs s ON s.id = x.id;
-- +goose StatementBegin
CREATE TRIGGER search_projects_ins AFTER INSERT ON projects
BEGIN
    INSERT INTO search_docs (kind, id, short_id, project_id, archived)
    VALUES ('project', NEW.id, 'project-' || NEW.short_n, NEW.id, 0);
    INSERT INTO search_index (rowid, name, body)
    VALUES ((SELECT rowid FROM search_docs WHERE id = NEW.id), NEW.name, NEW.repo || ' ' || NEW.default_branch);
END;
-- +goose StatementEnd
-- +goose StatementBegin
CREATE TRIGGER search_projects_upd AFTER UPDATE OF name, repo, default_branch ON projects
BEGIN
    UPDATE search_index SET name = NEW.name, body = NEW.repo || ' ' || NEW.default_branch
     WHERE rowid = (SELECT rowid FROM search_docs WHERE id = NEW.id);
END;
-- +goose StatementEnd
-- +goose StatementBegin
CREATE TRIGGER search_projects_del AFTER DELETE ON projects
BEGIN
    DELETE FROM search_index WHERE rowid = (SELECT rowid FROM search_docs WHERE id = OLD.id);
    DELETE FROM search_docs WHERE id = OLD.id;
END;
-- +goose StatementEnd

-- task
INSERT INTO search_docs (kind, id, short_id, project_id, archived)
SELECT 'task', x.id, 'task-' || x.short_n, x.project_id, x.archived_at IS NOT NULL FROM tasks x;
INSERT INTO search_index (rowid, name, body)
SELECT s.rowid, x.name, x.prompt FROM tasks x JOIN search_docs s ON s.id = x.id;
-- +goose StatementBegin
CREATE TRIGGER search_tasks_ins AFTER INSERT ON tasks
BEGIN
    INSERT INTO search_docs (kind, id, short_id, project_id, archived)
    VALUES ('task', NEW.id, 'task-' || NEW.short_n, NEW.project_id, NEW.archived_at IS NOT NULL);
    INSERT INTO search_index (rowid, name, body)
    VALUES ((SELECT rowid FROM search_docs WHERE id = NEW.id), NEW.name, NEW.prompt);
END;
-- +goose StatementEnd
-- +goose StatementBegin
CREATE TRIGGER search_tasks_upd AFTER UPDATE OF name, prompt, project_id, archived_at ON tasks
BEGIN
    UPDATE search_docs SET project_id = NEW.project_id, archived = NEW.archived_at IS NOT NULL WHERE id = NEW.id;
    UPDATE search_index SET name = NEW.name, body = NEW.prompt
     WHERE rowid = (SELECT rowid FROM search_docs WHERE id = NEW.id);
END;
-- +goose StatementEnd
-- +goose StatementBegin
CREATE TRIGGER search_tasks_del AFTER DELETE ON tasks
BEGIN
    DELETE FROM search_index WHERE rowid = (SELECT rowid FROM search_docs WHERE id = OLD.id);
    DELETE FROM search_docs WHERE id = OLD.id;
END;
-- +goose StatementEnd

-- run
INSERT INTO search_docs (kind, id, short_id, project_id, archived)
SELECT 'run', x.id, 'run-' || x.short_n, COALESCE((SELECT t.project_id FROM tasks t WHERE t.id = x.task_id), ''), 0 FROM runs x;
INSERT INTO search_index (rowid, name, body)
SELECT s.rowid, COALESCE((SELECT t.name FROM tasks t WHERE t.id = x.task_id), '') || ' #' || x.attempt, x.state || ' ' || COALESCE(x.end_reason, '') || ' ' || COALESCE(x.failure_code, '') || ' ' || COALESCE(x.branch, '') || ' ' || COALESCE(x.model, '') FROM runs x JOIN search_docs s ON s.id = x.id;
-- +goose StatementBegin
CREATE TRIGGER search_runs_ins AFTER INSERT ON runs
BEGIN
    INSERT INTO search_docs (kind, id, short_id, project_id, archived)
    VALUES ('run', NEW.id, 'run-' || NEW.short_n, COALESCE((SELECT t.project_id FROM tasks t WHERE t.id = NEW.task_id), ''), 0);
    INSERT INTO search_index (rowid, name, body)
    VALUES ((SELECT rowid FROM search_docs WHERE id = NEW.id), COALESCE((SELECT t.name FROM tasks t WHERE t.id = NEW.task_id), '') || ' #' || NEW.attempt, NEW.state || ' ' || COALESCE(NEW.end_reason, '') || ' ' || COALESCE(NEW.failure_code, '') || ' ' || COALESCE(NEW.branch, '') || ' ' || COALESCE(NEW.model, ''));
END;
-- +goose StatementEnd
-- +goose StatementBegin
CREATE TRIGGER search_runs_upd AFTER UPDATE OF state, end_reason, failure_code, branch, model ON runs
BEGIN
    UPDATE search_docs SET project_id = COALESCE((SELECT t.project_id FROM tasks t WHERE t.id = NEW.task_id), ''), archived = 0 WHERE id = NEW.id;
    UPDATE search_index SET name = COALESCE((SELECT t.name FROM tasks t WHERE t.id = NEW.task_id), '') || ' #' || NEW.attempt, body = NEW.state || ' ' || COALESCE(NEW.end_reason, '') || ' ' || COALESCE(NEW.failure_code, '') || ' ' || COALESCE(NEW.branch, '') || ' ' || COALESCE(NEW.model, '')
     WHERE rowid = (SELECT rowid FROM search_docs WHERE id = NEW.id);
END;
-- +goose StatementEnd
-- +goose StatementBegin
CREATE TRIGGER search_runs_del AFTER DELETE ON runs
BEGIN
    DELETE FROM search_index WHERE rowid = (SELECT rowid FROM search_docs WHERE id = OLD.id);
    DELETE FROM search_docs WHERE id = OLD.id;
END;
-- +goose StatementEnd

-- worker
INSERT INTO search_docs (kind, id, short_id, project_id, archived)
SELECT 'worker', x.id, 'worker-' || x.short_n, '', x.removed_at IS NOT NULL FROM workers x;
INSERT INTO search_index (rowid, name, body)
SELECT s.rowid, x.name, COALESCE(x.hostname, '') || ' ' || COALESCE(x.os, '') || ' ' || COALESCE(x.arch, '') FROM workers x JOIN search_docs s ON s.id = x.id;
-- +goose StatementBegin
CREATE TRIGGER search_workers_ins AFTER INSERT ON workers
BEGIN
    INSERT INTO search_docs (kind, id, short_id, project_id, archived)
    VALUES ('worker', NEW.id, 'worker-' || NEW.short_n, '', NEW.removed_at IS NOT NULL);
    INSERT INTO search_index (rowid, name, body)
    VALUES ((SELECT rowid FROM search_docs WHERE id = NEW.id), NEW.name, COALESCE(NEW.hostname, '') || ' ' || COALESCE(NEW.os, '') || ' ' || COALESCE(NEW.arch, ''));
END;
-- +goose StatementEnd
-- +goose StatementBegin
CREATE TRIGGER search_workers_upd AFTER UPDATE OF name, hostname, os, arch, removed_at ON workers
BEGIN
    UPDATE search_docs SET archived = NEW.removed_at IS NOT NULL WHERE id = NEW.id;
    UPDATE search_index SET name = NEW.name, body = COALESCE(NEW.hostname, '') || ' ' || COALESCE(NEW.os, '') || ' ' || COALESCE(NEW.arch, '')
     WHERE rowid = (SELECT rowid FROM search_docs WHERE id = NEW.id);
END;
-- +goose StatementEnd
-- +goose StatementBegin
CREATE TRIGGER search_workers_del AFTER DELETE ON workers
BEGIN
    DELETE FROM search_index WHERE rowid = (SELECT rowid FROM search_docs WHERE id = OLD.id);
    DELETE FROM search_docs WHERE id = OLD.id;
END;
-- +goose StatementEnd

-- workflow
INSERT INTO search_docs (kind, id, short_id, project_id, archived)
SELECT 'workflow', x.id, '', '', 0 FROM workflows x;
INSERT INTO search_index (rowid, name, body)
SELECT s.rowid, x.name, COALESCE((SELECT v.changelog FROM workflow_versions v WHERE v.workflow_id = x.id ORDER BY v.version DESC LIMIT 1), '') FROM workflows x JOIN search_docs s ON s.id = x.id;
-- +goose StatementBegin
CREATE TRIGGER search_workflows_ins AFTER INSERT ON workflows
BEGIN
    INSERT INTO search_docs (kind, id, short_id, project_id, archived)
    VALUES ('workflow', NEW.id, '', '', 0);
    INSERT INTO search_index (rowid, name, body)
    VALUES ((SELECT rowid FROM search_docs WHERE id = NEW.id), NEW.name, COALESCE((SELECT v.changelog FROM workflow_versions v WHERE v.workflow_id = NEW.id ORDER BY v.version DESC LIMIT 1), ''));
END;
-- +goose StatementEnd
-- +goose StatementBegin
CREATE TRIGGER search_workflows_upd AFTER UPDATE OF name ON workflows
BEGIN
    UPDATE search_index SET name = NEW.name, body = COALESCE((SELECT v.changelog FROM workflow_versions v WHERE v.workflow_id = NEW.id ORDER BY v.version DESC LIMIT 1), '')
     WHERE rowid = (SELECT rowid FROM search_docs WHERE id = NEW.id);
END;
-- +goose StatementEnd
-- +goose StatementBegin
CREATE TRIGGER search_workflows_del AFTER DELETE ON workflows
BEGIN
    DELETE FROM search_index WHERE rowid = (SELECT rowid FROM search_docs WHERE id = OLD.id);
    DELETE FROM search_docs WHERE id = OLD.id;
END;
-- +goose StatementEnd

-- A renamed or moved Task is also what its Runs are named and filed under.
-- +goose StatementBegin
CREATE TRIGGER search_tasks_runs AFTER UPDATE OF name, project_id ON tasks
BEGIN
    UPDATE search_docs SET project_id = NEW.project_id
     WHERE id IN (SELECT id FROM runs WHERE task_id = NEW.id);
    UPDATE search_index
       SET name = NEW.name || ' #' || (SELECT r.attempt FROM runs r JOIN search_docs d ON d.id = r.id WHERE d.rowid = search_index.rowid)
     WHERE rowid IN (SELECT d.rowid FROM search_docs d JOIN runs r ON r.id = d.id WHERE r.task_id = NEW.id);
END;
-- +goose StatementEnd

-- A Workflow is described by the changelog of its latest version.
-- +goose StatementBegin
CREATE TRIGGER search_workflow_versions_ins AFTER INSERT ON workflow_versions
BEGIN
    UPDATE search_index
       SET body = COALESCE((SELECT v.changelog FROM workflow_versions v WHERE v.workflow_id = NEW.workflow_id ORDER BY v.version DESC LIMIT 1), '')
     WHERE rowid = (SELECT rowid FROM search_docs WHERE id = NEW.workflow_id);
END;
-- +goose StatementEnd
