package executor

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	v1 "github.com/Autarquico/codefabric/internal/api/v1"
	"github.com/Autarquico/codefabric/internal/domain"
	"github.com/Autarquico/codefabric/internal/repoconfig"
	"github.com/Autarquico/codefabric/internal/worker/gitops"
	"github.com/Autarquico/codefabric/internal/worker/state"
)

// errAckPending is the wait for the Controller to confirm config_resolved. It
// is a prepare put off (errPrepareLater) but polled every ackPoll, outside the
// backoff of the prepare, which it neither grows nor inherits.
var errAckPending = fmt.Errorf("%w: the resolved configuration is waiting for the Controller to confirm it", errPrepareLater)

// ackPoll is how often the stored resolution is looked at while its report is
// in the journal; the sender keeps sending it meanwhile, so a lost ack is
// retried by the sender and noticed here at the next poll.
const ackPoll = exitTick

// codedFailure is a preparation that fails for a reason the Controller keeps as
// the failure code of the Run (v1.PrepareFailureCodes). msg goes in the report:
// it carries nothing of the Run's secrets.
type codedFailure struct {
	code domain.Code
	msg  string
}

func (f *codedFailure) Error() string { return string(f.code) + ": " + f.msg }

// resolveRepoConfig is the first step of the prepare of a Run of schema 4
// (repo-config:v1): it reads .codefabric/project.yaml at the commit of the Run
// if the Project allows it, resolves the effective configuration with what the
// Controller froze, stores it in state.db together with the config_resolved
// report that tells it, and returns nil only once the Controller has confirmed
// that report. Until then it says errPrepareLater, and the next try finds the
// stored resolution: nothing is read or computed twice, and the report that is
// sent again after a restart is the same one.
func (e *Executor) resolveRepoConfig(ctx context.Context, r state.Run, spec specView) error {
	res, ok, err := state.ResolutionOf(ctx, e.o.DB.Reader(), r.Key)
	if err != nil {
		return err
	}
	if !ok {
		if err := e.recordResolution(ctx, r, spec); err != nil {
			return err
		}
		return errAckPending
	}
	pending, err := state.HasJournalSeq(ctx, e.o.DB.Reader(), res.JournalSeq)
	if err != nil {
		return err
	}
	if pending {
		return errAckPending
	}
	var eff repoconfig.Effective
	if err := json.Unmarshal(res.Effective, &eff); err != nil {
		return fmt.Errorf("the stored configuration of the Run is corrupt: %w", err)
	}
	if env := eff.Environment; env != nil && (env.Prepare != "" || len(env.Services) > 0) {
		return &codedFailure{domain.CodeNotImplemented, "this Worker cannot run the prepare or the services of an Environment: it does not offer " + domain.EnvironmentHostCapability}
	}
	return nil
}

// recordResolution computes the effective configuration and stores it with its
// report in one transaction.
func (e *Executor) recordResolution(ctx context.Context, r state.Run, spec specView) error {
	in := repoconfig.Inputs{
		RepoConfig: spec.RepoConfig,
		Resources:  domain.Resources{CPU: spec.Requirements.Resources.CPU, MemoryMB: spec.Requirements.Resources.MemoryMB},
		// Where each came from was frozen with the spec by the Controller.
		CPUOrigin: spec.Origins.Resources, MemoryOrigin: spec.Origins.Resources,
		Environment: spec.EnvironmentDef, EnvironmentOrigin: spec.Origins.Environment,
	}
	// The commit must be here before any git read: the file of the repo (if the
	// Project allows it) and the digest inputs of the Environment both need it.
	if e.o.Git != nil {
		if err := e.ensureSource(ctx, r, spec); err != nil {
			return err
		}
	}
	if err := e.readRepoFile(ctx, r, spec, &in); err != nil {
		return err
	}
	eff, err := repoconfig.Resolve(in)
	if err != nil {
		return invalidConfig(err)
	}
	blobs, err := e.blobIDs(ctx, r, eff.DigestInputPaths())
	if err != nil {
		return err
	}
	digest, err := repoconfig.EffectiveDigest(eff, blobs)
	if err != nil {
		return invalidConfig(err)
	}
	raw, err := json.Marshal(eff)
	if err != nil {
		return invalidConfig(err)
	}
	now := e.clock.Now()
	err = e.o.DB.WithTx(ctx, func(tx *state.Tx) error {
		cur, err := state.RunBy(ctx, tx, r.Key)
		if err != nil {
			return err
		}
		if cur.Phase != state.PhaseAccepted || cur.FencedCode != "" {
			return fmt.Errorf("%w: the Run moved on", errPrepareLater)
		}
		seq, err := e.reportSeq(ctx, tx, r.Key, v1.ReportConfigResolved, now, func(en *v1.RunReportEntry) {
			en.Effective, en.EffectiveDigest = raw, digest
		})
		if errors.Is(err, errOrphaned) {
			return fmt.Errorf("%w: the Run is an orphan", errPrepareLater)
		}
		if err != nil {
			return err
		}
		return state.InsertResolution(ctx, tx, r.Key, state.Resolution{Effective: raw, Digest: digest, JournalSeq: seq}, now)
	})
	if err != nil {
		return err
	}
	e.o.Journal.Notify()
	return nil
}

// readRepoFile puts the file of the repo in in.File: only if the Project allows
// it (with deny the file is not even read, so an invalid one cannot fail the
// Run) and the commit has one.
func (e *Executor) readRepoFile(ctx context.Context, r state.Run, spec specView, in *repoconfig.Inputs) error {
	if e.o.Git == nil || spec.RepoConfig == domain.RepoConfigDeny {
		return nil
	}
	data, found, err := e.o.Git.Show(ctx, r.ProjectID, r.SourceCommit, repoconfig.Path)
	switch {
	case errors.Is(err, gitops.ErrNotRegular):
		return &codedFailure{domain.CodeRepoConfigInvalid, repoconfig.Path + " is not a regular file"}
	case err != nil:
		return fmt.Errorf("reading %s: %w", repoconfig.Path, err)
	case !found:
		return nil
	}
	file, err := repoconfig.Parse(data)
	if err != nil {
		return invalidConfig(err)
	}
	in.File = &file
	return nil
}

func (e *Executor) blobIDs(ctx context.Context, r state.Run, paths []string) (map[string]string, error) {
	if e.o.Git == nil || len(paths) == 0 {
		return nil, nil
	}
	blobs, err := e.o.Git.BlobIDs(ctx, r.ProjectID, r.SourceCommit, paths)
	if err != nil {
		return nil, fmt.Errorf("reading the digest inputs of the configuration: %w", err)
	}
	return blobs, nil
}

func (e *Executor) ensureSource(ctx context.Context, r state.Run, spec specView) error {
	if r.Repo == "" {
		return fmt.Errorf("this assignment was made before the Controller sent the repository of the Run: it cannot be prepared")
	}
	return e.o.Git.EnsureSource(ctx, gitops.SourceSpec{
		Project: r.ProjectID, Run: r.RunID, Repo: r.Repo, SourceRef: spec.Source.Ref, SourceCommit: r.SourceCommit,
	})
}

// invalidConfig turns the error of the repo configuration into the failure of
// the Run when it is the file's fault (a coded error); anything else is a bug
// of this Worker and fails the prepare as it is.
func invalidConfig(err error) error {
	var coded *domain.Error
	if errors.As(err, &coded) && coded.Code == domain.CodeRepoConfigInvalid {
		return &codedFailure{domain.CodeRepoConfigInvalid, coded.Message}
	}
	return err
}
