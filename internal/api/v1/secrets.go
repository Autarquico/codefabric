package v1

import "time"

// Secrets on the wire are refs and metadata: no request or response carries a
// value, a hash of one or its length (§16.7).

// Backends of a Worker's secrets.
const (
	SecretBackendFile = "file"
)

// MaxSecretsReportBytes bounds a SecretsReport, serialized, inside the 64 KiB
// of a heartbeat: domain.MaxWorkerSecrets refs with no Project scope fit, and a
// Worker that scopes many of them may not hold as many.
const MaxSecretsReportBytes = 40 << 10

// SecretEnv is an environment variable of the harness that carries the secret
// Ref (domain.SecretEnv).
type SecretEnv struct {
	Name string `json:"name"`
	Ref  string `json:"ref"`
}

// SecretMeta is what a Worker says of one secret it holds. Projects scope it:
// only Runs of those Projects may use it; empty means any Project.
type SecretMeta struct {
	Ref       string    `json:"ref"`
	Backend   string    `json:"backend"`
	Projects  []string  `json:"projects,omitempty"`
	UpdatedAt time.Time `json:"updated_at"`
}

// SecretsReport is the whole set of secrets a daemon holds, sent with every
// heartbeat: an empty Refs means none. Seq grows with every report of one
// daemon epoch, so that the Controller keeps the latest of those that reach it
// out of order.
type SecretsReport struct {
	Seq  uint64       `json:"seq"`
	Refs []SecretMeta `json:"refs"`
}

// GrantSecretsRequest is POST /v1/worker/runs/{run}/secrets/grant: the Worker
// asks, before it reads a single value, to resolve Refs for the Run it holds
// under (LeaseID, LeaseEpoch), as the daemon of WorkerEpoch. Every call is
// decided anew and audited, one entry per ref.
type GrantSecretsRequest struct {
	LeaseID     string   `json:"lease_id"`
	LeaseEpoch  uint64   `json:"lease_epoch"`
	WorkerEpoch string   `json:"worker_epoch"`
	Refs        []string `json:"refs"`
}

// GrantSecretsResponse grants every ref asked for. A grant is all or nothing:
// when any ref is denied the answer is CF_POLICY_DENIED, with details.denied a
// []SecretDenial, and none is granted.
type GrantSecretsResponse struct {
	Granted []string `json:"granted"`
}

// Reasons a ref is denied, in details.denied of a grant and in its audit.
const (
	SecretNotDeclared    = "not_declared"    // the frozen spec of the Run does not use it
	SecretNotAllowlisted = "not_allowlisted" // the allowlist of the Project does not hold it
	SecretOutOfScope     = "out_of_scope"    // the Worker holds it only for other Projects
)

// SecretDenial is one ref a grant denies, and why.
type SecretDenial struct {
	Ref    string `json:"ref"`
	Reason string `json:"reason"`
}

// ProjectSecretsRequest is POST /v1/projects/{ref}/secrets/allow and
// POST /v1/projects/{ref}/secrets/deny: Refs join or leave the allowlist of the
// Project. A ref already in (allow) or out (deny) changes nothing.
type ProjectSecretsRequest struct {
	RequestID string   `json:"request_id,omitempty"`
	Refs      []string `json:"refs"`
}

// ProjectSecrets is GET /v1/projects/{ref}/secrets, and the answer to an allow
// or a deny: the allowlist of the Project, sorted, and, for each ref in it,
// the names of the Workers that hold it for the Project.
type ProjectSecrets struct {
	ProjectID string              `json:"project_id"`
	Allowed   []string            `json:"allowed"`
	Available map[string][]string `json:"available"`
}
