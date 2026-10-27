package api_test

import (
	"errors"
	"fmt"
	"net/http"
	"testing"

	v1 "github.com/Autarquico/codefabric/internal/api/v1"
	"github.com/Autarquico/codefabric/internal/domain"
)

// The grant is for Workers only, goes through the SDK, and a denial reaches
// the Worker as its code with details.denied naming each ref and why.
func TestSecretGrantApi_ADenialReachesTheWorkerWithItsRefs(t *testing.T) {
	f := newSchedulingFixture(t)
	ctx := t.Context()
	_, err := f.alpha.AcceptLease(ctx, f.assign.LeaseID, f.assign.Epoch)
	must(t, err)
	daemon := newEpoch()
	_, err = f.alpha.Heartbeat(ctx, heartbeat(daemon))
	must(t, err)
	req := v1.GrantSecretsRequest{LeaseID: f.assign.LeaseID, LeaseEpoch: f.assign.Epoch, WorkerEpoch: daemon, Refs: []string{"gh/token"}}

	// The Run of the fixture declares no secret.
	out, err := f.alpha.GrantSecrets(ctx, f.run.ID, req)
	var coded *domain.Error
	if !errors.As(err, &coded) || coded.Code != domain.CodePolicyDenied || len(out.Granted) != 0 {
		t.Fatalf("grant = %+v, %v", out, err)
	}
	if got := fmt.Sprint(coded.Details["denied"]); got != "[map[reason:not_declared ref:gh/token]]" {
		t.Errorf("details.denied = %s", got)
	}
	if n := f.countRows(`SELECT COUNT(*) FROM audit_entries WHERE action = 'secret.grant'`); n != 1 {
		t.Errorf("%d audit entries, want 1", n)
	}

	// Another Worker's Run is as unknown as one that does not exist.
	_, err = f.beta.GrantSecrets(ctx, f.run.ID, v1.GrantSecretsRequest{
		LeaseID: f.assign.LeaseID, LeaseEpoch: 9, WorkerEpoch: daemon, Refs: []string{"gh/token"},
	})
	if !errors.As(err, &coded) || coded.Code != domain.CodeNotFound {
		t.Errorf("an unknown lease epoch: %v", err)
	}

	owner := f.httpClientFor(f.ownerDir)
	body := fmt.Sprintf(`{"lease_id":%q,"lease_epoch":%d,"worker_epoch":%q,"refs":["gh/token"]}`, f.assign.LeaseID, f.assign.Epoch, daemon)
	if s, e, _ := f.call(owner, "POST", "/v1/worker/runs/"+f.run.ID+"/secrets/grant", body); s != http.StatusForbidden || e.Code != domain.CodePolicyDenied {
		t.Errorf("a client: %d %s", s, e.Code)
	}
	alpha := f.httpClientFor(f.alphaD)
	if s, e, _ := f.call(alpha, "POST", "/v1/worker/runs/"+f.run.ID+"/secrets/grant", `{"refs":["gh/token"],"value":"x"}`); s != http.StatusBadRequest || e.Code != domain.CodeInvalidRequest {
		t.Errorf("an unknown field: %d %s", s, e.Code)
	}
}
