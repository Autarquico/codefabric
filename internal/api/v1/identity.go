package v1

import (
	"time"

	"github.com/Autarquico/codefabric/internal/domain"
)

// ErrorEnvelope is the body of every non-2xx response.
type ErrorEnvelope struct {
	Error ErrorBody `json:"error"`
}

// ErrorBody is {code,message,details}.
type ErrorBody struct {
	Code    domain.Code    `json:"code"`
	Message string         `json:"message"`
	Details map[string]any `json:"details,omitempty"`
}

// JoinRequest redeems a token. The token travels in the body, never the URL.
// CSR is a PEM CERTIFICATE REQUEST: the private key never leaves the client.
//
// Worker is present exactly when the token is a worker's: it carries what the
// machine contributes (§9.2), so a worker invitation cannot be redeemed as a
// client and the other way round.
type JoinRequest struct {
	Token  string          `json:"token"`
	CSR    string          `json:"csr"`
	Name   string          `json:"name"`
	Worker *WorkerSettings `json:"worker,omitempty"`
}

// CertificateResponse carries a freshly signed client certificate (PEM).
type CertificateResponse struct {
	IdentityID  string    `json:"identity_id"`
	Certificate string    `json:"certificate"`
	NotAfter    time.Time `json:"not_after"`
}

// RenewRequest asks for a new certificate for the authenticated identity.
type RenewRequest struct {
	CSR string `json:"csr"`
}

// CreateTokenRequest asks for a single-use join token.
type CreateTokenRequest struct {
	Kind       string `json:"kind"` // "client" or "worker"
	TTLSeconds int64  `json:"ttl_seconds"`
}

// CreateTokenResponse returns the invitation; it is the only time the token is visible.
type CreateTokenResponse struct {
	Kind      string    `json:"kind"`
	ExpiresAt time.Time `json:"expires_at"`
	Invite    string    `json:"invite"`
}

// Client is one client identity as listed by the API.
type Client struct {
	ID        string     `json:"id"`
	Name      string     `json:"name"`
	CreatedAt time.Time  `json:"created_at"`
	NotAfter  time.Time  `json:"not_after"`
	RevokedAt *time.Time `json:"revoked_at,omitempty"`
}

// ListClientsResponse is GET /v1/clients.
type ListClientsResponse struct {
	Clients []Client `json:"clients"`
}
