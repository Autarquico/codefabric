package gc

import (
	"context"
	"slices"
	"strings"
	"time"

	v1 "github.com/Autarquico/codefabric/internal/api/v1"
	"github.com/Autarquico/codefabric/internal/controller/store"
	"github.com/Autarquico/codefabric/internal/domain"
	"github.com/Autarquico/codefabric/internal/support/flock"
)

// relayRefsKind is the item of the report for the refs of the relay.
const relayRefsKind = "relay_refs"

// Relay is the git relay as the GC uses it (§29). *relay.Relay implements it.
//
// It lists only the refs the GC may ever delete (branches cf/*/* and orphans);
// DeleteRefs refuses any other, so a namespace the GC does not know, such as
// refs/cf/snapshots/*, is out of reach by construction; the refs of checkpoints are deleted by name.
type Relay interface {
	// Refs lists by Project the full names of those refs: refs/heads/cf/<task>/<run>
	// and refs/cf/orphans/<run>-e<epoch>.
	Refs(ctx context.Context) (map[domain.ID][]string, error)
	DeleteRefs(ctx context.Context, project domain.ID, refs []string) error
	// Collect runs git gc on the repository of the Project, with git's own
	// prune grace. It does nothing, and says so, while a push is in flight.
	Collect(ctx context.Context, project domain.ID) (collected bool, err error)
}

// WithRelay makes the pass delete the refs of archived Tasks from the relay.
func WithRelay(r Relay) Option { return func(s *Service) { s.relay = r } }

// archivedRefs are the names the relay holds for the Runs of the Tasks archived
// before the cutoff: their branches by Project, and the Runs whose orphans go.
type archivedRefs struct {
	branches map[domain.ID]map[string]bool // project -> "refs/heads/cf/<task>/<run>"
	runs     map[string]bool               // run ID -> orphan refs
}

// archivedRuns reads the Runs of Tasks archived at or before cutoff. A Run
// whose publication is pending is left: its result may still be on its way.
func archivedRuns(ctx context.Context, q store.Querier, cutoff time.Time) (archivedRefs, error) {
	out := archivedRefs{branches: map[domain.ID]map[string]bool{}, runs: map[string]bool{}}
	rows, err := store.ArchivedRuns(ctx, q, cutoff)
	if err != nil {
		return out, err
	}
	for _, r := range rows {
		branch := domain.RunBranch(r.Branch, r.TaskName,
			domain.ShortID{Kind: domain.KindTask, N: r.TaskShort}, domain.ShortID{Kind: domain.KindRun, N: r.RunShort})
		if out.branches[r.Project] == nil {
			out.branches[r.Project] = map[string]bool{}
		}
		out.branches[r.Project]["refs/heads/"+branch] = true
		out.runs[r.RunID] = true
	}
	return out, nil
}

func (a archivedRefs) owns(project domain.ID, name string) bool {
	if a.branches[project][name] {
		return true
	}
	rest, ok := strings.CutPrefix(name, "refs/cf/orphans/")
	if !ok {
		return false
	}
	run, _, ok := domain.ParseRunEpoch(rest)
	return ok && a.runs[run]
}

// relayGarbage is the refs to delete, by Project and in a stable order.
func (s *Service) relayGarbage(ctx context.Context, now time.Time) (map[domain.ID][]string, error) {
	var archived archivedRefs
	if err := s.db.Read(ctx, func(q store.Querier) (err error) {
		archived, err = archivedRuns(ctx, q, now.Add(-s.policy.RelayBranches))
		return err
	}); err != nil {
		return nil, err
	}
	refs, err := s.relay.Refs(ctx)
	if err != nil {
		return nil, err
	}
	out := map[domain.ID][]string{}
	for project, names := range refs {
		for _, name := range names {
			if archived.owns(project, name) {
				out[project] = append(out[project], name)
			}
		}
	}
	for _, names := range out {
		slices.Sort(names)
	}
	return out, nil
}

// planRelay is the dry run of sweepRelay.
func (s *Service) planRelay(ctx context.Context, now time.Time) (v1.GCCount, error) {
	garbage, err := s.relayGarbage(ctx, now)
	if err != nil {
		return v1.GCCount{}, err
	}
	n := 0
	for _, names := range garbage {
		n += len(names)
	}
	return v1.GCCount{Kind: relayRefsKind, Count: int64(n)}, nil
}

// sweepRelay deletes the refs of archived Tasks a batch at a time, each batch
// under the lock of the backups, and then collects the repositories it
// deleted from. The ids of the refs come from the database; nothing is deleted
// for being unreferenced.
func (s *Service) sweepRelay(ctx context.Context, rep *v1.GCReport) error {
	done := v1.GCCount{Kind: relayRefsKind}
	defer func() { rep.Items = append(rep.Items, done) }()
	garbage, err := s.relayGarbage(ctx, s.clock.Now())
	if err != nil {
		return err
	}
	projects := make([]domain.ID, 0, len(garbage))
	for p := range garbage {
		projects = append(projects, p)
	}
	slices.Sort(projects)
	for _, project := range projects {
		for batch := range chunks(garbage[project], s.policy.BatchSize) {
			if err := s.deleteRefs(ctx, project, batch); err != nil {
				return err
			}
			done.Count += int64(len(batch))
			if s.hooks.AfterBatch != nil {
				s.hooks.AfterBatch(relayRefsKind, int64(len(batch)))
			}
			if err := yield(ctx); err != nil {
				return err
			}
		}
		if err := s.collect(ctx, project); err != nil {
			return err
		}
	}
	return nil
}

func (s *Service) deleteRefs(ctx context.Context, project domain.ID, refs []string) error {
	release, err := s.lock()
	if err != nil {
		return err
	}
	defer release()
	return s.relay.DeleteRefs(ctx, project, refs)
}

func (s *Service) collect(ctx context.Context, project domain.ID) error {
	release, err := s.lock()
	if err != nil {
		return err
	}
	defer release()
	_, err = s.relay.Collect(ctx, project)
	return err
}

// lock takes the lock of the backups, if there is one.
func (s *Service) lock() (release func(), err error) {
	if s.lockPath == "" {
		return func() {}, nil
	}
	return flock.Lock(s.lockPath)
}
