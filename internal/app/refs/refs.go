// Package refs turns the references people type (§3.13) into entities.
package refs

import (
	"context"

	v1 "github.com/Autarquico/codefabric/internal/api/v1"
	"github.com/Autarquico/codefabric/internal/controller/store"
	"github.com/Autarquico/codefabric/internal/domain"
)

// Project resolves a Project reference.
func Project(ctx context.Context, q store.Querier, ref string) (domain.ID, error) {
	c, err := resolve(ctx, q, domain.KindProject, ref, "")
	return c.ID, err
}

// Scope resolves the Project that narrows an ambiguous name; no reference means no scope.
func Scope(ctx context.Context, q store.Querier, ref string) (domain.ID, error) {
	if ref == "" {
		return "", nil
	}
	return Project(ctx, q, ref)
}

// Task resolves a Task reference; a name shared by Projects is narrowed to scope.
func Task(ctx context.Context, q store.Querier, ref string, scope domain.ID) (domain.ID, error) {
	c, err := resolve(ctx, q, domain.KindTask, ref, scope)
	return c.ID, err
}

// Run resolves a Run reference. Runs have no names, so no scope applies.
func Run(ctx context.Context, q store.Querier, ref string) (domain.ID, error) {
	c, err := resolve(ctx, q, domain.KindRun, ref, "")
	return c.ID, err
}

// Resolve is the GET /v1/resolve use case: the entity of kind that ref points
// to, with names narrowed to the Project projectRef.
func Resolve(ctx context.Context, db *store.DB, kind domain.Kind, ref, projectRef string) (v1.Resolved, error) {
	var out v1.Resolved
	err := db.Read(ctx, func(q store.Querier) error {
		scope, err := Scope(ctx, q, projectRef)
		if err != nil {
			return err
		}
		c, err := resolve(ctx, q, kind, ref, scope)
		out = v1.Resolved{Kind: kind, ID: string(c.ID), Short: c.Short.String()}
		return err
	})
	return out, err
}

func resolve(ctx context.Context, q store.Querier, kind domain.Kind, ref string, scope domain.ID) (domain.Candidate, error) {
	var (
		cands []domain.Candidate
		err   error
	)
	switch kind {
	case domain.KindProject:
		cands, err = store.ProjectCandidates(ctx, q)
	case domain.KindTask:
		cands, err = store.TaskCandidates(ctx, q)
	case domain.KindRun:
		cands, err = store.RunCandidates(ctx, q)
	default:
		return domain.Candidate{}, domain.NewError(domain.CodeInvalidRequest,
			"kind must be project, task or run", map[string]any{"kind": string(kind)})
	}
	if err != nil {
		return domain.Candidate{}, err
	}
	id, err := domain.Resolve(ref, kind, scope, cands)
	if err != nil {
		return domain.Candidate{}, err
	}
	for _, c := range cands {
		if c.ID == id {
			return c, nil
		}
	}
	return domain.Candidate{ID: id}, nil // unreachable: Resolve picks from cands
}
