package relay

import (
	"context"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/Autarquico/codefabric/internal/domain"
)

// collectTimeout bounds one git gc. It is an infrastructure deadline on a
// child process, so it runs on real time.
const collectTimeout = 30 * time.Minute

// deletable says whether the GC may delete ref: a branch cf/<task>/<run>, the
// ref of an orphan, or the ref of a checkpoint (the GC of checkpoints names
// those by the rows it deletes; Refs does not list them). Every other namespace
// (refs/cf/snapshots/*, whatever a later phase adds) is not its to delete.
func deletable(ref string) bool {
	if branch, ok := strings.CutPrefix(ref, "refs/heads/"); ok {
		return domain.ValidBranch(branch)
	}
	return domain.ValidOrphanRef(ref) || domain.ValidCheckpointRef(ref)
}

// Refs lists, by Project, the refs of its repository that the GC may delete.
func (r *Relay) Refs(ctx context.Context) (map[domain.ID][]string, error) {
	entries, err := os.ReadDir(r.cfg.Dir)
	if err != nil {
		return nil, fmt.Errorf("relay: reading %s: %w", r.cfg.Dir, err)
	}
	out := map[domain.ID][]string{}
	for _, e := range entries {
		id, ok := strings.CutSuffix(e.Name(), ".git")
		if !e.IsDir() || !ok || !isProjectID(id) {
			continue
		}
		project := domain.ID(id)
		listed, err := r.run(ctx, verifyTimeout, r.RepoDir(project), "-C", r.RepoDir(project),
			"for-each-ref", "--format=%(refname)", "refs/heads/cf/", "refs/cf/orphans/")
		if err != nil {
			return nil, err
		}
		for name := range strings.SplitSeq(listed, "\n") {
			if deletable(name) {
				out[project] = append(out[project], name)
			}
		}
	}
	return out, nil
}

func isProjectID(id string) bool {
	kind, _, err := domain.ParseID(id)
	return err == nil && kind == domain.KindProject
}

// DeleteRefs deletes refs of the repository of the Project. It refuses, before
// deleting any, a ref that is not one the GC may delete.
func (r *Relay) DeleteRefs(ctx context.Context, project domain.ID, refs []string) error {
	if !isProjectID(string(project)) {
		return fmt.Errorf("relay: %q is not a Project ID", project)
	}
	for _, ref := range refs {
		if !deletable(ref) {
			return fmt.Errorf("relay: %q is not a ref the garbage collector deletes", ref)
		}
	}
	dir := r.RepoDir(project)
	if !exists(dir) {
		return nil
	}
	for _, ref := range refs {
		if _, err := r.run(ctx, verifyTimeout, dir, "-C", dir, "update-ref", "-d", ref); err != nil {
			return err
		}
	}
	return nil
}

// Collect runs git gc on the repository of the Project, with git's own prune
// grace (never --prune=now): what a push in flight has just written is younger
// than it. While any push is in flight it does nothing and says so.
func (r *Relay) Collect(ctx context.Context, project domain.ID) (bool, error) {
	if !isProjectID(string(project)) {
		return false, fmt.Errorf("relay: %q is not a Project ID", project)
	}
	dir := r.RepoDir(project)
	if !exists(dir) {
		return false, nil
	}
	r.mu.Lock()
	busy := len(r.pushing) > 0
	r.mu.Unlock()
	if busy {
		return false, nil
	}
	if _, err := r.run(ctx, collectTimeout, dir, "-C", dir, "gc", "--quiet"); err != nil {
		return false, err
	}
	return true, nil
}
