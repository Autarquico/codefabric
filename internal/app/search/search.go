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
		exact, err := exactMatches(ctx, r, text, searched, scope, limit)
		if err != nil {
			return err
		}
		seen := map[string]bool{}
		for _, res := range exact {
			seen[res.ID] = true
		}
		var texts []store.SearchHit
		if match := MatchExpression(text); match != "" {
			texts, err = store.SearchText(ctx, r, match, store.SearchFilter{Kinds: searched, Project: scope, Limit: limit + len(exact)})
			if err != nil {
				return err
			}
		}
		out.Results = append(out.Results, exact...)
		for _, h := range texts {
			if !seen[string(h.ID)] {
				out.Results = append(out.Results, result(h, v1.SearchMatchText))
			}
		}
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

// exactMatch is a hit on a reference and how well the reference matched.
type exactMatch struct {
	level domain.RefLevel
	res   v1.SearchResult
}

// exactMatches resolves text as a reference for each kind: the first step of
// the §3.13 order that finds anything. Several matches of one kind are all
// returned, marked ambiguous. The result is ordered by match level across
// kinds, so that a Project named "task-1" never ranks above Task "task-1".
// Each lookup is a filtered query of at most max(limit, 2) rows.
func exactMatches(ctx context.Context, r store.Querier, text string, ks []domain.Kind, scope domain.ID, limit int) ([]v1.SearchResult, error) {
	var found []exactMatch
	for _, k := range ks {
		for _, step := range domain.RefSteps(text, k) {
			hits, err := store.SearchRef(ctx, r, k, step, scope, max(limit, 2))
			if err != nil {
				return nil, err
			}
			if len(hits) == 0 {
				continue
			}
			how := v1.SearchMatchExact
			if len(hits) > 1 {
				how = v1.SearchMatchAmbiguous
			}
			for _, h := range hits {
				found = append(found, exactMatch{step.Level, result(h, how)})
			}
			break
		}
	}
	slices.SortStableFunc(found, func(a, b exactMatch) int { return int(a.level) - int(b.level) })
	out := make([]v1.SearchResult, len(found))
	for i, f := range found {
		out[i] = f.res
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
