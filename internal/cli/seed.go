package cli

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"time"

	"github.com/spf13/cobra"

	v1 "github.com/Autarquico/codefabric/internal/api/v1"
	"github.com/Autarquico/codefabric/internal/client"
	csession "github.com/Autarquico/codefabric/internal/client/session"
	"github.com/Autarquico/codefabric/internal/config"
	"github.com/Autarquico/codefabric/internal/domain"
)

// SessionsFactory builds the sessions of this machine over the directory where cf
// remembers what it handed over. It is composed in by cmd/cf, as the adopter of the
// workflows is, because the harnesses that know where their sessions live are
// adapters; without one, --session answers CF_NOT_IMPLEMENTED.
type SessionsFactory func(stateDir string) *csession.Local

// seedFlags are the flags with which `cf do` and `cf task resume` carry a session of
// this machine to the Run they start (lineage.native_seed).
type seedFlags struct {
	session        string
	allowSensitive bool
	force          bool
	// caller says that the session is the one that runs the command (set by `cf
	// handoff`, never by a flag): it is open by definition and its export ends at the
	// last tool call.
	caller bool
}

func (f *seedFlags) add(cmd *cobra.Command, forceUsage string) {
	fl := cmd.Flags()
	fl.StringVar(&f.session, "session", "", "id of a session of Claude Code or Codex of this machine that the Run continues (it is uploaded to the Controller; the Project must keep native state)")
	fl.BoolVar(&f.allowSensitive, "allow-sensitive-session", false, "send the session even if it holds what looks like a secret")
	fl.BoolVar(&f.force, "force", false, forceUsage)
}

// check refuses a flag that has no session to apply to; nothing has been asked of the
// Controller yet.
func (f seedFlags) check() error {
	if f.session == "" && f.allowSensitive {
		return domain.NewError(domain.CodeInvalidRequest, "--allow-sensitive-session is for --session", nil)
	}
	return nil
}

// seed is a session of this machine uploaded for a Run that is about to be created.
type seed struct {
	id     string
	ref    v1.NativeStateRef
	local  *csession.Local
	digest string
}

// upload exports the session, refuses what must not leave this machine and uploads it
// for the Project. Nothing is uploaded when there is no --session.
func (f seedFlags) upload(ctx context.Context, sessions SessionsFactory, c *client.Client, project string) (*seed, error) {
	if f.session == "" {
		return nil, nil
	}
	if sessions == nil {
		return nil, domain.NewError(domain.CodeNotImplemented, "this binary cannot read the sessions of the harnesses", nil)
	}
	paths, err := config.ResolveFromEnv()
	if err != nil {
		return nil, err
	}
	local := sessions(filepath.Join(paths.ConfigDir, "sessions"))
	exp, err := local.Export(ctx, csession.ExportRequest{
		SessionID: f.session, Caller: f.caller, AllowSensitive: f.allowSensitive, Force: f.force,
	})
	if err != nil {
		return nil, asUserError(err)
	}
	if _, err := c.PutNativeSeed(ctx, project, exp.Digest, bytes.NewReader(exp.Archive), int64(len(exp.Archive))); err != nil {
		return nil, err
	}
	return &seed{
		id: exp.Ref.ID, local: local, digest: exp.Digest,
		ref: v1.NativeStateRef{Adapter: exp.Ref.Adapter, AdapterVersion: exp.Version, Digest: exp.Digest},
	}, nil
}

// handedOff leaves the marker of the session once the Run exists: from now on the work
// of the session goes on in the Task.
func (s *seed) handedOff(task string) error {
	if s == nil {
		return nil
	}
	return s.local.State.MarkHandedOff(s.id, task, s.digest, time.Now())
}

// asUserError gives the refusals of the sessions a code the CLI can show and a script
// can test.
func asUserError(err error) error {
	var sensitive *csession.SensitiveError
	var live *csession.LiveError
	var handed *csession.HandedOffError
	switch {
	case errors.As(err, &sensitive):
		return domain.NewError(domain.CodePolicyDenied, err.Error(), map[string]any{"findings": len(sensitive.Findings)})
	case errors.As(err, &live):
		return domain.NewError(domain.CodeInvalidTransition, err.Error(), nil)
	case errors.As(err, &handed):
		return domain.NewError(domain.CodeConfirmationRequired, err.Error()+" (--force)", nil)
	case errors.Is(err, csession.ErrNotFound):
		return domain.NewError(domain.CodeNotFound, err.Error(), nil)
	}
	return fmt.Errorf("session: %w", err)
}

// projectOfDir is the Project a Task created from dir goes to: the one given, else the
// one whose repository is the origin of dir.
func projectOfDir(ctx context.Context, c *client.Client, dir, project string) (string, error) {
	var (
		p   v1.Project
		err error
	)
	if project != "" {
		p, err = c.Project(ctx, project)
	} else {
		p, err = c.ProjectForDir(ctx, dir)
	}
	return p.ID, err
}
