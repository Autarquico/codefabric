package config

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"

	"github.com/Autarquico/codefabric/internal/domain"
)

// Client is the settings of this machine as a client of the Controller. It is
// where a person says what they trust: whatever a Worker announces about itself
// is a claim, and this file wins over it.
type Client struct {
	Attach ClientAttach `json:"attach"`
}

// ClientAttach is what `cf attach` trusts.
type ClientAttach struct {
	// SSHTargets maps the name of a Worker to the ssh target to reach it with
	// ([user@]host[:port]), instead of the one the Worker announced.
	SSHTargets map[string]string `json:"ssh_targets,omitempty"`
}

// LoadClient reads client.json; a machine without one trusts the announcements.
// A target in it that is not a valid ssh target is an error here, not later.
func LoadClient(p Paths) (Client, error) {
	raw, err := os.ReadFile(p.ClientConfigFile())
	if errors.Is(err, fs.ErrNotExist) {
		return Client{}, nil
	}
	if err != nil {
		return Client{}, fmt.Errorf("config: reading client config: %w", err)
	}
	var c Client
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&c); err != nil {
		return Client{}, fmt.Errorf("config: parsing %s: %w", p.ClientConfigFile(), err)
	}
	for worker, target := range c.Attach.SSHTargets {
		if !domain.ValidSSHTarget(target) {
			return Client{}, fmt.Errorf("config: %s: attach.ssh_targets[%q] is not [user@]host[:port]", p.ClientConfigFile(), worker)
		}
	}
	return c, nil
}
