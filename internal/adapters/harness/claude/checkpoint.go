package claude

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/Autarquico/codefabric/internal/adapters/harness"
	"github.com/Autarquico/codefabric/internal/domain"
)

// CheckpointFormat is the layout of the Data of a Checkpoint: the tar.gz of
// Export (manifest, transcript and subagents of one session).
const CheckpointFormat = "claude-session-tar-gz/v1"

// Checkpoint exports the session of the Run, which is no longer running: the
// Worker stopped the process before asking. It is h.SessionID, the one Start
// or Resume said in their Launch; only a Handle without it falls back to the
// only transcript in the directory of the Run.
func (a Adapter) Checkpoint(ctx context.Context, _ harness.Host, h harness.Handle) (harness.Checkpoint, error) {
	id, err := runSession(h)
	if err != nil {
		return harness.Checkpoint{}, err
	}
	var buf bytes.Buffer
	ref := harness.SessionRef{ConfigDir: h.ConfigDir, Cwd: h.Worktree, SessionID: id}
	if _, err := a.Export(ctx, ref, harness.ExportOptions{}, &buf); err != nil {
		return harness.Checkpoint{}, err
	}
	return harness.Checkpoint{Harness: ID, Format: CheckpointFormat, Data: buf.Bytes()}, nil
}

// runSession is the session of a Run: the one its Handle carries or, for a
// Run that predates that, the only transcript in its directory.
func runSession(h harness.Handle) (string, error) {
	if h.ConfigDir == "" {
		return "", errors.New("claude-code: the Run has no config directory to find its session in")
	}
	if h.SessionID != "" {
		return h.SessionID, nil
	}
	dir := projectDir(h.ConfigDir, h.Worktree)
	transcripts, err := filepath.Glob(filepath.Join(dir, "*.jsonl"))
	if err != nil {
		return "", err
	}
	transcripts = slices.DeleteFunc(transcripts, func(p string) bool {
		return !validSessionID(strings.TrimSuffix(filepath.Base(p), ".jsonl"))
	})
	switch len(transcripts) {
	case 0:
		return "", fmt.Errorf("%w: no session of the Run in %s", harness.ErrSessionNotFound, dir)
	case 1:
		return strings.TrimSuffix(filepath.Base(transcripts[0]), ".jsonl"), nil
	}
	return "", fmt.Errorf("claude-code: %d sessions in %s and the Handle does not say which is the Run's", len(transcripts), dir)
}

// Resume installs the session of the checkpoint under the cwd of the Run, in
// its ConfigDir, and starts the process with --resume of it. The session keeps
// its id, so two checkpoints are two different resumes. Headless the prompt is
// what the process is asked to continue with, and it is required; interactive
// it is optional. Everything else is Start's.
//
// It is idempotent: a session that is already there is the one resumed (the
// Run restarted in the same epoch), and its prompt is sent again only if the
// session does not hold it (see planFor).
func (a Adapter) Resume(ctx context.Context, host harness.Host, run harness.RunSpec, cp harness.Checkpoint) (harness.Launch, error) {
	if err := a.validate(run); err != nil {
		return harness.Launch{}, err
	}
	if cp.Harness != ID || cp.Format != CheckpointFormat {
		return harness.Launch{}, fmt.Errorf("claude-code: cannot resume from a checkpoint of %q in format %q", cp.Harness, cp.Format)
	}
	if run.Mode == domain.ModeHeadless && run.Prompt == "" {
		return harness.Launch{}, errors.New("claude-code: the prompt is empty: a headless resume needs one to continue with")
	}
	imported, err := a.Import(ctx, harness.SessionImport{ConfigDir: run.ConfigDir, Cwd: run.Worktree}, bytes.NewReader(cp.Data))
	var plan sessionPlan
	switch {
	case err == nil:
		plan = sessionPlan{args: []string{"--resume=" + imported.SessionID}, prompt: run.Prompt, sessionID: imported.SessionID}
	case errors.Is(err, harness.ErrSessionExists):
		plan, err = planFor(run, imported.SessionID)
	}
	if err != nil {
		return harness.Launch{}, err
	}
	return a.launch(host, run, plan)
}

// sessionHolds tells whether the transcript of the session contains prompt
// (see harness.TranscriptHolds).
func sessionHolds(configDir, cwd, id, prompt string) (bool, error) {
	raw, err := os.ReadFile(filepath.Join(sessionProjectDir(configDir, cwd, id), id+".jsonl")) //nolint:gosec // a file of the Run
	if err != nil {
		return false, fmt.Errorf("claude-code: reading the session %s: %w", id, err)
	}
	return harness.TranscriptHolds(raw, prompt)
}
