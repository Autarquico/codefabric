package client

import (
	"context"
	"net/http"
	"strconv"

	v1 "github.com/Autarquico/codefabric/internal/api/v1"
)

// SearchQuery is a search (GET /v1/search). Kind, Project and Limit are optional.
type SearchQuery struct {
	Text    string
	Kind    string
	Project string
	Limit   int
}

// Search finds entities by reference and by text: references first, then
// matches ranked by relevance.
func (c *Client) Search(ctx context.Context, sq SearchQuery) (v1.SearchResponse, error) {
	q := scope(sq.Project)
	q.Set("q", sq.Text)
	if sq.Kind != "" {
		q.Set("kind", sq.Kind)
	}
	if sq.Limit != 0 {
		q.Set("limit", strconv.Itoa(sq.Limit))
	}
	var out v1.SearchResponse
	err := c.do(ctx, http.MethodGet, withQuery("/v1/search", q), nil, &out)
	return out, err
}
