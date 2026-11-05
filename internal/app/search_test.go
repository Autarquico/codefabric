package app_test

import (
	"errors"
	"fmt"
	"os"
	"regexp"
	"slices"
	"strings"
	"testing"

	v1 "github.com/Autarquico/codefabric/internal/api/v1"
	"github.com/Autarquico/codefabric/internal/app/search"
	"github.com/Autarquico/codefabric/internal/app/workflows"
	"github.com/Autarquico/codefabric/internal/controller/store"
	"github.com/Autarquico/codefabric/internal/domain"
)

func (e *env) search(text string, mutate ...func(*search.Query)) []v1.SearchResult {
	e.t.Helper()
	got, err := e.searchErr(text, mutate...)
	must(e.t, err)
	return got
}

func (e *env) searchErr(text string, mutate ...func(*search.Query)) ([]v1.SearchResult, error) {
	e.t.Helper()
	q := search.Query{Text: text}
	for _, m := range mutate {
		m(&q)
	}
	out, err := e.app.Search.Search(e.t.Context(), q)
	return out.Results, err
}

func ofKind(kind string) func(*search.Query) { return func(q *search.Query) { q.Kind = kind } }

func inProject(ref string) func(*search.Query) { return func(q *search.Query) { q.Project = ref } }

// find is the result with the ID, or a zero one.
func find(results []v1.SearchResult, id string) (v1.SearchResult, bool) {
	for _, r := range results {
		if r.ID == id {
			return r, true
		}
	}
	return v1.SearchResult{}, false
}

func (e *env) exec(query string, args ...any) {
	e.t.Helper()
	must(e.t, e.db.WithTx(e.t.Context(), func(tx *store.Tx) error {
		_, err := tx.Exec(e.t.Context(), query, args...)
		return err
	}))
}

func TestScenario_PaletteSearchNotify_Busqueda(t *testing.T) {
	e := newEnv(t)
	e.project("backend")
	task := e.mustTask("backend", "drain the staging workers", func(r *v1.CreateTaskRequest) { r.Name = "drain-staging" })

	for _, c := range []struct{ name, query, want string }{
		{"short ID", task.Task.Short, task.Task.ID},
		{"canonical ID", task.Task.ID, task.Task.ID},
		{"ULID prefix", strings.TrimPrefix(task.Task.ID, "task_")[:22], task.Task.ID},
		{"run short ID", task.Run.Short, task.Run.ID},
		{"run ULID prefix", strings.TrimPrefix(task.Run.ID, "run_")[:22], task.Run.ID},
		{"name", "drain-staging", task.Task.ID},
	} {
		got := e.search(c.query)
		if len(got) == 0 || got[0].ID != c.want || got[0].Match != v1.SearchMatchExact {
			t.Errorf("%s: search %q = %+v, want %s first and exact", c.name, c.query, got, c.want)
		}
	}
	got := e.search("staging workers")
	if r, ok := find(got, task.Task.ID); !ok || r.Match != v1.SearchMatchText {
		t.Errorf("text search = %+v, want the Task as a text match", got)
	}
}

func TestSearch_ResolverReachesWorkersAndWorkflows(t *testing.T) {
	e := newWorkflowEnv(t, 0)
	w := e.joinWorker("builder-1")
	version := e.mustCommit("review", 0, manifest(e.component("codex", "agents", "one")))

	worker := e.storedWorker(w)
	for _, c := range []struct {
		kind domain.Kind
		ref  string
		want string
	}{
		{domain.KindWorker, "builder-1", string(worker.ID)},
		{domain.KindWorker, fmt.Sprintf("worker-%d", worker.Short), string(worker.ID)},
		{domain.KindWorkflow, "review", version.WorkflowID},
	} {
		got, err := e.app.Resolve(t.Context(), c.kind, c.ref, "")
		if err != nil || got.ID != c.want {
			t.Errorf("Resolve(%s, %q) = %+v, %v; want %s", c.kind, c.ref, got, err, c.want)
		}
	}
	if got, _ := e.app.Resolve(t.Context(), domain.KindWorkflow, "review", ""); got.Short != "" {
		t.Errorf("a Workflow has no short ID, got %q", got.Short)
	}
	_, err := e.app.Resolve(t.Context(), domain.KindPlan, "x", "")
	wantCode(t, err, domain.CodeInvalidRequest)
}

func TestSearch_IndexesCreateUpdateArchiveAndDeleteOfEveryKind(t *testing.T) {
	e := newWorkflowEnv(t, 0)
	project := e.project("backend", func(r *v1.CreateProjectRequest) { r.Repo = "git@example.com:org/zebrafish.git" })
	created := e.mustTask("backend", "migrate the tables", func(r *v1.CreateTaskRequest) { r.Name = "quokka" })
	task, run := created.Task, created.Run
	w := e.joinWorker("narwhal-1")
	worker := e.storedWorker(w)
	wf := e.mustCommit("platypus", 0, manifest(e.component("codex", "agents", "one")))

	present := func(query, id string) bool {
		t.Helper()
		_, ok := find(e.search(query, func(q *search.Query) { q.Limit = 100 }), id)
		return ok
	}

	// create
	for _, c := range []struct{ kind, query, id string }{
		{"project", "zebrafish", project.ID},
		{"task", "quokka", task.ID},
		{"run", "quokka", run.ID},
		{"worker", "narwhal", string(worker.ID)},
		{"workflow", "platypus", wf.WorkflowID},
	} {
		if !present(c.query, c.id) {
			t.Errorf("%s %s not found by %q after create", c.kind, c.id, c.query)
		}
	}

	// update
	rename := "wombat"
	_, err := e.app.Tasks.Set(t.Context(), e.owner, task.ID, "", v1.UpdateTaskRequest{Name: &rename})
	must(t, err)
	if present("quokka", task.ID) || !present("wombat", task.ID) {
		t.Error("a renamed Task is still found by its old name or not by the new one")
	}
	if !present("wombat", run.ID) { // the old name stays in the branch, which is text of its own
		t.Error("the Runs of a renamed Task keep its old name")
	}
	e.exec(`UPDATE runs SET branch = 'cf/capybara-branch' WHERE id = ?`, run.ID)
	if !present("capybara", run.ID) {
		t.Error("a changed Run is not found by its new text")
	}
	if _, err := e.commit("platypus", 1, manifest(e.component("codex", "agents", "two")), func(r *workflows.CommitRequest) { r.Changelog = "adds the okapi rules" }); err != nil {
		t.Fatal(err)
	}
	if !present("okapi", wf.WorkflowID) {
		t.Error("a new Workflow version is not found by its changelog")
	}

	// archive and remove: out of text matches, still found by ID
	_, err = e.app.Tasks.Cancel(t.Context(), e.owner, task.ID, "", v1.ActionRequest{})
	must(t, err)
	_, err = e.app.Tasks.Archive(t.Context(), e.owner, task.ID, "", v1.ActionRequest{})
	must(t, err)
	if present("wombat", task.ID) {
		t.Error("an archived Task still matches text")
	}
	if got := e.search(task.Short); len(got) == 0 || got[0].ID != task.ID {
		t.Errorf("an archived Task is not found by its short ID: %+v", got)
	}
	_, err = e.app.Workers.Remove(t.Context(), e.owner, string(worker.ID), v1.ActionRequest{})
	must(t, err)
	if present("narwhal", string(worker.ID)) {
		t.Error("a removed Worker still matches text")
	}

	// delete
	e.exec(`DELETE FROM workflow_versions WHERE workflow_id = ?`, wf.WorkflowID)
	e.exec(`DELETE FROM workflows WHERE id = ?`, wf.WorkflowID)
	if present("platypus", wf.WorkflowID) || present(wf.WorkflowID, wf.WorkflowID) {
		t.Error("a deleted Workflow is still indexed")
	}
	other := e.mustTask("backend", "loose end", func(r *v1.CreateTaskRequest) { r.Name = "loose"; noStart(r) })
	e.exec(`DELETE FROM tasks WHERE id = ?`, other.Task.ID)
	if present("loose", other.Task.ID) || e.count("search_docs") != e.count("search_index") {
		t.Error("a deleted Task is still indexed")
	}
}

// TestSearch_IndexEqualsAFreshRebuildAfterAWorkload fails when a mutation of an
// indexed entity leaves the index different from what the migration's backfill
// would build from the tables: the way to notice a new write path or a new
// indexed column the triggers do not cover.
func TestSearch_IndexEqualsAFreshRebuildAfterAWorkload(t *testing.T) {
	e := newWorkflowEnv(t, 0)
	e.project("alpha")
	e.project("beta")
	a := e.mustTask("alpha", "first prompt", func(r *v1.CreateTaskRequest) { r.Name = "one" })
	e.mustTask("beta", "second prompt", func(r *v1.CreateTaskRequest) { r.Name = "two"; noStart(r) })
	_, err := e.startRun(a.Task.ID, commitA)
	must(t, err)
	renamed := "uno"
	_, err = e.app.Tasks.Set(t.Context(), e.owner, a.Task.ID, "", v1.UpdateTaskRequest{Name: &renamed})
	must(t, err)
	e.exec(`UPDATE runs SET branch = 'cf/x', model = 'opus' WHERE id = ?`, a.Run.ID)
	_, err = e.app.Tasks.Cancel(t.Context(), e.owner, a.Task.ID, "", v1.ActionRequest{})
	must(t, err)
	_, err = e.app.Tasks.Archive(t.Context(), e.owner, a.Task.ID, "", v1.ActionRequest{})
	must(t, err)
	e.joinWorker("w1")
	e.joinWorker("w2")
	_, err = e.app.Workers.Remove(t.Context(), e.owner, "w2", v1.ActionRequest{})
	must(t, err)
	e.mustCommit("wf", 0, manifest(e.component("codex", "agents", "one")))
	_, err = e.commit("wf", 1, manifest(e.component("codex", "agents", "two")), func(r *workflows.CommitRequest) { r.Changelog = "second" })
	must(t, err)

	before := e.indexDump()
	rebuild := backfillStatements(t)
	e.exec(`DELETE FROM search_index`)
	e.exec(`DELETE FROM search_docs`)
	for _, stmt := range rebuild {
		e.exec(stmt)
	}
	after := e.indexDump()
	if len(before) == 0 || !slices.Equal(before, after) {
		t.Errorf("the index differs from a rebuild:\n--- live\n%s\n--- rebuilt\n%s", strings.Join(before, "\n"), strings.Join(after, "\n"))
	}
}

func (e *env) indexDump() []string {
	e.t.Helper()
	rows, err := e.db.Reader().QueryContext(e.t.Context(),
		`SELECT d.kind, d.id, d.short_id, d.project_id, d.archived, i.name, i.body
		   FROM search_docs d JOIN search_index i ON i.rowid = d.rowid ORDER BY d.id`)
	must(e.t, err)
	defer rows.Close()
	var out []string
	for rows.Next() {
		var kind, id, short, project, name, body string
		var archived int
		must(e.t, rows.Scan(&kind, &id, &short, &project, &archived, &name, &body))
		out = append(out, fmt.Sprintf("%s|%s|%s|%s|%d|%s|%s", kind, id, short, project, archived, name, body))
	}
	must(e.t, rows.Err())
	return out
}

var backfillRe = regexp.MustCompile(`(?ms)^INSERT INTO search_(?:docs|index)\b.*?;$`)

// backfillStatements are the statements of the migration that fill the index
// from the tables (the triggers' own inserts are indented, so they do not match).
func backfillStatements(t *testing.T) []string {
	t.Helper()
	sql, err := os.ReadFile("../controller/store/migrations/0213_search_index.sql")
	must(t, err)
	stmts := backfillRe.FindAllString(string(sql), -1)
	if len(stmts) != 10 { // docs and text for each of the five kinds
		t.Fatalf("found %d backfill statements in the migration, want 10", len(stmts))
	}
	return stmts
}

func TestSearch_EveryIndexedTableHasItsTriggers(t *testing.T) {
	e := newEnv(t)
	for _, table := range []string{"projects", "tasks", "runs", "workers", "workflows"} {
		for _, op := range []string{"ins", "upd", "del"} {
			var n int
			name := "search_" + table + "_" + op
			must(t, e.db.Reader().QueryRowContext(t.Context(), `SELECT COUNT(*) FROM sqlite_master WHERE type = 'trigger' AND name = ?`, name).Scan(&n))
			if n != 1 {
				t.Errorf("trigger %s is missing: %s would not be indexed in the transaction of the change", name, table)
			}
		}
	}
}

func TestSearch_RanksANameAboveTheBody(t *testing.T) {
	e := newEnv(t)
	e.project("backend")
	inBody := e.mustTask("backend", "the deploy runbook mentions deploy twice", func(r *v1.CreateTaskRequest) { r.Name = "runbook"; noStart(r) })
	inName := e.mustTask("backend", "nothing to see", func(r *v1.CreateTaskRequest) { r.Name = "deploy"; noStart(r) })

	got := e.search("deploy", ofKind("task"))
	// "deploy" is also the exact name of one Task: it comes first as a reference,
	// then the text match.
	if len(got) != 2 || got[0].ID != inName.Task.ID || got[0].Match != v1.SearchMatchExact || got[1].ID != inBody.Task.ID {
		t.Fatalf("results = %+v", got)
	}

	e.mustTask("backend", "deployment notes", func(r *v1.CreateTaskRequest) { r.Name = "notes-a"; noStart(r) })
	e.mustTask("backend", "x", func(r *v1.CreateTaskRequest) { r.Name = "deploys-b"; noStart(r) })
	got = e.search("deplo", ofKind("task"))
	var order []string
	for _, r := range got {
		order = append(order, r.Name)
	}
	rank := func(name string) int { return slices.Index(order, name) }
	if len(order) != 4 || max(rank("deploy"), rank("deploys-b")) > min(rank("runbook"), rank("notes-a")) {
		t.Errorf("hits in the name must rank above hits in the body: %v", order)
	}
}

func TestSearch_RanksByRelevanceAcrossKinds(t *testing.T) {
	e := newWorkflowEnv(t, 0)
	e.project("backend")
	for i := range 3 {
		e.mustTask("backend", "we should drain the queue", func(r *v1.CreateTaskRequest) { r.Name = fmt.Sprintf("job-%d", i); noStart(r) })
	}
	worker := e.storedWorker(e.joinWorker("drain-box"))

	got := e.search("drain")
	if len(got) == 0 || got[0].ID != string(worker.ID) {
		t.Errorf("a Worker named drain-box must outrank Tasks that mention drain in the body: %+v", got)
	}

	exact := e.mustTask("backend", "nothing", func(r *v1.CreateTaskRequest) { r.Name = "drain"; noStart(r) })
	got = e.search("drain")
	if len(got) < 2 || got[0].ID != exact.Task.ID || got[0].Match != v1.SearchMatchExact || got[1].ID != string(worker.ID) {
		t.Errorf("the exact reference first, then the best text match: %+v", got)
	}
}

func TestSearch_UserInputIsNeverFTSSyntax(t *testing.T) {
	e := newEnv(t)
	e.project("backend")
	drain := e.mustTask("backend", "drain the pool NEAR the end", func(r *v1.CreateTaskRequest) { r.Name = "drain"; noStart(r) })
	other := e.mustTask("backend", "unrelated", func(r *v1.CreateTaskRequest) { r.Name = "other"; noStart(r) })

	for _, q := range []string{
		`"`, `"drain`, `drain"`, `*`, `drain*`, `NEAR(drain pool)`, `NEAR`, `-drain`, `-`, `name:drain`, `body:pool`, `:`, `drain:`,
		`drain AND`, `OR pool`, `NOT drain`, `drain OR other`, `(drain`, `^drain`, `{name body}: drain`, `drain + pool`, "drain\x00pool", `'; DROP TABLE tasks; --`,
	} {
		if _, err := e.searchErr(q); err != nil {
			t.Errorf("query %q is an error: %v", q, err)
		}
	}

	ids := func(q string) []string {
		var out []string
		for _, r := range e.search(q, ofKind("task")) {
			out = append(out, r.ID)
		}
		return out
	}
	if got := ids("-drain"); !slices.Contains(got, drain.Task.ID) {
		t.Errorf("-drain should read as the word drain, got %v", got)
	}
	if got := ids("drain OR other"); slices.Contains(got, other.Task.ID) {
		t.Errorf("OR must be a word, not a union: %v", got)
	}
	if got := ids("NEAR(drain"); !slices.Equal(got, []string{drain.Task.ID}) {
		t.Errorf("NEAR must be a word (the prompt has it), got %v", got)
	}
	if got := ids("*"); len(got) != 0 {
		t.Errorf("a bare * is no query: %v", got)
	}
}

func TestSearch_BoundsTheQueryAndTheLimit(t *testing.T) {
	e := newEnv(t)
	for _, c := range []struct {
		name string
		text string
		mut  func(*search.Query)
	}{
		{"empty", "  ", nil},
		{"too long", strings.Repeat("a", v1.SearchMaxQueryBytes+1), nil},
		{"limit too big", "x", func(q *search.Query) { q.Limit = v1.SearchMaxLimit + 1 }},
		{"negative limit", "x", func(q *search.Query) { q.Limit = -1 }},
		{"unknown kind", "x", ofKind("plan")},
	} {
		var mut []func(*search.Query)
		if c.mut != nil {
			mut = append(mut, c.mut)
		}
		_, err := e.searchErr(c.text, mut...)
		wantCode(t, err, domain.CodeInvalidRequest)
	}
	if _, err := e.searchErr(strings.Repeat("a", v1.SearchMaxQueryBytes)); err != nil {
		t.Errorf("a query of exactly the maximum length: %v", err)
	}

	e.project("backend")
	for i := range 5 {
		e.mustTask("backend", "bulk item", func(r *v1.CreateTaskRequest) { r.Name = fmt.Sprintf("bulk-%d", i); noStart(r) })
	}
	if got := e.search("bulk", func(q *search.Query) { q.Limit = 3 }); len(got) != 3 {
		t.Errorf("limit 3 returned %d results", len(got))
	}
}

func TestSearch_AmbiguousReferenceReturnsEveryCandidate(t *testing.T) {
	e := newEnv(t)
	e.project("backend")
	e.project("frontend")
	named := func(r *v1.CreateTaskRequest) { r.Name = "shared"; noStart(r) }
	a := e.mustTask("backend", "a", named).Task
	b := e.mustTask("frontend", "b", named).Task

	got := e.search("shared", ofKind("task"))
	if len(got) != 2 {
		t.Fatalf("results = %+v", got)
	}
	for _, id := range []string{a.ID, b.ID} {
		if r, ok := find(got, id); !ok || r.Match != v1.SearchMatchAmbiguous {
			t.Errorf("%s: %+v, want it listed as ambiguous", id, r)
		}
	}

	got = e.search("shared", ofKind("task"), inProject("frontend"))
	if len(got) != 1 || got[0].ID != b.ID || got[0].Match != v1.SearchMatchExact {
		t.Errorf("narrowed to a Project = %+v, want %s alone", got, b.ID)
	}
	_, err := e.searchErr("shared", inProject("nope"))
	wantCode(t, err, domain.CodeNotFound)
}

func TestSearch_ANameNeverPushesAShortIDOutOfTheLimit(t *testing.T) {
	e := newEnv(t)
	e.project("backend")
	task := e.mustTask("backend", "x", func(r *v1.CreateTaskRequest) { noStart(r) }).Task
	// The API refuses such a name; a legacy row or a direct write may still have one.
	e.exec(`UPDATE projects SET name = 'task-1'`)
	if task.Short != "task-1" {
		t.Fatalf("the first Task is %s, want task-1", task.Short)
	}

	got := e.search("task-1", func(q *search.Query) { q.Limit = 1 })
	if len(got) != 1 || got[0].ID != task.ID {
		t.Errorf("limit 1 = %+v, want Task %s: a short ID outranks a name", got, task.ID)
	}
	got = e.search("task-1")
	if len(got) < 2 || got[0].ID != task.ID || got[1].Kind != domain.KindProject {
		t.Errorf("results = %+v, want the Task (short ID) before the Project (name)", got)
	}
}

func TestSearch_APrefixOfManyEntriesIsBoundedByTheLimit(t *testing.T) {
	e := newEnv(t)
	// Far more entries than SQLite has variables, all behind one ULID prefix.
	e.exec(`WITH RECURSIVE n(i) AS (SELECT 1 UNION ALL SELECT i + 1 FROM n WHERE i < 2000)
		INSERT INTO search_docs (kind, id, short_id, project_id, archived)
		SELECT 'task', 'task_BULKPFX' || printf('%05d', i), 'task-' || (900000 + i), '', 0 FROM n`)
	e.exec(`INSERT INTO search_index (rowid, name, body) SELECT rowid, 'bulk', '' FROM search_docs WHERE id LIKE 'task_BULKPFX%'`)

	for _, limit := range []int{1, 5} {
		got, err := e.searchErr("bulkpf", func(q *search.Query) { q.Limit = limit })
		if err != nil || len(got) != limit {
			t.Fatalf("limit %d: %d results, err %v", limit, len(got), err)
		}
		for _, r := range got {
			if r.Match != v1.SearchMatchAmbiguous {
				t.Errorf("limit %d: %+v, want ambiguous", limit, r)
			}
		}
	}
	if got := e.search("bulkpf"); len(got) != v1.SearchDefaultLimit {
		t.Errorf("default limit: %d results, want %d", len(got), v1.SearchDefaultLimit)
	}
}

func idPrefix(t *testing.T, kind, a, b string) string {
	t.Helper()
	pa, pb := strings.TrimPrefix(a, kind+"_")[:6], strings.TrimPrefix(b, kind+"_")[:6]
	if pa != pb {
		t.Skipf("the two %s IDs do not share a 6-character prefix (%s, %s)", kind, pa, pb)
	}
	return pa
}

func TestSearch_ULIDPrefixOfProjectsWorkersAndWorkflows(t *testing.T) {
	e := newWorkflowEnv(t, 0)
	back := e.project("backend")
	front := e.project("frontend")
	w1, w2 := e.storedWorker(e.joinWorker("one")), e.storedWorker(e.joinWorker("two"))
	wf1 := e.mustCommit("wf-a", 0, manifest(e.component("codex", "agents", "one")))
	wf2 := e.mustCommit("wf-b", 0, manifest(e.component("codex", "agents", "two")))

	ambiguous := func(kind, prefix string, want ...string) {
		t.Helper()
		got := e.search(prefix, ofKind(kind))
		if len(got) != len(want) {
			t.Fatalf("%s %q = %+v, want %v", kind, prefix, got, want)
		}
		for _, id := range want {
			if r, ok := find(got, id); !ok || r.Match != v1.SearchMatchAmbiguous {
				t.Errorf("%s %q: %s = %+v, want it ambiguous", kind, prefix, id, r)
			}
		}
	}
	ambiguous("project", idPrefix(t, "project", back.ID, front.ID), back.ID, front.ID)
	ambiguous("worker", idPrefix(t, "worker", string(w1.ID), string(w2.ID)), string(w1.ID), string(w2.ID))
	ambiguous("workflow", idPrefix(t, "workflow", wf1.WorkflowID, wf2.WorkflowID), wf1.WorkflowID, wf2.WorkflowID)

	// A prefix longer than the shared part names one entity.
	for _, c := range []struct{ kind, id string }{
		{"project", back.ID}, {"worker", string(w1.ID)}, {"workflow", wf1.WorkflowID},
	} {
		long := strings.TrimPrefix(c.id, c.kind+"_")[:20]
		got := e.search(long, ofKind(c.kind))
		if len(got) != 1 || got[0].ID != c.id || got[0].Match != v1.SearchMatchExact {
			t.Errorf("%s prefix %q = %+v, want %s exact", c.kind, long, got, c.id)
		}
	}

	// A Project filter keeps the Project's own entry and drops the global kinds.
	got := e.search(idPrefix(t, "project", back.ID, front.ID), ofKind("project"), inProject("frontend"))
	if len(got) != 1 || got[0].ID != front.ID || got[0].Match != v1.SearchMatchExact {
		t.Errorf("Project prefix narrowed to frontend = %+v", got)
	}
	got = e.search(strings.TrimPrefix(string(w1.ID), "worker_")[:20], ofKind("worker"), inProject("frontend"))
	if len(got) != 0 {
		t.Errorf("a Worker belongs to no Project, got %+v under a Project filter", got)
	}
}

func TestSearch_AnAbortedMutationLeavesTheIndexUntouched(t *testing.T) {
	e := newEnv(t)
	e.project("backend")
	e.mustTask("backend", "kept", func(r *v1.CreateTaskRequest) { r.Name = "kept"; noStart(r) })
	before := e.indexDump()

	abort := errors.New("abort")
	err := e.db.WithTx(t.Context(), func(tx *store.Tx) error {
		for _, stmt := range []string{
			`UPDATE tasks SET name = 'renamed', prompt = 'other words'`,
			`UPDATE projects SET name = 'renamed'`,
			`DELETE FROM runs`,
			`DELETE FROM tasks`,
		} {
			if _, err := tx.Exec(t.Context(), stmt); err != nil {
				return err
			}
		}
		return abort
	})
	if !errors.Is(err, abort) {
		t.Fatalf("err = %v, want the abort", err)
	}
	if after := e.indexDump(); !slices.Equal(before, after) {
		t.Errorf("an aborted transaction changed the index:\n--- before\n%s\n--- after\n%s", strings.Join(before, "\n"), strings.Join(after, "\n"))
	}
}
