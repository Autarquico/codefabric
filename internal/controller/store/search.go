package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"

	"github.com/Autarquico/codefabric/internal/domain"
)

// ErrNoFTS5 is returned by Open when the SQLite build has no FTS5 module, so
// that a Controller without search fails at startup and not at the first query.
var ErrNoFTS5 = errors.New("store: this SQLite build has no FTS5 module (search needs it)")

type rowQuerier interface {
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

// checkFTS5 asks the connection which virtual-table modules it was built with.
func checkFTS5(ctx context.Context, db rowQuerier) error {
	var present bool
	err := db.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM pragma_module_list WHERE name = 'fts5')`).Scan(&present)
	if err != nil {
		return fmt.Errorf("store: checking for FTS5: %w", err)
	}
	if !present {
		return ErrNoFTS5
	}
	return nil
}

// SearchHit is one entry of the search index. Name is the indexed name: for a
// Run, "<task name> #<attempt>".
type SearchHit struct {
	Kind     domain.Kind
	ID       domain.ID
	Short    string
	Name     string
	Project  domain.ID
	Archived bool
}

// SearchFilter narrows a text search; zero fields do not filter.
type SearchFilter struct {
	Kinds   []domain.Kind
	Project domain.ID
	Limit   int
}

const searchColumns = `d.kind, d.id, d.short_id, i.name, d.project_id, d.archived`

func scanHit(row rowScanner) (SearchHit, error) {
	var h SearchHit
	var kind, id, project string
	if err := row.Scan(&kind, &id, &h.Short, &h.Name, &project, &h.Archived); err != nil {
		return h, scanErr(err, "search hit")
	}
	h.Kind, h.ID, h.Project = domain.Kind(kind), domain.ID(id), domain.ID(project)
	return h, nil
}

// SearchText returns the live entries that match an FTS5 expression, best
// first by bm25 (a hit in the name weighs ten times one in the body). match is
// built by the caller from sanitised terms; it is bound, never concatenated.
func SearchText(ctx context.Context, q Querier, match string, f SearchFilter) ([]SearchHit, error) {
	var sb strings.Builder
	args := []any{match}
	sb.WriteString(`SELECT ` + searchColumns + ` FROM search_index i JOIN search_docs d ON d.rowid = i.rowid
 WHERE search_index MATCH ? AND d.archived = 0`)
	if len(f.Kinds) > 0 {
		sb.WriteString(` AND d.kind IN (` + strings.TrimSuffix(strings.Repeat("?,", len(f.Kinds)), ",") + `)`)
		for _, k := range f.Kinds {
			args = append(args, string(k))
		}
	}
	if f.Project != "" {
		sb.WriteString(` AND d.project_id = ?`)
		args = append(args, string(f.Project))
	}
	sb.WriteString(` ORDER BY bm25(search_index, 10.0, 1.0), d.rowid LIMIT ?`)
	args = append(args, f.Limit)
	return collect(ctx, q, scanHit, sb.String(), args...)
}

// SearchByID returns the entries with the given IDs, archived ones included.
func SearchByID(ctx context.Context, q Querier, ids []domain.ID) ([]SearchHit, error) {
	if len(ids) == 0 {
		return nil, nil
	}
	args := make([]any, len(ids))
	for i, id := range ids {
		args[i] = string(id)
	}
	marks := strings.TrimSuffix(strings.Repeat("?,", len(ids)), ",")
	return collect(ctx, q, scanHit,
		`SELECT `+searchColumns+` FROM search_docs d JOIN search_index i ON i.rowid = d.rowid WHERE d.id IN (`+marks+`)`, args...)
}

// WorkflowCandidates feeds domain.Resolve. Workflows have no short ID, so only
// their canonical ID, name and ULID prefix match.
func WorkflowCandidates(ctx context.Context, q Querier) ([]domain.Candidate, error) {
	return candidates(ctx, q, domain.KindWorkflow, `SELECT id, 0, name, '', 0 FROM workflows`)
}
