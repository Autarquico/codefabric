package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/Autarquico/codefabric/internal/adapters/secrets"
	v1 "github.com/Autarquico/codefabric/internal/api/v1"
	"github.com/Autarquico/codefabric/internal/domain"
	"github.com/Autarquico/codefabric/internal/worker/state"
)

// Problems of the catalog the heartbeat reports, as the health of the daemon.
const (
	catalogUnreadable = "the catalog of secrets cannot be read; see the log of the daemon"
	catalogTooLarge   = "the catalog of secrets does not fit in a heartbeat; remove some secrets or their Project scopes"
)

// secretRecheck is how long the state of the keyring entries is trusted before
// the next heartbeat asks again, in the background: an item may leave the
// keyring without the catalog changing.
const secretRecheck = time.Minute

// secretKey is one version of a secret: a state holds only for the version it
// was checked for, so setting a ref again invalidates what was known of it.
type secretKey struct {
	ref       string
	updatedAt time.Time
}

// secretHealth is the last Check of the keyring entries of the catalog, kept
// for the heartbeat: Check reads the keyring, may wait on a locked one, and
// writes the catalog while it sweeps, so it never runs on the beat. The zero
// value is ready to use.
type secretHealth struct {
	mu        sync.Mutex
	states    map[secretKey]secrets.State
	checkedAt time.Time
	running   bool
}

// stateOf is how metadata stood at the last Check, or false if no Check has
// seen this version of the secret.
func (h *secretHealth) stateOf(m secrets.Metadata) (secrets.State, bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	st, ok := h.states[secretKey{m.Ref, m.UpdatedAt}]
	return st, ok
}

// due says whether a Check must run: the last one too old, or a keyring entry
// no Check has seen. Staleness alone is enough, with no keyring entry left: a
// Check also sweeps the keyring items the catalog retired, and those of the
// last entry removed while the keyring was locked still need their retry. It
// marks the Check as running when it says so.
func (h *secretHealth) due(metas []secrets.Metadata, now time.Time) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.running {
		return false
	}
	due := now.Sub(h.checkedAt) >= secretRecheck
	for _, m := range metas {
		if _, seen := h.states[secretKey{m.Ref, m.UpdatedAt}]; !due && m.Backend == secrets.BackendKeyring && !seen {
			due = true
		}
	}
	h.running = due
	return due
}

// record stores the statuses of a Check made while metas was the catalog. A
// Check that failed keeps what was known and tries again after secretRecheck.
func (h *secretHealth) record(metas []secrets.Metadata, statuses []secrets.Status, err error, now time.Time) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.running, h.checkedAt = false, now
	if err != nil {
		return
	}
	version := make(map[string]time.Time, len(metas))
	for _, m := range metas {
		version[m.Ref] = m.UpdatedAt
	}
	h.states = make(map[secretKey]secrets.State, len(statuses))
	for _, st := range statuses {
		if at, ok := version[st.Ref]; ok {
			h.states[secretKey{st.Ref, at}] = st.State
		}
	}
}

func (d *daemon) now() time.Time {
	if d.clock == nil {
		return time.Now()
	}
	return d.clock.Now()
}

// checkInBackground starts a Check of the keyring entries if one is due. The
// heartbeat does not wait for it: it announces with what the last one saw, and
// a version of a secret no Check has seen yet is announced, as one that is
// there until a Check says it is not.
func (d *daemon) checkInBackground(ctx context.Context, metas []secrets.Metadata) {
	if !d.health.due(metas, d.now()) {
		return
	}
	go func() {
		statuses, err := d.secrets.Check(ctx)
		if err != nil && ctx.Err() == nil {
			d.log.WarnContext(ctx, "the secrets of the keyring could not be checked", "err", err)
		}
		d.health.record(metas, statuses, err, d.now())
	}()
}

// secretsReport is the metadata of every secret this Worker can hand out,
// under the next seq; never a value. A catalog that cannot be read, or that
// does not fit in a heartbeat, is reported as empty, which is what the Worker
// can then rely on, and the problem is returned for the health. Secrets of a
// backend this daemon cannot read are not reported: it could not hand them out.
// Nor is a keyring entry whose value the keyring no longer has (broken); one
// the keyring could not be asked for (locked) is, and a Run that needs it fails
// at prepare with CF_SECRET_UNAVAILABLE and a diagnostic.
func (d *daemon) secretsReport(ctx context.Context) (v1.SecretsReport, string) {
	d.seq++
	report := v1.SecretsReport{Seq: d.seq, Refs: []v1.SecretMeta{}}
	if d.secrets == nil {
		return report, ""
	}
	metas, err := d.secrets.List(ctx)
	if err != nil {
		d.log.WarnContext(ctx, "the catalog of secrets cannot be read; reporting none", "err", err)
		return report, catalogUnreadable
	}
	d.checkInBackground(ctx, metas)
	for _, m := range metas {
		if m.Backend != secrets.BackendFile && m.Backend != secrets.BackendKeyring {
			d.log.DebugContext(ctx, "secret not reported: its backend is not supported", "ref", m.Ref, "backend", m.Backend)
			continue
		}
		if st, _ := d.health.stateOf(m); m.Backend == secrets.BackendKeyring && st == secrets.StateBroken {
			d.log.WarnContext(ctx, "secret not reported: its value is gone from the keyring; set it again or remove it", "ref", m.Ref)
			continue
		}
		report.Refs = append(report.Refs, v1.SecretMeta{Ref: m.Ref, Backend: m.Backend, Projects: m.Projects, UpdatedAt: m.UpdatedAt})
	}
	if encoded, err := json.Marshal(report); err != nil || len(encoded) > v1.MaxSecretsReportBytes {
		d.log.WarnContext(ctx, "the catalog of secrets does not fit in a heartbeat; reporting none",
			"secrets", len(report.Refs), "bytes", len(encoded), "limit", v1.MaxSecretsReportBytes)
		report.Refs = []v1.SecretMeta{}
		return report, catalogTooLarge
	}
	return report, ""
}

// answerFence handles a heartbeat the Controller's fence refused, and says
// whether to send the next one at once. A retired generation is a state.db that
// was lost or restored: this run takes one above the Controller's and goes on,
// unless a later run on this machine took over, and this one stops. A
// heartbeat of its own that another overtook needs nothing but the next one.
// Any other error is returned as it came.
func (d *daemon) answerFence(ctx context.Context, err error) (bool, error) {
	var coded *domain.Error
	if !errors.As(err, &coded) || coded.Code != domain.CodeStaleLease {
		return false, err
	}
	switch coded.Details["reason"] {
	case v1.FenceOutOfOrder:
		return true, nil
	case v1.FenceGenerationRetired:
	default:
		return false, err
	}
	controller, ok := detailUint(coded.Details["generation"])
	if !ok {
		return false, err
	}
	next, rerr := d.stateDB.RecoverGeneration(ctx, d.generation, controller)
	if errors.Is(rerr, state.ErrSuperseded) {
		return false, fmt.Errorf("a later run of the daemon of this Worker took over (generation %d is retired): %w", d.generation, rerr)
	}
	if rerr != nil {
		return false, rerr
	}
	d.log.WarnContext(ctx, "the Controller had seen this generation from another run, as when state.db is lost or restored; taking a new one",
		"retired", d.generation, "controller", controller, "generation", next)
	d.generation = next
	return true, nil
}

// detailUint reads a number of the details of an error, which JSON decodes
// as a float64.
func detailUint(v any) (uint64, bool) {
	switch n := v.(type) {
	case uint64:
		return n, true
	case float64:
		if n < 0 || n > 1<<53 || n != float64(uint64(n)) {
			return 0, false
		}
		return uint64(n), true
	}
	return 0, false
}
