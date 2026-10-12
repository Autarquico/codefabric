package v1

import "time"

// RelayTokenRequest is POST /v1/worker/runs/{run}/relay-token: the token of
// the lease the Worker holds the Run under.
type RelayTokenRequest struct {
	LeaseEpoch uint64 `json:"lease_epoch"`
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
}

// RelayPathPrefix is where the Controller serves the git relay.
const RelayPathPrefix = "/v1/relay/"
