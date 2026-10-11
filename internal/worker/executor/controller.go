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
}
