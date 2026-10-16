package store

import (
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	v1 "github.com/Autarquico/codefabric/internal/api/v1"
	"github.com/Autarquico/codefabric/internal/domain"
)

// The secrets tables hold refs and metadata only. Every column is pinned here,
// so a column added to them has to be added to this list, and looked at.
func TestSecretsTables_HaveNoColumnForAValue(t *testing.T) {
	db, _ := openTest(t, Options{})
	want := map[string][]string{
		"project_secret_allowlist": {"project_id", "ref", "added_at", "added_by"},
		"worker_secret_reports":    {"worker_id", "generation", "epoch", "seq", "metadata_digest", "reported_at"},
		"worker_secrets":           {"worker_id", "ref", "backend", "projects", "updated_at"},
		"secret_grants":            {"id", "run_id", "lease_id", "lease_epoch", "worker_id", "worker_epoch", "granted", "decided_at"},
		"secret_grant_refs":        {"grant_id", "ref", "denial"},
	}
	for table, columns := range want {
		if got := secretsColumns(t, db, table); !slices.Equal(got, columns) {
			t.Errorf("%s has columns %v, want %v", table, got, columns)
		}
	}
	var n int
	if err := db.Reader().QueryRowContext(t.Context(),
		`SELECT COUNT(*) FROM sqlite_schema WHERE type = 'table' AND name LIKE '%secret%'`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != len(want) {
		t.Errorf("%d tables are about secrets, %d are pinned here", n, len(want))
	}
}

// secretsColumns lists the columns of table, failing on any whose name or type
// could hold a value.
func secretsColumns(t *testing.T, db *DB, table string) []string {
	t.Helper()
	rows, err := db.Reader().QueryContext(t.Context(), `SELECT name, type FROM pragma_table_info(?)`, table)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rows.Close() }()
	var got []string
	for rows.Next() {
		var name, typ string
		if err := rows.Scan(&name, &typ); err != nil {
			t.Fatal(err)
		}
		got = append(got, name)
		if typ == "BLOB" || typ == "ANY" {
			t.Errorf("%s.%s is %s: nothing in a secrets table holds bytes", table, name, typ)
		}
		for _, word := range []string{"value", "hash", "length", "secret", "content"} {
			if strings.Contains(name, word) {
				t.Errorf("%s.%s names a %s", table, name, word)
			}
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return got
}

// A Controller with the data of Phase 1 upgrades to the secrets schema, after
// leaving a backup, and keeps its Projects, Tasks and Runs.
func TestOpen_UpgradesAPhase1DatabaseToSecrets(t *testing.T) {
	path := filepath.Join(t.TempDir(), "controller.db")
	seedUntil(t, path, 107)

	db := reopen(t, path, Options{})
	if err := quickCheckFile(t.Context(), path+".pre-107"); err != nil {
		t.Errorf("no valid backup of the 0107 database: %v", err)
	}
	for table, want := range map[string]int{
		"projects": 1, "tasks": 1, "runs": 1,
		"project_secret_allowlist": 0, "worker_secrets": 0, "worker_secret_reports": 0, "secret_grants": 0,
	} {
		if got := count(t, db, table); got != want {
			t.Errorf("%s has %d rows after the upgrade, want %d", table, got, want)
		}
	}
	ctx := t.Context()
	if err := db.WithTx(ctx, func(tx *Tx) error {
		_, err := tx.AllowSecrets(ctx, "project_1", []string{"github/token"}, "client_1", epoch0)
		return err
	}); err != nil {
		t.Fatalf("the Project of Phase 1 cannot allow a secret: %v", err)
	}
}

func TestAllowlist_AllowAndDenyReportWhatChanged(t *testing.T) {
	f := newLeaseFixture(t)
	ctx := t.Context()
	f.tx(func(tx *Tx) error {
		return tx.InsertProject(ctx, Project{ID: "project_2", Short: 2, Name: "q", Repo: "r", DefaultBranch: "main", CreatedAt: epoch0})
	})

	var added, again, removed []string
	f.tx(func(tx *Tx) (err error) {
		if added, err = tx.AllowSecrets(ctx, "project_1", []string{"npm", "github/token", "npm"}, "client_1", epoch0); err != nil {
			return err
		}
		if again, err = tx.AllowSecrets(ctx, "project_1", []string{"npm", "pypi"}, "client_1", epoch0); err != nil {
			return err
		}
		if _, err = tx.AllowSecrets(ctx, "project_2", []string{"npm"}, "client_1", epoch0); err != nil {
			return err
		}
		removed, err = tx.DenySecrets(ctx, "project_1", []string{"github/token", "never-allowed"})
		return err
	})
	if !slices.Equal(added, []string{"github/token", "npm"}) || !slices.Equal(again, []string{"pypi"}) {
		t.Errorf("allow added %v then %v, want [github/token npm] then [pypi]", added, again)
	}
	if !slices.Equal(removed, []string{"github/token"}) {
		t.Errorf("deny removed %v, want only the ref the allowlist held", removed)
	}

	var one []string
	var all map[domain.ID][]string
	if err := f.db.Read(ctx, func(q Querier) (err error) {
		if one, err = ProjectAllowlist(ctx, q, "project_1"); err != nil {
			return err
		}
		all, err = AllowlistsFor(ctx, q, []domain.ID{"project_1", "project_2", "project_none"})
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(one, []string{"npm", "pypi"}) {
		t.Errorf("allowlist of project_1 = %v", one)
	}
	if len(all) != 2 || !slices.Equal(all["project_1"], one) || !slices.Equal(all["project_2"], []string{"npm"}) {
		t.Errorf("AllowlistsFor = %v", all)
	}
}

func TestSecretsFence_OrdersByGenerationThenSeq(t *testing.T) {
	fence := SecretsFence{Generation: 2, Epoch: "e2", Seq: 5}
	legacy := SecretsFence{Generation: 0, Epoch: "e0", Seq: 5}
	for _, c := range []struct {
		name  string
		fence SecretsFence
		r     SecretsReport
		want  bool
	}{
		{"a later seq of the same daemon", fence, SecretsReport{Generation: 2, Epoch: "e2", Seq: 6}, true},
		{"the same seq again", fence, SecretsReport{Generation: 2, Epoch: "e2", Seq: 5}, false},
		{"an earlier seq of the same daemon", fence, SecretsReport{Generation: 2, Epoch: "e2", Seq: 4}, false},
		{"a new generation starting over", fence, SecretsReport{Generation: 3, Epoch: "e3", Seq: 0}, true},
		{"a retired generation, however late its seq", fence, SecretsReport{Generation: 1, Epoch: "e1", Seq: 99}, false},
		{"a daemon without generation after one with it", fence, SecretsReport{Generation: 0, Epoch: "eX", Seq: 99}, false},
		{"a new epoch, both without generation", legacy, SecretsReport{Generation: 0, Epoch: "e1", Seq: 0}, true},
		{"an earlier seq, both without generation", legacy, SecretsReport{Generation: 0, Epoch: "e0", Seq: 4}, false},
		{"the first daemon with a generation", legacy, SecretsReport{Generation: 1, Epoch: "e1", Seq: 0}, true},
	} {
		if got := c.fence.Admits(c.r); got != c.want {
			t.Errorf("%s: Admits = %v, want %v", c.name, got, c.want)
		}
	}
	if !fence.Retired(1) || !fence.Retired(0) || fence.Retired(2) || fence.Retired(3) {
		t.Error("Retired must be exactly the generations below the fence")
	}
}

type secretsFixture struct {
	*workerFixture
	w Worker
}

func newSecretsFixture(t *testing.T) *secretsFixture {
	f := &secretsFixture{workerFixture: newWorkerFixture(t)}
	f.w = f.worker(1, "alpha", domain.WorkerOnline)
	return f
}

func (f *secretsFixture) report(r SecretsReport) SecretsOutcome {
	f.t.Helper()
	var out SecretsOutcome
	if err := f.db.WithTx(f.t.Context(), func(tx *Tx) (err error) {
		out, err = tx.ReplaceWorkerSecrets(f.t.Context(), f.w.ID, r, f.clk.Now())
		return err
	}); err != nil {
		f.t.Fatal(err)
	}
	return out
}

func (f *secretsFixture) held() []v1.SecretMeta {
	f.t.Helper()
	var metas []v1.SecretMeta
	if err := f.db.Read(f.t.Context(), func(q Querier) (err error) {
		metas, err = SecretsOfWorker(f.t.Context(), q, f.w.ID)
		return err
	}); err != nil {
		f.t.Fatal(err)
	}
	return metas
}

func refsOf(metas []v1.SecretMeta) []string {
	out := []string{}
	for _, m := range metas {
		out = append(out, m.Ref)
	}
	return out
}

func meta(ref string, projects ...string) v1.SecretMeta {
	return v1.SecretMeta{Ref: ref, Backend: v1.SecretBackendFile, Projects: projects, UpdatedAt: epoch0}
}

// A late report of a daemon that was replaced does not restore what it held,
// and within one daemon only a later seq replaces the set.
func TestReplaceWorkerSecrets_FencesByGenerationAndSeq(t *testing.T) {
	f := newSecretsFixture(t)

	steps := []struct {
		name string
		r    SecretsReport
		want SecretsOutcome
		held []string
	}{
		{"first report", SecretsReport{Generation: 1, Epoch: "e1", Seq: 1, Refs: []v1.SecretMeta{meta("b"), meta("a")}}, SecretsChanged, []string{"a", "b"}},
		{"same set, later seq", SecretsReport{Generation: 1, Epoch: "e1", Seq: 2, Refs: []v1.SecretMeta{meta("a"), meta("b")}}, SecretsUnchanged, []string{"a", "b"}},
		{"earlier seq arrives late", SecretsReport{Generation: 1, Epoch: "e1", Seq: 1, Refs: nil}, SecretsStale, []string{"a", "b"}},
		{"new daemon, empty", SecretsReport{Generation: 2, Epoch: "e2", Seq: 0, Refs: nil}, SecretsChanged, []string{}},
		{"retired daemon arrives late", SecretsReport{Generation: 1, Epoch: "e1", Seq: 9, Refs: []v1.SecretMeta{meta("a")}}, SecretsStale, []string{}},
		{"new daemon adds one", SecretsReport{Generation: 2, Epoch: "e2", Seq: 1, Refs: []v1.SecretMeta{meta("c", "project_b", "project_a")}}, SecretsChanged, []string{"c"}},
	}
	for _, s := range steps {
		if got := f.report(s.r); got != s.want {
			t.Errorf("%s: outcome %d, want %d", s.name, got, s.want)
		}
		if got := refsOf(f.held()); !slices.Equal(got, s.held) {
			t.Errorf("%s: held %v, want %v", s.name, got, s.held)
		}
	}
	if got := f.held()[0]; !slices.Equal(got.Projects, []string{"project_a", "project_b"}) || got.Backend != v1.SecretBackendFile || !got.UpdatedAt.Equal(epoch0) {
		t.Errorf("metadata read back as %+v", got)
	}
	var fence SecretsFence
	if err := f.db.Read(t.Context(), func(q Querier) (err error) {
		fence, _, err = WorkerSecretsFence(t.Context(), q, f.w.ID)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if fence.Generation != 2 || fence.Epoch != "e2" || fence.Seq != 1 {
		t.Errorf("fence = %+v, want generation 2, epoch e2, seq 1", fence)
	}
}

// The digest covers every field of the metadata, not only the refs.
func TestReplaceWorkerSecrets_RewritesWhenAnyMetadataChanges(t *testing.T) {
	f := newSecretsFixture(t)
	seq := uint64(0)
	next := func(m v1.SecretMeta) SecretsOutcome {
		seq++
		return f.report(SecretsReport{Generation: 1, Epoch: "e1", Seq: seq, Refs: []v1.SecretMeta{m}})
	}
	base := meta("a", "project_1")
	if got := next(base); got != SecretsChanged {
		t.Fatalf("first report: %d", got)
	}
	reordered := meta("a", "project_1", "project_1")
	reordered.UpdatedAt = epoch0.In(time.FixedZone("x", 3600))
	if got := next(reordered); got != SecretsUnchanged {
		t.Errorf("the same metadata, written differently, is outcome %d", got)
	}
	later := base
	later.UpdatedAt = epoch0.Add(time.Second)
	scoped := meta("a", "project_2")
	keyring := base
	keyring.Backend = "keyring"
	for name, m := range map[string]v1.SecretMeta{"timestamp": later, "scope": scoped, "backend": keyring} {
		if got := next(m); got != SecretsChanged {
			t.Errorf("a new %s is outcome %d, want changed", name, got)
		}
	}
}

// A removed Worker's secrets are gone and no late report brings them back; a
// Worker that sent none has no fence.
func TestDeleteWorkerSecrets_KeepsTheFence(t *testing.T) {
	f := newSecretsFixture(t)
	other := f.worker(2, "beta", domain.WorkerOnline)
	ctx := t.Context()
	f.report(SecretsReport{Generation: 1, Epoch: "e1", Seq: 3, Refs: []v1.SecretMeta{meta("a")}})
	if err := f.db.WithTx(ctx, func(tx *Tx) error {
		if _, err := tx.ReplaceWorkerSecrets(ctx, other.ID, SecretsReport{Generation: 1, Epoch: "o1", Seq: 1, Refs: []v1.SecretMeta{meta("a"), meta("z")}}, epoch0); err != nil {
			return err
		}
		return tx.DeleteWorkerSecrets(ctx, f.w.ID)
	}); err != nil {
		t.Fatal(err)
	}
	if got := f.report(SecretsReport{Generation: 1, Epoch: "e1", Seq: 2, Refs: []v1.SecretMeta{meta("a")}}); got != SecretsStale {
		t.Errorf("a late report after the delete is outcome %d, want stale", got)
	}
	if got := f.report(SecretsReport{Generation: 1, Epoch: "e1", Seq: 4, Refs: []v1.SecretMeta{meta("a")}}); got != SecretsChanged {
		t.Errorf("the next report, with the set held before the delete, is outcome %d, want it written again", got)
	}

	var all map[domain.ID][]v1.SecretMeta
	var withZ map[domain.ID]v1.SecretMeta
	var hasFence bool
	if err := f.db.Read(ctx, func(q Querier) (err error) {
		if all, err = WorkerSecrets(ctx, q); err != nil {
			return err
		}
		if withZ, err = WorkersWithSecret(ctx, q, "z"); err != nil {
			return err
		}
		_, hasFence, err = WorkerSecretsFence(ctx, q, "worker_none")
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if len(all) != 2 || !slices.Equal(refsOf(all[f.w.ID]), []string{"a"}) || !slices.Equal(refsOf(all[other.ID]), []string{"a", "z"}) {
		t.Errorf("WorkerSecrets = %v", all)
	}
	if _, ok := withZ[other.ID]; len(withZ) != 1 || !ok {
		t.Errorf("WorkersWithSecret(z) = %v, want only %s", withZ, other.ID)
	}
	if hasFence {
		t.Error("a Worker that never reported has a fence")
	}

	if err := f.db.WithTx(ctx, func(tx *Tx) error {
		_, err := tx.SetWorkerStatus(ctx, other.ID, other.Status(), domain.WorkerStatus{State: domain.WorkerRemoved}, epoch0)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if err := f.db.Read(ctx, func(q Querier) (err error) {
		if all, err = WorkerSecrets(ctx, q); err != nil {
			return err
		}
		withZ, err = WorkersWithSecret(ctx, q, "z")
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if _, ok := all[other.ID]; ok || len(withZ) != 0 {
		t.Errorf("a removed Worker still offers its secrets: %v, %v", all, withZ)
	}
}

// A denied grant commits its trail: one audit entry per ref, and the verdict
// of each ref read back.
func TestRecordSecretGrant_AuditsEveryRefOfADenial(t *testing.T) {
	f := newLeaseFixture(t)
	ctx := t.Context()
	run, lease := f.acceptedRun(1)
	at := epoch0.Add(time.Minute)
	grant := func(verdicts ...SecretVerdict) SecretGrant {
		return SecretGrant{
			RunID: run.ID, LeaseID: lease.ID, LeaseEpoch: lease.Epoch, WorkerID: lease.WorkerID,
			WorkerEpoch: "e1", Refs: verdicts, DecidedAt: at,
		}
	}
	denied := grant(SecretVerdict{Ref: "npm"}, SecretVerdict{Ref: "github/token", Denial: v1.SecretNotAllowlisted})
	granted := grant(SecretVerdict{Ref: "npm"})
	f.tx(func(tx *Tx) error {
		if _, err := tx.RecordSecretGrant(ctx, denied); err != nil {
			return err
		}
		_, err := tx.RecordSecretGrant(ctx, granted)
		return err
	})

	var grants []SecretGrant
	if err := f.db.Read(ctx, func(q Querier) (err error) {
		grants, err = SecretGrantsOf(ctx, q, run.ID)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if len(grants) != 2 || grants[0].Granted() || !grants[1].Granted() {
		t.Fatalf("grants = %+v, want the denied one, then the granted one", grants)
	}
	if g := grants[0]; g.LeaseID != lease.ID || g.LeaseEpoch != lease.Epoch || g.WorkerID != lease.WorkerID || g.WorkerEpoch != "e1" ||
		!g.DecidedAt.Equal(at) || !slices.Equal(g.Refs, []SecretVerdict{{Ref: "github/token", Denial: v1.SecretNotAllowlisted}, {Ref: "npm"}}) {
		t.Errorf("denied grant read back as %+v", g)
	}

	rows, err := f.db.Reader().QueryContext(ctx,
		`SELECT actor_kind, actor_id, resource_kind, resource_id, result, after FROM audit_entries WHERE action = ? ORDER BY id`, AuditSecretGrant)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rows.Close() }()
	var results []string
	for rows.Next() {
		var actorKind, actorID, kind, resource, result, after string
		if err := rows.Scan(&actorKind, &actorID, &kind, &resource, &result, &after); err != nil {
			t.Fatal(err)
		}
		if actorKind != string(domain.ActorWorker) || actorID != string(lease.WorkerID) || kind != "run" || resource != string(run.ID) {
			t.Errorf("audit entry by %s %s on %s %s", actorKind, actorID, kind, resource)
		}
		if !strings.Contains(after, `"lease_epoch":`) || !strings.Contains(after, `"worker_epoch":"e1"`) {
			t.Errorf("audit after = %s", after)
		}
		results = append(results, result)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	if want := []string{"denied", "denied:" + v1.SecretNotAllowlisted, "granted"}; !slices.Equal(results, want) {
		t.Errorf("audit results = %v, want %v: one per ref asked for", results, want)
	}
}

// Recording a grant inside a transaction that fails leaves nothing behind:
// the grant and its audit are one write.
func TestRecordSecretGrant_RollsBackWithItsTransaction(t *testing.T) {
	f := newLeaseFixture(t)
	ctx := t.Context()
	run, lease := f.acceptedRun(1)
	audits := count(t, f.db, "audit_entries")
	err := f.db.WithTx(ctx, func(tx *Tx) error {
		if _, err := tx.RecordSecretGrant(ctx, SecretGrant{
			RunID: run.ID, LeaseID: lease.ID, LeaseEpoch: lease.Epoch, WorkerID: lease.WorkerID,
			WorkerEpoch: "e1", Refs: []SecretVerdict{{Ref: "npm"}}, DecidedAt: epoch0,
		}); err != nil {
			return err
		}
		return ErrNoRowChanged
	})
	if err == nil {
		t.Fatal("the transaction did not fail")
	}
	for _, table := range []string{"secret_grants", "secret_grant_refs"} {
		if n := count(t, f.db, table); n != 0 {
			t.Errorf("%s has %d rows after the rollback", table, n)
		}
	}
	if n := count(t, f.db, "audit_entries"); n != audits {
		t.Errorf("the rollback left %d audit entries, want %d", n, audits)
	}
}
