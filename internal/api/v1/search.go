package v1

import "github.com/Autarquico/codefabric/internal/domain"

// Search limits (§37): the longest query and the most results one call returns.
const (
	SearchMaxQueryBytes = 256
	SearchDefaultLimit  = 20
	SearchMaxLimit      = 100
)

// How a SearchResult matched: Exact is a reference (§3.13) that points to this
// entity alone, Ambiguous one of several it points to, Text a match on the
// indexed text, best first.
const (
	SearchMatchExact     = "exact"
	SearchMatchAmbiguous = "ambiguous"
	SearchMatchText      = "text"
)

// SearchResult is one entity GET /v1/search found.
type SearchResult struct {
	Kind    domain.Kind `json:"kind"`
	ID      string      `json:"id"`
	Short   string      `json:"short_id,omitempty"`
	Name    string      `json:"name"`
	Project string      `json:"project,omitempty"`
	Match   string      `json:"match"`
}

// SearchResponse is GET /v1/search: references first, then text matches.
type SearchResponse struct {
	Results []SearchResult `json:"results"`
}
