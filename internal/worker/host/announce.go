package host

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/Autarquico/codefabric/internal/adapters/harness"
	"github.com/Autarquico/codefabric/internal/domain"
)

// DetectTimeout bounds the Detect of one adapter, on the clock of the host.
const DetectTimeout = 5 * time.Second

// errDetectTimeout is what detect returns when it gives up on a Detect after
// DetectTimeout.
var errDetectTimeout = errors.New("detection timed out")

// Status is why a harness is, or is not, offered.
type Status string

// The statuses of a harness.
const (
	// Offered: installed, in at least one mode of the Runtime.
	Offered Status = "offered"
	// NoCommonMode: none of its modes is one the Runtime runs.
	NoCommonMode Status = "no-common-mode"
	// NotInstalled: Detect found it is not on the host.
	NotInstalled Status = "not-installed"
	// DetectFailed: Detect could not tell; Err says why.
	DetectFailed Status = "detect-failed"
)

// Harness is what Announce found of one harness.
type Harness struct {
	ID     string
	Status Status
	// Modes are the modes it is offered in, sorted; empty unless Offered.
	Modes []domain.Mode
	// Path is the binary Detect found, when it was installed.
	Path string
	// Err is why Detect failed, for DetectFailed.
	Err error
}

// Announcement is what a Worker announces of its harnesses.
type Announcement struct {
	// Capabilities are the capabilities to add to the heartbeat: sorted and
	// without repeats.
	Capabilities []string
	// Harnesses are every registered one, by ID, for the log.
	Harnesses []Harness
}

// Announce detects every harness of reg on host and says which the Worker
// offers, for a Runtime that runs runtimeModes. A harness is offered when
// some of its modes is one of runtimeModes and Detect finds it installed; the
// Worker then announces harness:<id>, harness:<id>:mode:<m> for each of
// those modes and harness:<id>:config-isolation if it has ConfigIsolation.
// It always announces domain.RepoConfigCapability (it reads the repo
// configuration of a Run), domain.WorkflowConfigCapability (it fetches and
// materialises the Workflow a Run froze), domain.EnvironmentHostCapability (it runs the prepare
// and the services of an Environment) and domain.HarnessModesMarker: whatever harness it offers,
// it offers with its modes.
//
// It runs once, at start: a harness installed or removed later is seen at the
// next start. It waits for each Detect at most DetectTimeout, whether or not
// the adapter honours its ctx.
func Announce(ctx context.Context, host harness.Host, reg *harness.Registry, runtimeModes []domain.Mode) Announcement {
	out := Announcement{Capabilities: []string{domain.HarnessModesMarker, domain.RepoConfigCapability, domain.WorkflowConfigCapability, domain.EnvironmentHostCapability}}
	for _, id := range reg.IDs() {
		a, _ := reg.Get(id)
		h := assess(ctx, host, a, runtimeModes)
		out.Harnesses = append(out.Harnesses, h)
		if h.Status != Offered {
			continue
		}
		out.Capabilities = append(out.Capabilities, domain.HarnessCapability(id))
		for _, m := range h.Modes {
			out.Capabilities = append(out.Capabilities, domain.HarnessModeCapability(id, m))
		}
		if a.Capabilities().ConfigIsolation {
			out.Capabilities = append(out.Capabilities, domain.HarnessConfigIsolationCapability(id))
		}
	}
	slices.Sort(out.Capabilities)
	out.Capabilities = slices.Compact(out.Capabilities)
	return out
}

func assess(ctx context.Context, host harness.Host, a harness.Adapter, runtimeModes []domain.Mode) Harness {
	h := Harness{ID: a.ID(), Modes: []domain.Mode{}}
	modes := []domain.Mode{}
	for _, m := range a.Capabilities().Modes {
		if slices.Contains(runtimeModes, m) {
			modes = append(modes, m)
		}
	}
	slices.Sort(modes)
	modes = slices.Compact(modes)
	if len(modes) == 0 {
		h.Status = NoCommonMode
		return h
	}
	d, err := detect(ctx, host, a)
	switch {
	case err != nil:
		h.Status, h.Err = DetectFailed, err
	case !d.Installed:
		h.Status = NotInstalled
	default:
		h.Status, h.Modes, h.Path = Offered, modes, d.Path
	}
	return h
}

// detect is a.Detect, given up on after DetectTimeout on the clock of host:
// its ctx is cancelled then, and an adapter that does not honour it is left
// to finish on its own, its answer unread.
func detect(ctx context.Context, host harness.Host, a harness.Adapter) (harness.Detection, error) {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	timer := host.Clock().NewTimer(DetectTimeout)
	defer timer.Stop()
	type result struct {
		d   harness.Detection
		err error
	}
	done := make(chan result, 1)
	go func() {
		d, err := a.Detect(ctx, host)
		done <- result{d, err}
	}()
	select {
	case r := <-done:
		return r.d, r.err
	case <-timer.C():
		return harness.Detection{}, fmt.Errorf("%w after %v", errDetectTimeout, DetectTimeout)
	case <-ctx.Done():
		return harness.Detection{}, ctx.Err()
	}
}
