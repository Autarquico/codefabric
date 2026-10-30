package v1

import (
	"time"

	"github.com/Autarquico/codefabric/internal/domain"
)

// Environments (§25.3) are global, versioned definitions of how a worktree is
// prepared for a Run. A version is immutable; applying the definition of the
// head changes nothing.

// EnvironmentAuthor is the principal that made a version.
type EnvironmentAuthor struct {
	Kind string `json:"kind"`
	ID   string `json:"id"`
}

// EnvironmentVersion is one version of an Environment. Definition is only set
// by GET /v1/environments/{ref}.
type EnvironmentVersion struct {
	Environment string                 `json:"environment"`
	Version     int                    `json:"version"`
	Parent      int                    `json:"parent,omitempty"`
	Digest      string                 `json:"digest"`
	Author      EnvironmentAuthor      `json:"author"`
	CreatedAt   time.Time              `json:"created_at"`
	Definition  *domain.EnvironmentDef `json:"definition,omitempty"`
}

// EnvironmentSummary is an Environment with its head; Head is 0 for one
// without versions.
type EnvironmentSummary struct {
	Name       string    `json:"name"`
	Head       int       `json:"head"`
	HeadDigest string    `json:"head_digest,omitempty"`
	CreatedAt  time.Time `json:"created_at"`
}

// ListEnvironmentsResponse is GET /v1/environments.
type ListEnvironmentsResponse struct {
	Environments []EnvironmentSummary `json:"environments"`
}

// ShowEnvironmentResponse is GET /v1/environments/{ref}: the version the
// reference points to, with its definition, and every version, newest first,
// without definitions.
type ShowEnvironmentResponse struct {
	EnvironmentVersion
	Versions []EnvironmentVersion `json:"versions"`
}

// ApplyEnvironmentRequest is PUT /v1/environments/{name}: the definition is
// the next version unless it is that of the head, which answers
// CF_INVALID_REQUEST with details.nothing_to_commit. The Environment is
// created by its first apply.
type ApplyEnvironmentRequest struct {
	RequestID  string                `json:"request_id,omitempty"`
	Definition domain.EnvironmentDef `json:"definition"`
}
