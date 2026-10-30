package client

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/http"
	"os/exec"
	"strings"

	v1 "github.com/Autarquico/codefabric/internal/api/v1"
	"github.com/Autarquico/codefabric/internal/client/gitctx"
	"github.com/Autarquico/codefabric/internal/domain"
	"github.com/Autarquico/codefabric/internal/repoconfig"
	"github.com/Autarquico/codefabric/internal/support/redact"
)

// ExplainProject asks what the configuration of a Project is at req.Commit.
// If the answer lists NeedBlobs, repeat the call with their blob ids.
func (c *Client) ExplainProject(ctx context.Context, ref string, req v1.ExplainProjectRequest) (v1.ProjectExplanation, error) {
	var out v1.ProjectExplanation
	err := c.do(ctx, http.MethodPost, refPath("projects", ref, "explain"), req, &out)
	return out, err
}

// ExplainProjectAt explains the Project at the commit that ref (default: its
// default branch) has in the clone at dir. The clone must be of the Project
// (its origin is one of the Project's URLs, as for NewTask) and is the only
// thing read: the Controller never touches the origin (ADR 0002 D1).
//
// The first request carries the file of the repo; if the Controller says it
// needs blob ids of digest inputs (it knows them only once the file and the
// Environment are merged), the client looks them up in the commit and asks
// again. That is at most two calls and the client never parses the file.
func (c *Client) ExplainProjectAt(ctx context.Context, dir, projectRef, ref string) (v1.ProjectExplanation, error) {
	project, err := c.Project(ctx, projectRef)
	if err != nil {
		return v1.ProjectExplanation{}, err
	}
	if err := checkClone(ctx, dir, project); err != nil {
		return v1.ProjectExplanation{}, err
	}
	commit, err := commitOf(ctx, dir, first(ref, project.DefaultBranch))
	if err != nil {
		return v1.ProjectExplanation{}, err
	}
	req := v1.ExplainProjectRequest{Commit: commit}
	if err := readRepoFile(ctx, dir, commit, project.RepoConfig, &req); err != nil {
		return v1.ProjectExplanation{}, err
	}
	out, err := c.ExplainProject(ctx, projectRef, req)
	if err != nil || len(out.NeedBlobs) == 0 {
		return out, err
	}
	req.BlobIDs = make(map[string]string, len(out.NeedBlobs))
	for _, path := range out.NeedBlobs {
		if req.BlobIDs[path], err = blobID(ctx, dir, commit, path); err != nil {
			return v1.ProjectExplanation{}, err
		}
	}
	return c.ExplainProject(ctx, projectRef, req)
}

// readRepoFile puts the file of the repo in req. With repo_config deny the
// Worker ignores the file, so here too: only whether the commit has an entry
// there is looked up, whatever it is, and nothing is validated or read.
func readRepoFile(ctx context.Context, dir, commit string, policy domain.RepoConfig, req *v1.ExplainProjectRequest) error {
	if policy == domain.RepoConfigDeny {
		_, oid, err := treeEntry(ctx, dir, commit, repoconfig.Path)
		req.FilePresent = oid != ""
		return err
	}
	blob, err := fileBlob(ctx, dir, commit, repoconfig.Path)
	if err != nil || blob == "" {
		return err
	}
	req.FilePresent = true
	req.File, err = blobBytes(ctx, dir, blob)
	return err
}

// checkClone refuses a directory whose origin is not a URL of the Project.
func checkClone(ctx context.Context, dir string, p v1.Project) error {
	origin, err := gitctx.OriginURL(ctx, dir)
	if err != nil {
		return domain.NewError(domain.CodeInvalidRequest, "this directory has no origin remote, so it cannot be told to be a clone of "+p.Name, nil)
	}
	matches, err := gitctx.Match(origin, []v1.Project{p})
	if err != nil {
		return domain.NewError(domain.CodeInvalidRequest, err.Error(), nil)
	}
	if len(matches) != 1 {
		return domain.NewError(domain.CodeInvalidRequest,
			fmt.Sprintf("this directory is not a clone of Project %s: its origin is not one of the repo URLs of the Project", p.Name), nil)
	}
	return nil
}

// commitOf is the commit that ref has in the clone at dir (no fetch: what the
// clone has is what is explained).
func commitOf(ctx context.Context, dir, ref string) (string, error) {
	if strings.HasPrefix(ref, "-") {
		return "", domain.NewError(domain.CodeInvalidRequest, "the ref must not start with '-'", nil)
	}
	out, err := localGit(ctx, dir, "rev-parse", "--verify", "--end-of-options", ref+"^{commit}")
	out = strings.TrimSpace(out)
	if err != nil || !domain.ValidCommitID(out) {
		return "", domain.NewError(domain.CodeNotFound, fmt.Sprintf("ref %q is not a commit in this clone", ref), nil)
	}
	return out, nil
}

// treeEntry is the mode and git object ID of whatever the commit has at path
// (repo-root-relative, whatever the directory of the clone), "" if it has no
// entry there. Any object counts, as for the Worker's gitops.BlobIDs.
func treeEntry(ctx context.Context, dir, commit, path string) (mode, oid string, err error) {
	out, err := localGit(ctx, dir, "ls-tree", "--full-tree", commit, "--", path)
	if err != nil {
		return "", "", err
	}
	out = strings.TrimSpace(out)
	if out == "" {
		return "", "", nil
	}
	meta, _, _ := strings.Cut(out, "\t")
	fields := strings.Fields(meta)
	if len(fields) != 3 {
		return "", "", fmt.Errorf("git ls-tree answered something unexpected for %s", path)
	}
	return fields[0], fields[2], nil
}

// blobID is the object ID of the digest input at path, "" if absent.
func blobID(ctx context.Context, dir, commit, path string) (string, error) {
	_, oid, err := treeEntry(ctx, dir, commit, path)
	return oid, err
}

// fileBlob is the blob of the regular file at path, "" if absent; anything
// else is an error, as for the Worker's gitops.Show.
func fileBlob(ctx context.Context, dir, commit, path string) (string, error) {
	mode, oid, err := treeEntry(ctx, dir, commit, path)
	if err != nil || oid == "" {
		return "", err
	}
	if mode != "100644" && mode != "100755" {
		return "", domain.NewError(domain.CodeInvalidRequest, fmt.Sprintf("%s at %s is not a regular file", path, commit), nil)
	}
	return oid, nil
}

func blobBytes(ctx context.Context, dir, blob string) ([]byte, error) {
	out, err := localGit(ctx, dir, "cat-file", "blob", blob)
	return []byte(out), err
}

// localGit runs git in dir and returns its stdout untouched, so that file
// contents survive. Its failure carries git's message.
func localGit(ctx context.Context, dir string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "git", append([]string{"-C", dir}, args...)...) //nolint:gosec // fixed git subcommands; user values follow "--" or --end-of-options
	cmd.Env = append(cmd.Environ(), "GIT_TERMINAL_PROMPT=0", "LC_ALL=C", "GIT_LITERAL_PATHSPECS=1")
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		if msg := strings.TrimSpace(stderr.String()); msg != "" {
			return "", errors.New("git " + args[0] + ": " + redact.Text(msg))
		}
		return "", fmt.Errorf("git %s: %w", args[0], err)
	}
	return stdout.String(), nil
}
