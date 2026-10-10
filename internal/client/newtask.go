package client

import (
	"context"
	"fmt"
	"strings"

	v1 "github.com/Autarquico/codefabric/internal/api/v1"
	"github.com/Autarquico/codefabric/internal/client/gitctx"
	"github.com/Autarquico/codefabric/internal/domain"
)

// NewTask creates a Task from the working directory dir, the way every
// interface does (§11.1). Without req.Project it is the Project whose repo is
// the origin of dir; nothing matching is an error, never a default. Unless
// req.NoStart, it asks the origin for the commit of the source ref (the
// Project's default branch if req.SourceRef is empty) and sends it with the
// ref it resolved; the Run freezes the pair, and the Controller never resolves
// refs (ADR 0002 D1).
//
// If the origin cannot be asked and the call carries a request_id, the request
// goes without the pair: the Controller answers it from the saved response if
// the request was already made, and refuses it otherwise. Repeating a call
// must not depend on the remote being reachable.
func (c *Client) NewTask(ctx context.Context, dir string, req v1.CreateTaskRequest) (v1.CreateTaskResponse, error) {
	project, err := c.projectFor(ctx, dir, req.Project)
	if err != nil {
		return v1.CreateTaskResponse{}, err
	}
	req.Project = project.ID
	req.ResolvedRef, req.SourceCommit = "", ""
	if !req.NoStart {
		ref := first(req.SourceRef, project.DefaultBranch)
		commit, err := gitctx.ResolveRef(ctx, dir, remoteFor(ctx, dir, project), ref)
		switch {
		case err == nil:
			req.ResolvedRef, req.SourceCommit = ref, commit
		case req.RequestID == "":
			return v1.CreateTaskResponse{}, err
		}
	}
	return c.CreateTask(ctx, req)
}

func first(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

// ProjectForDir is the Project whose repo is the origin of dir, if exactly one is.
func (c *Client) ProjectForDir(ctx context.Context, dir string) (v1.Project, error) {
	return c.projectFor(ctx, dir, "")
}

func (c *Client) projectFor(ctx context.Context, dir, ref string) (v1.Project, error) {
	if ref != "" {
		return c.Project(ctx, ref)
	}
	projects, err := c.Projects(ctx)
	if err != nil {
		return v1.Project{}, err
	}
	origin, err := gitctx.OriginURL(ctx, dir)
	if err != nil {
		return v1.Project{}, noMatch("the working directory has no origin remote", projects)
	}
	matches, err := gitctx.Match(origin, projects)
	if err != nil {
		return v1.Project{}, domain.NewError(domain.CodeInvalidRequest, err.Error(), nil)
	}
	switch len(matches) {
	case 0:
		return v1.Project{}, noMatch("no Project has the repository of this working directory", projects)
	case 1:
		return matches[0], nil
	}
	names := projectNames(matches)
	return v1.Project{}, domain.NewError(domain.CodeAmbiguousRef,
		fmt.Sprintf("more than one Project has the repository of this working directory (%s): choose one with --project", strings.Join(names, ", ")),
		map[string]any{"candidates": names})
}

// remoteFor is "origin" when the working copy is of the Project's repository,
// so that git uses the user's own configuration for it; else the repo URL.
func remoteFor(ctx context.Context, dir string, p v1.Project) string {
	if origin, err := gitctx.OriginURL(ctx, dir); err == nil {
		if m, err := gitctx.Match(origin, []v1.Project{p}); err == nil && len(m) == 1 {
			return "origin"
		}
	}
	return p.Repo
}

func noMatch(why string, projects []v1.Project) error {
	names := projectNames(projects)
	known := "there are no Projects yet: create one with `cf project create`"
	if len(names) > 0 {
		known = "Projects: " + strings.Join(names, ", ")
	}
	return domain.NewError(domain.CodeNotFound,
		fmt.Sprintf("%s, so no Project can be chosen (%s); give one with --project", why, known),
		map[string]any{"projects": names})
}

func projectNames(projects []v1.Project) []string {
	names := make([]string, len(projects))
	for i, p := range projects {
		names[i] = p.Name
	}
	return names
}
