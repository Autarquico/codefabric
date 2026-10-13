package api_test

import (
	"net/http"
	"strings"
	"testing"

	"github.com/oklog/ulid/v2"

	v1 "github.com/Autarquico/codefabric/internal/api/v1"
	"github.com/Autarquico/codefabric/internal/domain"
)

func TestReconcileOverHTTPIsForWorkersAndTheirOwnLeases(t *testing.T) {
	f := newSchedulingFixture(t)
	ctx := t.Context()
	ownerHTTP, anonymous := f.httpClientFor(f.ownerDir), f.httpClient()
	if code, body, _ := f.call(ownerHTTP, http.MethodPost, "/v1/worker/reconcile", "{}"); code != http.StatusForbidden || body.Code != domain.CodePolicyDenied {
		t.Errorf("client: %d %s", code, body.Code)
	}
	if code, body, _ := f.call(anonymous, http.MethodPost, "/v1/worker/reconcile", "{}"); code != http.StatusUnauthorized || body.Code != domain.CodeUnauthenticated {
		t.Errorf("anonymous: %d %s", code, body.Code)
	}
	_, err := f.alpha.AcceptLease(ctx, f.assign.LeaseID, f.assign.Epoch)
	must(t, err)

	// beta names alpha's Run and lease: unknown, and its leases are not listed.
	req := v1.ReconcileRequest{
		Epoch: newEpoch(), Fragment: v1.ReconcileFragment{RoundID: ulid.Make().String(), Count: 1},
		Runs: []v1.ReconcileRun{{RunID: f.assign.RunID, LeaseID: f.assign.LeaseID, LeaseEpoch: f.assign.Epoch, Phase: "running", Alive: true}},
	}
	out, err := f.beta.Reconcile(ctx, req)
	must(t, err)
	if len(out.Verdicts) != 1 || out.Verdicts[0].Verdict != v1.ReconcileUnknown || len(out.Leases) != 0 || !out.Ack.Complete {
		t.Errorf("beta = %+v", out)
	}
	// alpha has its lease listed when it does not name it.
	req.Runs = nil
	out, err = f.alpha.Reconcile(ctx, req)
	must(t, err)
	if len(out.Leases) != 1 || out.Leases[0].Assign.LeaseID != f.assign.LeaseID || out.Ack.Complete {
		t.Errorf("alpha = %+v", out)
	}

	// A body past 1 MiB is refused; one under it with too many entries too.
	big := `{"epoch":"` + newEpoch() + `","sessions":["` + strings.Repeat("x", 2<<20) + `"]}`
	if code, body, _ := f.call(f.httpClientFor(f.alphaD), http.MethodPost, "/v1/worker/reconcile", big); code != http.StatusBadRequest || body.Code != domain.CodeInvalidRequest {
		t.Errorf("a large body: %d %s", code, body.Code)
	}
}
