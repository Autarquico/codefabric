package config

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"time"

	"github.com/Autarquico/codefabric/internal/domain"
)

// Client is the settings of this machine as a client of the Controller. It is
// where a person says what they trust: whatever a Worker announces about itself
// is a claim, and this file wins over it.
type Client struct {
	Attach ClientAttach `json:"attach"`
	Notify ClientNotify `json:"notify"`
}

// ClientNotify is what this machine is told about (spec §26).
type ClientNotify struct {
	// Events are the event types that notify. Absent, DefaultNotifyEvents.
	Events []string `json:"events,omitempty"`
	// MutedProjects are Projects (id) whose events never notify.
	MutedProjects []string `json:"muted_projects,omitempty"`
	// QuietHours, if set, holds back the native notifications in that window.
	QuietHours *QuietHours `json:"quiet_hours,omitempty"`
	// Hook is a command (argv, never a shell) that gets each notified event as
	// JSON on stdin.
	Hook []string `json:"hook,omitempty"`
}

// QuietHours is a daily window of local time, "HH:MM" each; it may cross midnight.
type QuietHours struct {
	Start string `json:"start"`
	End   string `json:"end"`
}

// DefaultNotifyEvents is what notifies when nothing says otherwise: what needs a person.
var DefaultNotifyEvents = []string{"TaskNeedsInput", "RunFailed", "WorkerLost"}

// NotifyEvents are the event types that notify.
func (n ClientNotify) NotifyEvents() []string {
	if n.Events == nil {
		return DefaultNotifyEvents
	}
	return n.Events
}

func (n ClientNotify) validate() error {
	if q := n.QuietHours; q != nil {
		for _, hhmm := range []string{q.Start, q.End} {
			if _, err := time.Parse("15:04", hhmm); err != nil {
				return fmt.Errorf("notify.quiet_hours: %q is not HH:MM", hhmm)
			}
		}
	}
	if n.Hook != nil && (len(n.Hook) == 0 || n.Hook[0] == "") {
		return errors.New("notify.hook: the command is empty")
	}
	return nil
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
	if err := c.Notify.validate(); err != nil {
		return Client{}, fmt.Errorf("config: %s: %w", p.ClientConfigFile(), err)
	}
	return c, nil
}
