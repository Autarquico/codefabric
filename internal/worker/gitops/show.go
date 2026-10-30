package gitops

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/Autarquico/codefabric/internal/domain"
)

// MaxShowBytes is what Show reads of a file, plus one byte so that the caller
// can tell a file over the limit from one that fits.
const MaxShowBytes = 64<<10 + 1

// ErrNotRegular means the path at the commit is not a regular file.
var ErrNotRegular = errors.New("gitops: the path is not a regular file at that commit")

// entry is a line of git ls-tree.
type entry struct{ mode, oid string }

// lsTree reads the entries of paths at commit in the bare repository of the
// Project. A path the commit does not have is not in the result; a git that
// fails is an error.
func (g *Git) lsTree(ctx context.Context, project, commit string, paths []string) (map[string]entry, error) {
	switch {
	case !isProject(project):
		return nil, errors.New("gitops: the Project is not a Project ID")
	case !domain.ValidCommitID(commit):
		return nil, errors.New("gitops: the commit is not a full commit ID")
	}
	bare := g.o.Paths.WorkerRepoDir(project)
	res, err := g.run(ctx, cmd{
		dir: bare, env: isolatedEnv(bare), timeout: quickTimeout,
		args: append([]string{"ls-tree", "-z", commit, "--"}, paths...),
	})
	if err != nil {
		return nil, err
	}
	if res.exit != 0 {
		return nil, fmt.Errorf("gitops: git ls-tree failed (%d): %s", res.exit, strings.TrimSpace(res.stderr))
	}
	out := map[string]entry{}
	for _, rec := range strings.Split(res.stdout, "\x00") {
		if rec == "" {
			continue
		}
		meta, path, ok := strings.Cut(rec, "\t")
		f := strings.Fields(meta)
		if !ok || len(f) != 3 {
			return nil, fmt.Errorf("gitops: git ls-tree answered something unexpected")
		}
		out[path] = entry{mode: f[0], oid: f[2]}
	}
	return out, nil
}

// Show reads the file at path in the commit, up to MaxShowBytes. found is
// false when the commit has no such path, which is no error; an error is a
// git that failed, or ErrNotRegular for a directory, a link or a submodule.
// The commit must be there already (EnsureSource).
func (g *Git) Show(ctx context.Context, project, commit, path string) (data []byte, found bool, err error) {
	if err := validRepoPath(path); err != nil {
		return nil, false, err
	}
	entries, err := g.lsTree(ctx, project, commit, []string{path})
	if err != nil {
		return nil, false, err
	}
	e, ok := entries[path]
	if !ok {
		return nil, false, nil
	}
	if e.mode != "100644" && e.mode != "100755" {
		return nil, true, ErrNotRegular
	}
	bare := g.o.Paths.WorkerRepoDir(project)
	res, err := g.run(ctx, cmd{
		dir: bare, env: isolatedEnv(bare), timeout: quickTimeout, maxOut: MaxShowBytes,
		args: []string{"cat-file", "blob", e.oid},
	})
	if err != nil {
		return nil, true, err
	}
	if res.exit != 0 {
		return nil, true, fmt.Errorf("gitops: git cat-file failed (%d): %s", res.exit, strings.TrimSpace(res.stderr))
	}
	return []byte(res.stdout), true, nil
}

// BlobIDs says the git object id of each path at the commit. A path the commit
// does not have is not in the map: it is the caller's to call absent.
func (g *Git) BlobIDs(ctx context.Context, project, commit string, paths []string) (map[string]string, error) {
	out := map[string]string{}
	if len(paths) == 0 {
		return out, nil
	}
	for _, p := range paths {
		if err := validRepoPath(p); err != nil {
			return nil, err
		}
	}
	entries, err := g.lsTree(ctx, project, commit, paths)
	if err != nil {
		return nil, err
	}
	for p, e := range entries {
		out[p] = e.oid
	}
	return out, nil
}

func validRepoPath(p string) error {
	if p == "" || strings.HasPrefix(p, "/") || strings.HasPrefix(p, "-") || bytes.ContainsAny([]byte(p), "\x00\\") {
		return fmt.Errorf("gitops: %q is not a path of the repository", p)
	}
	return nil
}
