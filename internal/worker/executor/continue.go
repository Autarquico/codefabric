package executor

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"slices"
	"strings"

	"github.com/Autarquico/codefabric/internal/adapters/harness"
	v1 "github.com/Autarquico/codefabric/internal/api/v1"
	"github.com/Autarquico/codefabric/internal/domain"
	"github.com/Autarquico/codefabric/internal/worker/gitops"
	"github.com/Autarquico/codefabric/internal/worker/state"
)

// maxSummaryPrompt bounds what the summary of a checkpoint adds to the prompt
// of a Run that continues without the session of its parent.
const maxSummaryPrompt = 16 << 10

// continuationOf is what the assignment of a Run said it continues, or nil.
func continuationOf(r state.Run) (*v1.Continuation, error) {
	if r.Continues == "" {
		return nil, nil
	}
	var c v1.Continuation
	if err := json.Unmarshal([]byte(r.Continues), &c); err != nil {
		return nil, &codedFailure{domain.CodeInvalidRequest, "the assignment carries a continuation this Worker cannot read"}
	}
	if !validContinuation(c) {
		return nil, &codedFailure{domain.CodeInvalidRequest, "the assignment continues a checkpoint it does not name well"}
	}
	return &c, nil
}

// validContinuation says that the ref is the one of the checkpoint it names:
// anything else would fetch someone else's work. A continuation with no checkpoint
// (a Run seeded with a session of a client, or one that only warns) has neither
// checkpoint nor ref and nothing to fetch from the relay.
func validContinuation(c v1.Continuation) bool {
	if c.CheckpointID == "" && c.Ref == "" {
		return c.NativeState == nil
	}
	kind, _, err := domain.ParseID(c.CheckpointID)
	return err == nil && kind == domain.KindCheckpoint && c.Ref == domain.CheckpointRef(domain.ID(c.CheckpointID))
}

// SeedClient is how a Worker downloads the session a client seeded a Run with, under
// the lease of the Run. *client.Client implements it.
type SeedClient interface {
	OpenNativeSeed(ctx context.Context, run string, leaseEpoch uint64) (io.ReadCloser, int64, error)
}

// relaySource is how the source of a Run that continues a checkpoint is
// fetched: from the relay, with the read token of the lease of the Run. Nothing
// of it goes to the origin, and the credential of the origin is not asked for.
func (e *Executor) relaySource(ctx context.Context, r state.Run, ref string) (*gitops.RelaySource, error) {
	tok, err := e.o.Controller.RelayToken(ctx, r.RunID, r.LeaseEpoch)
	if err != nil {
		return nil, fmt.Errorf("%w: the token of the relay: %w", errPrepareLater, err)
	}
	return &gitops.RelaySource{Ref: ref, Auth: gitops.PushAuth{
		URL: e.o.Controller.RelayURL(tok.Path), Token: tok.Token,
		Cert: e.o.RelayTLS.Cert, Key: e.o.RelayTLS.Key, CA: e.o.RelayTLS.CA,
	}}, nil
}

// sourceAccess is what the source of a Run is fetched with: the credential of
// the origin, or for a Run that continues a checkpoint, or starts from the
// snapshot of a client, the token of the relay.
func (e *Executor) sourceAccess(ctx context.Context, r state.Run, og *originAccess) (*gitops.OriginAuth, *gitops.RelaySource, error) {
	c, err := continuationOf(r)
	if err != nil {
		return nil, nil, err
	}
	if c != nil && c.Ref != "" {
		relay, err := e.relaySource(ctx, r, c.Ref)
		return nil, relay, err
	}
	if digest := snapshotOf(r); digest != "" {
		relay, err := e.relaySource(ctx, r, domain.SnapshotRef(digest))
		return nil, relay, err
	}
	auth, err := og.credential(ctx)
	return auth, nil, err
}

// launchOf builds the Launch of a Run: Start for any Run, and for one that
// continues a checkpoint Resume from its native state when it travels, or Start
// with a prompt that carries the summary of the checkpoint when it does not (or
// cannot be restored here).
func (e *Executor) launchOf(ctx context.Context, adapter harness.Adapter, r state.Run, run harness.RunSpec) (harness.Launch, error) {
	c, err := continuationOf(r)
	if err != nil || c == nil {
		if err != nil {
			return harness.Launch{}, err
		}
		return adapter.Start(ctx, e.o.Host, run)
	}
	warnings := slices.Clone(c.Warnings)
	if c.Seed != nil {
		cp, why, err := e.nativeCheckpoint(ctx, adapter, c.Seed, func() (io.ReadCloser, int64, error) {
			sc, ok := e.o.Controller.(SeedClient)
			if !ok {
				return nil, 0, &domain.Error{Code: domain.CodeNotFound, Message: "this Worker cannot download a session seed"}
			}
			return sc.OpenNativeSeed(ctx, r.RunID, r.LeaseEpoch)
		})
		if err != nil {
			return harness.Launch{}, err
		}
		if cp != nil {
			native := run
			native.Prompt = seededPrompt(c, run.Prompt)
			l, err := adapter.Resume(ctx, e.o.Host, native, *cp)
			if err == nil {
				e.log.InfoContext(ctx, "a Run continues the session its client seeded it with", "run", r.RunID)
				return l, nil
			}
			if ctx.Err() != nil {
				return harness.Launch{}, err
			}
			why = "the session could not be resumed: " + reportable(err.Error())
		}
		warnings = append(warnings, why+": the Run starts without the session it was seeded with")
	}
	if c.NativeState != nil {
		cp, why, err := e.nativeCheckpoint(ctx, adapter, c.NativeState, func() (io.ReadCloser, int64, error) {
			cc, ok := e.o.Controller.(CheckpointClient)
			if !ok {
				return nil, 0, &domain.Error{Code: domain.CodeNotFound, Message: "this Worker cannot download native state"}
			}
			return cc.OpenNativeState(ctx, r.RunID, c.CheckpointID, r.LeaseEpoch)
		})
		if err != nil {
			return harness.Launch{}, err
		}
		if cp != nil {
			native := run
			native.Prompt = harness.ContinuePrompt
			l, err := adapter.Resume(ctx, e.o.Host, native, *cp)
			if err == nil {
				e.log.InfoContext(ctx, "a Run continues the session of its checkpoint", "run", r.RunID, "checkpoint", c.CheckpointID)
				return l, nil
			}
			if ctx.Err() != nil {
				return harness.Launch{}, err
			}
			why = "the session could not be resumed: " + reportable(err.Error())
		}
		warnings = append(warnings, why+": the Run continues from the source and the summary")
	}
	for _, w := range warnings {
		e.log.WarnContext(ctx, "a Run continues without the native state of its checkpoint", "run", r.RunID, "checkpoint", c.CheckpointID, "why", w)
	}
	if c.CheckpointID != "" {
		run.Prompt = continuationPrompt(run.Prompt, c.Summary)
	}
	return adapter.Start(ctx, e.o.Host, run)
}

// seededPrompt is what a Run resumed on the session of a client is told: the prompt of
// its Task when the Task began with that session (`cf do --session <id> "..."`), and
// the generic one for a resume or a move, whose prompt is the one the parent already ran.
func seededPrompt(c *v1.Continuation, prompt string) string {
	if c.ParentRunID == "" && strings.TrimSpace(prompt) != "" {
		return prompt
	}
	return harness.ContinuePrompt
}

// nativeCheckpoint downloads the native state of the checkpoint under the lease
// of the Run and makes the Checkpoint the adapter resumes from. A state that
// cannot be used here is nil with the reason (the Run goes on without it); an
// error is a download to try again, with the prepare.
func (e *Executor) nativeCheckpoint(_ context.Context, adapter harness.Adapter, native *v1.NativeStateRef, open func() (io.ReadCloser, int64, error)) (*harness.Checkpoint, string, error) {
	f, ok := adapter.(harness.CheckpointFormatter)
	switch {
	case native.Adapter != adapter.ID():
		return nil, fmt.Sprintf("the native state is of the %s adapter and the Run uses %s", native.Adapter, adapter.ID()), nil
	case !adapter.Capabilities().Resume || !ok:
		return nil, "the harness of this Worker cannot resume a session", nil
	}
	body, _, err := open()
	if err != nil {
		var coded *domain.Error
		if errors.As(err, &coded) && (coded.Code == domain.CodeNotFound || coded.Code == domain.CodePolicyDenied) {
			return nil, "the Controller has no native state for the checkpoint", nil
		}
		return nil, "", fmt.Errorf("%w: downloading the native state: %w", errPrepareLater, err)
	}
	defer func() { _ = body.Close() }()
	var buf bytes.Buffer
	sum := sha256.New()
	n, err := io.Copy(io.MultiWriter(&buf, sum), io.LimitReader(body, harness.DefaultMaxSessionBytes+1))
	switch {
	case err != nil:
		return nil, "", fmt.Errorf("%w: downloading the native state: %w", errPrepareLater, err)
	case n > harness.DefaultMaxSessionBytes:
		return nil, "the native state is too large", nil
	case "sha256:"+hex.EncodeToString(sum.Sum(nil)) != native.Digest:
		return nil, "the native state does not hash to the digest it was declared with", nil
	}
	return &harness.Checkpoint{Harness: adapter.ID(), Format: f.CheckpointFormat(), Data: buf.Bytes()}, "", nil
}

// continuationPrompt is the prompt of a Run that continues a checkpoint whose
// session it does not have: its own prompt and what the earlier Run left, as the
// Controller summarised it.
func continuationPrompt(prompt string, s v1.CheckpointSummary) string {
	var b strings.Builder
	b.WriteString(prompt)
	b.WriteString("\n\nThis work continues an earlier session that was suspended. The files are as it left them. Its summary:\n")
	for _, part := range [][2]string{{"Progress", s.Progress}, {"Next", s.Next}, {"Decisions", s.Decisions}, {"Commands", s.Commands}} {
		if strings.TrimSpace(part[1]) != "" {
			fmt.Fprintf(&b, "\n%s:\n%s\n", part[0], strings.TrimSpace(part[1]))
		}
	}
	out := b.String()
	if len(out) > len(prompt)+maxSummaryPrompt {
		cut := len(prompt) + maxSummaryPrompt
		for cut > 0 && out[cut]&0xC0 == 0x80 {
			cut--
		}
		out = out[:cut] + "\n[summary cut]"
	}
	return out
}
