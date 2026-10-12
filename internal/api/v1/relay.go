package v1

import (
	"strconv"
	"time"
)

// RelayTokenRequest is POST /v1/worker/runs/{run}/relay-token: the token of
// the lease the Worker holds the Run under.
type RelayTokenRequest struct {
	LeaseEpoch uint64 `json:"lease_epoch"`
}

// OrphanTokenRequest is POST /v1/worker/runs/{run}/orphan-token: a token that
// may only create refs/cf/orphans/<run>-e<epoch> at the sealed commit of an
// orphan the Controller detected under that epoch (§10.4 point 5).
type OrphanTokenRequest struct {
	LeaseEpoch   uint64 `json:"lease_epoch"`
	ResultCommit string `json:"result_commit"`
}

// RelayTokenResponse carries a token for the git relay (§16.8). The token goes
// to git in an HTTP header and nowhere else: not in argv, not in a repository's
// configuration, not in a file. Path is where the relay serves the Project
// of the Run, relative to the Controller's address. Write says whether the
// token also lets the Worker push, which it does only while the Run waits for
// its result to be published.
type RelayTokenResponse struct {
	Token     string    `json:"token"`
	ExpiresAt time.Time `json:"expires_at"`
	Path      string    `json:"path"`
	Write     bool      `json:"write"`
	// Ref is the one ref an orphan token may create; empty for the token of a lease.
	Ref string `json:"ref,omitempty"`
}

// OrphanRef is where the result of an orphan is kept in the relay: a ref that
// is only ever created, never moved or deleted.
func OrphanRef(run string, epoch uint64) string {
	return "refs/cf/orphans/" + run + "-e" + strconv.FormatUint(epoch, 10)
}

// RelayPathPrefix is where the Controller serves the git relay.
const RelayPathPrefix = "/v1/relay/"
