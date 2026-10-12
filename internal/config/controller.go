package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"time"

	"github.com/Autarquico/codefabric/internal/support/atomicfile"
)

// Controller is the Controller's settings file. JSON keeps the module free of
// a YAML dependency; nothing in it is a secret.
type Controller struct {
	// Listen is the address the API binds, e.g. "0.0.0.0:7443".
	Listen string `json:"listen"`
	// Advertise is the host:port clients are told to dial; its host becomes the
	// server certificate's SAN.
	Advertise string `json:"advertise"`
	// WorkerOfflineTTL is how long a Worker may stay silent before it is
	// offline, e.g. "45s". Unset, it is the default of Appendix A.
	WorkerOfflineTTL Duration `json:"worker_offline_ttl,omitempty"`
	// RelayMaxPushBytes bounds one push to the git relay. Unset, it is 2 GiB.
	RelayMaxPushBytes int64 `json:"relay_max_push_bytes,omitempty"`
}

// Duration is a time.Duration written in JSON as text such as "45s".
type Duration time.Duration

// MarshalJSON writes the duration as a string.
func (d Duration) MarshalJSON() ([]byte, error) {
	return json.Marshal(time.Duration(d).String())
}

// UnmarshalJSON reads a string such as "45s"; anything under one second is refused.
func (d *Duration) UnmarshalJSON(raw []byte) error {
	var text string
	if err := json.Unmarshal(raw, &text); err != nil {
		return fmt.Errorf("a duration such as \"45s\": %w", err)
	}
	parsed, err := time.ParseDuration(text)
	if err != nil || parsed < time.Second {
		return fmt.Errorf("%q is not a duration of at least 1s such as \"45s\"", text)
	}
	*d = Duration(parsed)
	return nil
}

// LoadController reads controller.json; ErrNotInitialized if it is missing.
func LoadController(p Paths) (Controller, error) {
	raw, err := os.ReadFile(p.ControllerConfigFile())
	if errors.Is(err, fs.ErrNotExist) {
		return Controller{}, fmt.Errorf("%w: %s does not exist (run cf controller init)", ErrNotInitialized, p.ControllerConfigFile())
	}
	if err != nil {
		return Controller{}, fmt.Errorf("config: reading controller config: %w", err)
	}
	var c Controller
	if err := json.Unmarshal(raw, &c); err != nil {
		return Controller{}, fmt.Errorf("config: parsing %s: %w", p.ControllerConfigFile(), err)
	}
	return c, nil
}

// SaveController writes controller.json atomically.
func SaveController(p Paths, c Controller) error {
	raw, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return fmt.Errorf("config: encoding controller config: %w", err)
	}
	return atomicfile.Write(p.ControllerConfigFile(), append(raw, '\n'), 0o600)
}
