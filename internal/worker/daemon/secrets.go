package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

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

// secretsReport is the metadata of every secret this Worker can hand out,
// under the next seq; never a value. A catalog that cannot be read, or that
// does not fit in a heartbeat, is reported as empty, which is what the Worker
// can then rely on, and the problem is returned for the health. Secrets of a
// backend this daemon cannot read are not reported: it could not hand them out.
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
	for _, m := range metas {
		if m.Backend != secrets.BackendFile {
			d.log.DebugContext(ctx, "secret not reported: its backend is not supported", "ref", m.Ref, "backend", m.Backend)
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
