package executor

import (
	"context"
	"time"

	v1 "github.com/Autarquico/codefabric/internal/api/v1"
)

// Controller is the Controller as the executor sees it. *client.Client opened
// with the credentials of the Worker implements it.
type Controller interface {
	Assignments(ctx context.Context, afterSeq uint64, wait time.Duration, limit int) (v1.AssignmentsResponse, error)
	AcceptLease(ctx context.Context, lease string, epoch uint64) (v1.Lease, error)
	RejectLease(ctx context.Context, lease string, req v1.RejectLeaseRequest) (v1.Lease, error)
	AckCommands(ctx context.Context, upToSeq uint64) (v1.AckCommandsResponse, error)
	// RelayToken asks for the token of the git relay under the lease epoch of a Run;
	// RelayURL turns the path it answers into the URL git dials.
	RelayToken(ctx context.Context, run string, leaseEpoch uint64) (v1.RelayTokenResponse, error)
	RelayURL(path string) string
	// OrphanToken asks for the token that may only create the ref of an orphan
	// this Worker holds, at the commit it sealed.
	OrphanToken(ctx context.Context, run string, leaseEpoch uint64, commit string) (v1.RelayTokenResponse, error)
	// Reconcile sends one request of a reconciliation (§27.3).
	Reconcile(ctx context.Context, req v1.ReconcileRequest) (v1.ReconcileResponse, error)
}
