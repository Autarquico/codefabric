package executor

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/Autarquico/codefabric/internal/adapters/harness"
	v1 "github.com/Autarquico/codefabric/internal/api/v1"
	"github.com/Autarquico/codefabric/internal/domain"
	"github.com/Autarquico/codefabric/internal/worker/state"
	"github.com/Autarquico/codefabric/internal/worker/workflowfetch"
)

// WorkflowClient is what the Worker reads of the Workflow a Run froze.
// *client.Client opened with the credentials of the Worker implements it.
type WorkflowClient interface {
	RunWorkflow(ctx context.Context, run string, leaseEpoch uint64) (v1.WorkflowVersion, error)
	OpenRunBlob(ctx context.Context, run string, leaseEpoch uint64, digest string) (io.ReadCloser, int64, error)
}

// config is the ConfigSource of a Worker without one of its own: a Run that
// froze a Workflow gets the components of it for its harness, one without gets
// an empty configuration.
func (e *Executor) config(ctx context.Context, r state.Run, harnessID string) (harness.NativeSnapshot, error) {
	frozen := parseSpec(r.Spec).Workflow
	if frozen == "" {
		return e.emptyConfig(ctx, r, harnessID)
	}
	if e.o.Workflows == nil {
		return harness.NativeSnapshot{}, fmt.Errorf("the Run froze the Workflow %s and this Worker cannot fetch it", frozen)
	}
	if _, ok := e.harnesses.Get(harnessID); !ok {
		return harness.NativeSnapshot{}, fmt.Errorf("no harness %q", harnessID)
	}
	return e.workflowSnapshot(ctx, r, frozen, harnessID)
}

// workflowSnapshot downloads the manifest of the version the Run froze and the
// blobs of the components of harnessID, checking each against the digest that
// names it, and builds the snapshot the adapter applies.
func (e *Executor) workflowSnapshot(ctx context.Context, r state.Run, frozen, harnessID string) (harness.NativeSnapshot, error) {
	v, err := e.o.Workflows.RunWorkflow(ctx, r.RunID, r.LeaseEpoch)
	if err != nil {
		return harness.NativeSnapshot{}, fetchFailure("reading the Workflow of the Run", err)
	}
	_, digest, _ := strings.Cut(frozen, "@")
	if v.Digest != digest || v.Manifest == nil {
		return harness.NativeSnapshot{}, fmt.Errorf("the Controller sent a Workflow that is not the one the Run froze (%s)", frozen)
	}
	if err := v.Manifest.Validate(); err != nil {
		return harness.NativeSnapshot{}, err
	}
	if got, err := v.Manifest.Digest(); err != nil || got != digest {
		return harness.NativeSnapshot{}, fmt.Errorf("the manifest the Controller sent does not digest to the Workflow the Run froze (%s)", frozen)
	}
	return e.fetcher(r).Snapshot(ctx, v.Manifest, harnessID)
}

// fetcher downloads the blobs of the Workflow of r under its lease.
func (e *Executor) fetcher(r state.Run) workflowfetch.Fetcher {
	return workflowfetch.Fetcher{
		DataDir: e.o.Paths.WorkerDataDir(), Log: e.log, Failure: fetchFailure,
		Open: func(ctx context.Context, digest string) (io.ReadCloser, int64, error) {
			return e.o.Workflows.OpenRunBlob(ctx, r.RunID, r.LeaseEpoch, digest)
		},
	}
}

// fetchFailure says whether a download that failed is worth another try: one
// the Controller could not answer now, or a lease that moved (the next prepare
// is of the Run's live one), is put off; any other refusal is final.
func fetchFailure(what string, err error) error {
	var coded *domain.Error
	if errors.As(err, &coded) {
		switch coded.Code { //nolint:exhaustive // the rest is final
		case domain.CodeControllerUnreachable, domain.CodeInternal, domain.CodeInProgress, domain.CodeStaleLease:
		default:
			return fmt.Errorf("%s: %w", what, err)
		}
	}
	return fmt.Errorf("%w: %s: %w", errPrepareLater, what, err)
}
