package codex

import (
	"encoding/json"
	"errors"
	"path/filepath"
	"time"

	"github.com/Autarquico/codefabric/internal/adapters/harness"
)

var _ harness.AuthSyncer = Adapter{}

// GlobalAuth is ~/.codex/auth.json.
func (Adapter) GlobalAuth(host harness.Host) string {
	return filepath.Join(globalDir(host), AuthFile)
}

// AuthStamp reads last_refresh of an auth.json: Codex writes it each time it
// refreshes the token, and tokens.account_id is the account it belongs to. A
// content that is not a JSON object, like a file that Codex has truncated and
// not yet written, is an error.
func (Adapter) AuthStamp(content []byte) (time.Time, string, error) {
	var auth struct {
		LastRefresh *string `json:"last_refresh"`
		Tokens      *struct {
			AccountID string `json:"account_id"`
		} `json:"tokens"`
	}
	if err := json.Unmarshal(content, &auth); err != nil {
		return time.Time{}, "", errors.New("codex: auth is not a JSON object")
	}
	identity := ""
	if auth.Tokens != nil {
		identity = auth.Tokens.AccountID
	}
	if auth.LastRefresh == nil {
		return time.Time{}, identity, nil
	}
	t, err := time.Parse(time.RFC3339Nano, *auth.LastRefresh)
	if err != nil {
		return time.Time{}, "", errors.New("codex: auth last_refresh is not a timestamp")
	}
	return t, identity, nil
}
