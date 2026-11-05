// Package search answers GET /v1/search (§37): the entities a reference points
// to, then those whose indexed text matches, best first.
package search

import (
	"context"
	"slices"
	"strconv"
	"strings"
	"unicode"

	v1 "github.com/Autarquico/codefabric/internal/api/v1"
	"github.com/Autarquico/codefabric/internal/app/refs"
	"github.com/Autarquico/codefabric/internal/controller/store"
	"github.com/Autarquico/codefabric/internal/domain"
)

// Plans and artifacts have no entity yet; they join this list with their APIs.
var kinds = []domain.Kind{domain.KindProject, domain.KindTask, domain.KindRun, domain.KindWorker, domain.KindWorkflow}

const maxTerms = 12

// Service searches the index kept by the store's triggers.
type Service struct{ db *store.DB }

// New wires the search use case to the store.
func New(db *store.DB) *Service { return &Service{db: db} }

// Query is a search as the client sent it; Limit 0 takes the default.
type Query struct {
	Text    string
	Kind    string
	Project string
	Limit   int
}

// Search validates q and runs it in one read snapshot.
func (s *Service) Search(ctx context.Context, q Query) (v1.SearchResponse, error) {
	text, searched, limit, err := validate(q)
	if err != nil {
		return v1.SearchResponse{}, err
	}
	out := v1.SearchResponse{Results: []v1.SearchResult{}}
	err = s.db.Read(ctx, func(r store.Querier) error {
		scope, err := refs.Scope(ctx, r, q.Project)
		if err != nil {
			return err
		}
		exact, err := exactMatches(ctx, r, text, searched, scope)
		if err != nil {
			return err
		}
		seen := map[string]bool{}
		for _, res := range exact {
			seen[res.ID] = true
		}
		var texts []v1.SearchResult
		if match := MatchExpression(text); match != "" {
			for _, k := range searched {
				hits, err := store.SearchText(ctx, r, match, store.SearchFilter{Kind: k, Project: scope, Limit: limit})
				if err != nil {
					return err
				}
				for _, h := range hits {
					if !seen[string(h.ID)] {
						texts = append(texts, result(h, v1.SearchMatchText))
					}
				}
			}
		}
		out.Results = append(append(out.Results, exact...), texts...)
		if len(out.Results) > limit {
			out.Results = out.Results[:limit]
		}
		return nil
	})
	return out, err
}

func validate(q Query) (string, []domain.Kind, int, error) {
	text := strings.TrimSpace(q.Text)
	switch {
	case text == "":
		return "", nil, 0, invalid("q is required", "q")
	case len(text) > v1.SearchMaxQueryBytes:
		return "", nil, 0, invalid("q is longer than "+strconv.Itoa(v1.SearchMaxQueryBytes)+" bytes", "q")
	case q.Limit < 0 || q.Limit > v1.SearchMaxLimit:
		return "", nil, 0, invalid("limit must be between 1 and "+strconv.Itoa(v1.SearchMaxLimit), "limit")
	}
	limit := q.Limit
	if limit == 0 {
		limit = v1.SearchDefaultLimit
	}
	if q.Kind == "" {
		return text, kinds, limit, nil
	}
	if !slices.Contains(kinds, domain.Kind(q.Kind)) {
		return "", nil, 0, invalid("kind must be project, task, run, worker or workflow", "kind")
	}
	return text, []domain.Kind{domain.Kind(q.Kind)}, limit, nil
}

func invalid(msg, field string) error {
	return domain.NewError(domain.CodeInvalidRequest, msg, map[string]any{"field": field})
}

// exactMatches resolves text as a reference for each kind. Several matches of
// one kind are all returned, marked ambiguous.
func exactMatches(ctx context.Context, r store.Querier, text string, ks []domain.Kind, scope domain.ID) ([]v1.SearchResult, error) {
	var out []v1.SearchResult
	for _, k := range ks {
		cands, err := refs.Candidates(ctx, r, k)
		if err != nil {
			return nil, err
		}
		matched := domain.MatchRef(text, k, scope, cands)
		ids := make([]domain.ID, len(matched))
		for i, c := range matched {
			ids[i] = c.ID
		}
		hits, err := store.SearchByID(ctx, r, ids)
		if err != nil {
			return nil, err
		}
		hits = slices.DeleteFunc(hits, func(h store.SearchHit) bool { return scope != "" && h.Project != scope })
		slices.SortFunc(hits, func(a, b store.SearchHit) int { return strings.Compare(string(a.ID), string(b.ID)) })
		how := v1.SearchMatchExact
		if len(hits) > 1 {
			how = v1.SearchMatchAmbiguous
		}
		for _, h := range hits {
			out = append(out, result(h, how))
		}
	}
	return out, nil
}

func result(h store.SearchHit, how string) v1.SearchResult {
	return v1.SearchResult{Kind: h.Kind, ID: string(h.ID), Short: h.Short, Name: h.Name, Project: string(h.Project), Match: how}
}

// MatchExpression turns what a person typed into an FTS5 expression of quoted
// prefix terms ("dra"* "wor"*), all of which must match. Only letters and
// digits survive, so quotes, '*', '-', ':', '^', parentheses and the words NEAR,
// AND, OR and NOT are text, never operators. It is "" when nothing is left.
func MatchExpression(text string) string {
	terms := strings.FieldsFunc(text, func(r rune) bool { return !unicode.IsLetter(r) && !unicode.IsDigit(r) })
	if len(terms) > maxTerms {
		terms = terms[:maxTerms]
	}
	for i, t := range terms {
		terms[i] = `"` + t + `"*`
	}
	return strings.Join(terms, " ")
}
