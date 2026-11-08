package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"strconv"
	"strings"
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
	// RunLostTimeout is how long a Worker may stay silent, with the Controller
	// running, before its Runs are lost (§27.2), e.g. "30m". Unset, it is the
	// default of Appendix A.
	RunLostTimeout Duration `json:"run_lost_timeout,omitempty"`
	// RelayMaxPushBytes bounds one push to the git relay. Unset, it is 2 GiB.
	RelayMaxPushBytes int64 `json:"relay_max_push_bytes,omitempty"`
	// Retention is how long each kind of history is kept (§20.5). Unset fields
	// take the defaults of the spec.
	Retention Retention `json:"retention,omitzero"`
	// GC tunes the garbage collector of the Controller.
	GC GC `json:"gc,omitzero"`
	// MaxStorage is max_storage of §20.5 in bytes. It only measures: the GC
	// warns when the Controller's storage passes it and deletes nothing for it.
	MaxStorage int64 `json:"max_storage,omitempty"`
}

// Retention is logs.retention of §20.5, as JSON (this file is not YAML), e.g. "30d".
type Retention struct {
	// System is how long the logs that belong to no Run are kept.
	System Duration `json:"system,omitempty"`
	// Events is how long the events are kept.
	Events Duration `json:"events,omitempty"`
	// RunLogs is how long the logs of a Run, and the commands already
	// delivered to its Worker, are kept.
	RunLogs Duration `json:"run_logs,omitempty"`
	// RawOutput is how long the output of a finished Run is kept.
	RawOutput Duration `json:"raw_output,omitempty"`
}

// GC tunes how and when the Controller collects garbage.
type GC struct {
	// BatchSize is the most rows that one transaction of the GC touches, so that
	// no write of the Controller waits behind more than that. Unset, it is 500.
	BatchSize int `json:"batch_size,omitempty"`
	// Interval is the time between two passes. Unset, it is 1h.
	Interval Duration `json:"interval,omitempty"`
	// BlobGrace is how long an unreferenced blob is left alone: the time a
	// writer has to reference what it has just stored. Unset, it is 24h.
	BlobGrace Duration `json:"blob_grace,omitempty"`
}

// Duration is a time.Duration written in JSON as text such as "45s".
type Duration time.Duration

// MarshalJSON writes the duration as a string.
func (d Duration) MarshalJSON() ([]byte, error) {
	return json.Marshal(time.Duration(d).String())
}

// day is the unit "d" of a duration, which time.ParseDuration does not know.
const day = 24 * time.Hour

// parseDuration is time.ParseDuration plus whole days: "30d".
func parseDuration(text string) (time.Duration, error) {
	if days, ok := strings.CutSuffix(text, "d"); ok {
		n, err := strconv.ParseUint(days, 10, 16)
		return time.Duration(n) * day, err
	}
	return time.ParseDuration(text)
}

// UnmarshalJSON reads a string such as "45s" or "30d"; anything under one second is refused.
func (d *Duration) UnmarshalJSON(raw []byte) error {
	var text string
	if err := json.Unmarshal(raw, &text); err != nil {
		return fmt.Errorf("a duration such as \"45s\": %w", err)
	}
	parsed, err := parseDuration(text)
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
