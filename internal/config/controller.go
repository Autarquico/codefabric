package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
)

// Controller is the Controller's settings file. JSON keeps the module free of
// a YAML dependency; nothing in it is a secret.
type Controller struct {
	// Listen is the address the API binds, e.g. "0.0.0.0:7443".
	Listen string `json:"listen"`
	// Advertise is the host:port clients are told to dial; its host becomes the
	// server certificate's SAN.
	Advertise string `json:"advertise"`
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
	return WriteFileAtomic(p.ControllerConfigFile(), append(raw, '\n'), 0o600)
}
