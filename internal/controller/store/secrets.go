package store

import (
	"cmp"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"time"

	v1 "github.com/Autarquico/codefabric/internal/api/v1"
	"github.com/Autarquico/codefabric/internal/domain"
)

// Secrets in the store are refs and metadata only (§16.7): nothing here takes
// or returns a value, a hash of one or its length.

// AllowSecrets adds refs to the allowlist of the Project and returns those it
// did not hold yet, sorted.
func (t *Tx) AllowSecrets(ctx context.Context, project domain.ID, refs []string, by string, at time.Time) ([]string, error) {
	var added []string
	for _, ref := range sortedSet(refs) {
		res, err := t.Exec(ctx,
			`INSERT INTO project_secret_allowlist (project_id, ref, added_at, added_by) VALUES (?, ?, ?, ?)
			 ON CONFLICT (project_id, ref) DO NOTHING`,
			string(project), ref, at.UnixNano(), by)
		if err != nil {
			return nil, fmt.Errorf("store: allowing a secret: %w", err)
		}
		if changed, err := rowsChanged(res); err != nil {
			return nil, err
		} else if changed {
			added = append(added, ref)
		}
	}
	return added, nil
}

// DenySecrets takes refs out of the allowlist of the Project and returns those
// it held, sorted.
func (t *Tx) DenySecrets(ctx context.Context, project domain.ID, refs []string) ([]string, error) {
	var removed []string
	for _, ref := range sortedSet(refs) {
		res, err := t.Exec(ctx, `DELETE FROM project_secret_allowlist WHERE project_id = ? AND ref = ?`, string(project), ref)
		if err != nil {
			return nil, fmt.Errorf("store: denying a secret: %w", err)
		}
		if changed, err := rowsChanged(res); err != nil {
			return nil, err
		} else if changed {
			removed = append(removed, ref)
		}
	}
	return removed, nil
}

// ProjectAllowlist is the allowlist of the Project, sorted.
func ProjectAllowlist(ctx context.Context, q Querier, project domain.ID) ([]string, error) {
	lists, err := AllowlistsFor(ctx, q, []domain.ID{project})
	return lists[project], err
}

// AllowlistsFor is the allowlist of each of the Projects, sorted; a Project
// that allows nothing has no entry.
func AllowlistsFor(ctx context.Context, q Querier, projects []domain.ID) (map[domain.ID][]string, error) {
	out := make(map[domain.ID][]string, len(projects))
	if len(projects) == 0 {
		return out, nil
	}
	args := make([]any, len(projects))
	for i, p := range projects {
		args[i] = string(p)
	}
	rows, err := q.Query(ctx,
		`SELECT project_id, ref FROM project_secret_allowlist WHERE project_id IN (`+placeholders(len(projects))+`)
		 ORDER BY project_id, ref`, args...)
	if err != nil {
		return nil, fmt.Errorf("store: reading allowlists: %w", err)
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var project, ref string
		if err := rows.Scan(&project, &ref); err != nil {
			return nil, scanErr(err, "allowlist")
		}
		out[domain.ID(project)] = append(out[domain.ID(project)], ref)
	}
	return out, rows.Err()
}

// SecretsReport is the whole set of secrets a daemon says it holds, with the
// position of that report among those of the Worker: Generation orders the
// runs of its daemon (0 for one that predates generations), Seq the reports of
// one run.
type SecretsReport struct {
	Generation uint64
	Epoch      string
	Seq        uint64
	Refs       []v1.SecretMeta
}

// SecretsFence is the position of the latest report taken from a Worker.
type SecretsFence struct {
	Generation uint64
	Epoch      string
	Seq        uint64
	ReportedAt time.Time
}

// Retired says whether a daemon of this generation was replaced by the one
// that sent the fence: its heartbeats must not be taken at all. A generation 0
// daemon is retired once any daemon with a generation has reported.
func (f SecretsFence) Retired(generation uint64) bool { return generation < f.Generation }

// Admits says whether a report at r comes after the fence. Between daemons
// with a generation the order is (Generation, Seq); between two that predate
// generations, a new epoch is a new daemon and within one epoch Seq orders.
func (f SecretsFence) Admits(r SecretsReport) bool {
	switch {
	case f.Retired(r.Generation):
		return false
	case r.Generation > f.Generation:
		return true
	case r.Generation == 0 && r.Epoch != f.Epoch:
		return true
	default:
		return r.Seq > f.Seq
	}
}

// SecretsOutcome is what ReplaceWorkerSecrets did with a report.
type SecretsOutcome int

const (
	// SecretsStale is a report that is not after the fence; nothing changed.
	SecretsStale SecretsOutcome = iota + 1
	// SecretsUnchanged is a report the fence moved to, whose metadata is the
	// one already kept.
	SecretsUnchanged
	// SecretsChanged is a report the fence moved to, whose metadata replaced
	// what was kept.
	SecretsChanged
)

// WorkerSecretsFence is the fence of the Worker, and false if it never sent a
// report.
func WorkerSecretsFence(ctx context.Context, q Querier, worker domain.ID) (SecretsFence, bool, error) {
	f, _, ok, err := secretsFence(ctx, q, worker)
	return f, ok, err
}

func secretsFence(ctx context.Context, q Querier, worker domain.ID) (fence SecretsFence, digest string, ok bool, err error) {
	var (
		stored   sql.NullString
		reported int64
	)
	err = q.QueryRow(ctx,
		`SELECT generation, epoch, seq, metadata_digest, reported_at FROM worker_secret_reports WHERE worker_id = ?`,
		string(worker)).Scan(&fence.Generation, &fence.Epoch, &fence.Seq, &stored, &reported)
	if err == sql.ErrNoRows {
		return SecretsFence{}, "", false, nil
	}
	if err != nil {
		return SecretsFence{}, "", false, fmt.Errorf("store: reading the secrets fence of a worker: %w", err)
	}
	fence.ReportedAt = time.Unix(0, reported).UTC()
	return fence, stored.String, true, nil
}

// ReplaceWorkerSecrets takes a report as the whole set of secrets of the Worker
// if it comes after the fence (compare-and-set on (generation, seq)), and
// rewrites the metadata only when it differs from what is kept. The caller
// validates the report.
func (t *Tx) ReplaceWorkerSecrets(ctx context.Context, worker domain.ID, r SecretsReport, at time.Time) (SecretsOutcome, error) {
	fence, stored, ok, err := secretsFence(ctx, t, worker)
	if err != nil {
		return 0, err
	}
	if ok && !fence.Admits(r) {
		return SecretsStale, nil
	}
	metas := canonicalSecrets(r.Refs)
	digest, err := secretsDigest(metas)
	if err != nil {
		return 0, err
	}
	if _, err := t.Exec(ctx,
		`INSERT INTO worker_secret_reports (worker_id, generation, epoch, seq, metadata_digest, reported_at)
		 VALUES (?, ?, ?, ?, ?, ?)
		 ON CONFLICT (worker_id) DO UPDATE SET generation = excluded.generation, epoch = excluded.epoch,
		     seq = excluded.seq, metadata_digest = excluded.metadata_digest, reported_at = excluded.reported_at`,
		string(worker), r.Generation, r.Epoch, r.Seq, digest, at.UnixNano()); err != nil {
		return 0, fmt.Errorf("store: recording a secrets report: %w", err)
	}
	if ok && stored == digest {
		return SecretsUnchanged, nil
	}
	if err := t.writeWorkerSecrets(ctx, worker, metas); err != nil {
		return 0, err
	}
	return SecretsChanged, nil
}

func (t *Tx) writeWorkerSecrets(ctx context.Context, worker domain.ID, metas []v1.SecretMeta) error {
	if _, err := t.Exec(ctx, `DELETE FROM worker_secrets WHERE worker_id = ?`, string(worker)); err != nil {
		return fmt.Errorf("store: replacing the secrets of a worker: %w", err)
	}
	for _, m := range metas {
		projects, err := json.Marshal(nonNilStrings(m.Projects))
		if err != nil {
			return err
		}
		if _, err := t.Exec(ctx,
			`INSERT INTO worker_secrets (worker_id, ref, backend, projects, updated_at) VALUES (?, ?, ?, ?, ?)`,
			string(worker), m.Ref, m.Backend, string(projects), m.UpdatedAt.UnixNano()); err != nil {
			return fmt.Errorf("store: replacing the secrets of a worker: %w", err)
		}
	}
	return nil
}

// DeleteWorkerSecrets forgets the secrets of a Worker, as when it is removed.
// Its fence stays, so no late heartbeat brings them back, but the next report
// it admits is written in full.
func (t *Tx) DeleteWorkerSecrets(ctx context.Context, worker domain.ID) error {
	if _, err := t.Exec(ctx, `DELETE FROM worker_secrets WHERE worker_id = ?`, string(worker)); err != nil {
		return fmt.Errorf("store: deleting the secrets of a worker: %w", err)
	}
	if _, err := t.Exec(ctx, `UPDATE worker_secret_reports SET metadata_digest = NULL WHERE worker_id = ?`, string(worker)); err != nil {
		return fmt.Errorf("store: deleting the secrets of a worker: %w", err)
	}
	return nil
}

// SecretsOfWorker is the metadata the Worker last reported, sorted by ref.
func SecretsOfWorker(ctx context.Context, q Querier, worker domain.ID) ([]v1.SecretMeta, error) {
	byWorker, err := readWorkerSecrets(ctx, q, `WHERE s.worker_id = ?`, string(worker))
	return byWorker[worker], err
}

// WorkerSecrets is the metadata of every Worker that is not removed, sorted by
// ref; a Worker that holds none has no entry.
func WorkerSecrets(ctx context.Context, q Querier) (map[domain.ID][]v1.SecretMeta, error) {
	return readWorkerSecrets(ctx, q, `WHERE w.removed_at IS NULL`)
}

// WorkersWithSecret is, for every Worker not removed that holds ref, its
// metadata; the caller checks its Project scope.
func WorkersWithSecret(ctx context.Context, q Querier, ref string) (map[domain.ID]v1.SecretMeta, error) {
	byWorker, err := readWorkerSecrets(ctx, q, `WHERE w.removed_at IS NULL AND s.ref = ?`, ref)
	out := make(map[domain.ID]v1.SecretMeta, len(byWorker))
	for id, metas := range byWorker {
		out[id] = metas[0]
	}
	return out, err
}

func readWorkerSecrets(ctx context.Context, q Querier, where string, args ...any) (map[domain.ID][]v1.SecretMeta, error) {
	rows, err := q.Query(ctx,
		`SELECT s.worker_id, s.ref, s.backend, s.projects, s.updated_at
		 FROM worker_secrets s JOIN workers w ON w.id = s.worker_id `+where+` ORDER BY s.worker_id, s.ref`, args...)
	if err != nil {
		return nil, fmt.Errorf("store: reading the secrets of workers: %w", err)
	}
	defer func() { _ = rows.Close() }()
	out := map[domain.ID][]v1.SecretMeta{}
	for rows.Next() {
		var (
			worker, projects string
			updated          int64
			m                v1.SecretMeta
		)
		if err := rows.Scan(&worker, &m.Ref, &m.Backend, &projects, &updated); err != nil {
			return nil, scanErr(err, "worker secret")
		}
		if err := json.Unmarshal([]byte(projects), &m.Projects); err != nil {
			return nil, fmt.Errorf("store: worker %s has a corrupt scope for %s: %w", worker, m.Ref, err)
		}
		if len(m.Projects) == 0 {
			m.Projects = nil
		}
		m.UpdatedAt = time.Unix(0, updated).UTC()
		out[domain.ID(worker)] = append(out[domain.ID(worker)], m)
	}
	return out, rows.Err()
}

// canonicalSecrets is metas sorted by ref, each scope sorted and every time in
// UTC at the precision kept, so that equal sets have equal digests.
func canonicalSecrets(metas []v1.SecretMeta) []v1.SecretMeta {
	out := make([]v1.SecretMeta, len(metas))
	for i, m := range metas {
		m.Projects = sortedSet(m.Projects)
		m.UpdatedAt = time.Unix(0, m.UpdatedAt.UnixNano()).UTC()
		out[i] = m
	}
	slices.SortFunc(out, func(a, b v1.SecretMeta) int { return cmp.Compare(a.Ref, b.Ref) })
	return out
}

// secretsDigest identifies a canonical set of metadata: refs, backends, scopes
// and timestamps. There is no value in it to hash.
func secretsDigest(metas []v1.SecretMeta) (string, error) {
	raw, err := json.Marshal(metas)
	if err != nil {
		return "", fmt.Errorf("store: digesting secrets metadata: %w", err)
	}
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:]), nil
}

// SecretGrant is one grant a Worker asked for: the refs of a Run it holds under
// a lease, as the daemon of WorkerEpoch. Refs keeps each ref with its Denial,
// "" if the ref itself passed. A grant is all or nothing: it grants every ref
// or, if any is denied, none.
type SecretGrant struct {
	ID          int64
	RunID       domain.ID
	LeaseID     domain.ID
	LeaseEpoch  uint64
	WorkerID    domain.ID
	WorkerEpoch string
	Refs        []SecretVerdict
	DecidedAt   time.Time
}

// SecretVerdict is the decision on one ref of a grant.
type SecretVerdict struct {
	Ref    string
	Denial string
}

// Granted says whether no ref of the grant is denied.
func (g SecretGrant) Granted() bool {
	return !slices.ContainsFunc(g.Refs, func(v SecretVerdict) bool { return v.Denial != "" })
}

// AuditSecretGrant is the action of the audit entries of a grant.
const AuditSecretGrant = "secret.grant"

// RecordSecretGrant keeps a grant and one audit entry per ref, in the
// transaction, so the trail of a denial commits with it. Each entry's result
// is "granted"; "denied:<reason>" for a ref that was denied; or "denied" for
// one that passed in a grant that another ref denied. The caller validates
// the refs and decides the verdicts; it returns the ID of the grant.
func (t *Tx) RecordSecretGrant(ctx context.Context, g SecretGrant) (int64, error) {
	granted := g.Granted()
	var id int64
	if err := t.QueryRow(ctx,
		`INSERT INTO secret_grants (run_id, lease_id, lease_epoch, worker_id, worker_epoch, granted, decided_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?) RETURNING id`,
		string(g.RunID), string(g.LeaseID), g.LeaseEpoch, string(g.WorkerID), g.WorkerEpoch, granted, g.DecidedAt.UnixNano(),
	).Scan(&id); err != nil {
		return 0, fmt.Errorf("store: recording a secret grant: %w", err)
	}
	for _, v := range g.Refs {
		if _, err := t.Exec(ctx, `INSERT INTO secret_grant_refs (grant_id, ref, denial) VALUES (?, ?, ?)`,
			id, v.Ref, nullable(v.Denial)); err != nil {
			return 0, fmt.Errorf("store: recording a secret grant: %w", err)
		}
		if err := t.AppendAudit(ctx, AuditEntry{
			Time: g.DecidedAt, ActorKind: domain.ActorWorker, ActorID: string(g.WorkerID),
			Action: AuditSecretGrant, ResourceKind: "run", ResourceID: string(g.RunID),
			After: secretGrantAudit{
				Grant: id, Ref: v.Ref, WorkerID: string(g.WorkerID), WorkerEpoch: g.WorkerEpoch,
				LeaseID: string(g.LeaseID), LeaseEpoch: g.LeaseEpoch,
			},
			Result: grantResult(granted, v.Denial),
		}); err != nil {
			return 0, err
		}
	}
	return id, nil
}

type secretGrantAudit struct {
	Grant       int64  `json:"grant"`
	Ref         string `json:"ref"`
	WorkerID    string `json:"worker_id"`
	WorkerEpoch string `json:"worker_epoch"`
	LeaseID     string `json:"lease_id"`
	LeaseEpoch  uint64 `json:"lease_epoch"`
}

func grantResult(granted bool, denial string) string {
	switch {
	case granted:
		return "granted"
	case denial != "":
		return "denied:" + denial
	default:
		return "denied"
	}
}

// SecretGrantsOf lists the grants of the Run, oldest first, each with its refs
// sorted.
func SecretGrantsOf(ctx context.Context, q Querier, run domain.ID) ([]SecretGrant, error) {
	rows, err := q.Query(ctx,
		`SELECT g.id, g.run_id, g.lease_id, g.lease_epoch, g.worker_id, g.worker_epoch, g.decided_at, r.ref, COALESCE(r.denial, '')
		 FROM secret_grants g JOIN secret_grant_refs r ON r.grant_id = g.id
		 WHERE g.run_id = ? ORDER BY g.id, r.ref`, string(run))
	if err != nil {
		return nil, fmt.Errorf("store: reading the secret grants of a run: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []SecretGrant
	for rows.Next() {
		var (
			g                    SecretGrant
			runID, lease, worker string
			decided              int64
			verdict              SecretVerdict
		)
		if err := rows.Scan(&g.ID, &runID, &lease, &g.LeaseEpoch, &worker, &g.WorkerEpoch, &decided, &verdict.Ref, &verdict.Denial); err != nil {
			return nil, scanErr(err, "secret grant")
		}
		if n := len(out); n == 0 || out[n-1].ID != g.ID {
			g.RunID, g.LeaseID, g.WorkerID = domain.ID(runID), domain.ID(lease), domain.ID(worker)
			g.DecidedAt = time.Unix(0, decided).UTC()
			out = append(out, g)
		}
		out[len(out)-1].Refs = append(out[len(out)-1].Refs, verdict)
	}
	return out, rows.Err()
}

// sortedSet is s sorted without repeats, nil when empty.
func sortedSet(s []string) []string {
	if len(s) == 0 {
		return nil
	}
	out := slices.Clone(s)
	slices.Sort(out)
	return slices.Compact(out)
}

func placeholders(n int) string {
	return strings.TrimSuffix(strings.Repeat("?, ", n), ", ")
}
