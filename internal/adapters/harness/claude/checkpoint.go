package claude

import (
	"bytes"
	"context"
	"encoding/json"
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
// Worker stopped the process before asking. It is the session Start named for
// the Run, or, for a Run that Resume started, the only one in its directory.
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

// runSession is the id of the session of a Run: SessionID(run, epoch), or
// the one transcript in the directory of the Run when Resume started it, which
// keeps the id of the session it resumed.
func runSession(h harness.Handle) (string, error) {
	if h.ConfigDir == "" {
		return "", errors.New("claude-code: the Run has no config directory to find its session in")
	}
	id := SessionID(h.RunID, h.LeaseEpoch)
	if exists, err := sessionExists(h.ConfigDir, h.Worktree, id); err != nil || exists {
		return id, err
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
	return "", fmt.Errorf("claude-code: %d sessions in %s and none is %s: which one is the Run's?", len(transcripts), dir, id)
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
		plan = sessionPlan{args: []string{"--resume=" + imported.SessionID}, prompt: run.Prompt}
	case errors.Is(err, harness.ErrSessionExists):
		var arc archive
		if arc, err = readArchive(ctx, bytes.NewReader(cp.Data), limitOf(0)); err == nil {
			plan, err = planFor(run, arc.manifest.SessionID)
		}
	}
	if err != nil {
		return harness.Launch{}, err
	}
	return a.launch(host, run, plan)
}

// sessionHolds tells whether the transcript of the session contains prompt,
// JSON-escaped, anywhere (it does not tell where). A prompt that the CLI wrote with other escapes
// than ours is not found: it is sent again, which is the safe side.
func sessionHolds(configDir, cwd, id, prompt string) (bool, error) {
	raw, err := os.ReadFile(filepath.Join(sessionProjectDir(configDir, cwd, id), id+".jsonl")) //nolint:gosec // a file of the Run
	if err != nil {
		return false, fmt.Errorf("claude-code: reading the session %s: %w", id, err)
	}
	var needle bytes.Buffer
	enc := json.NewEncoder(&needle)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(prompt); err != nil {
		return false, err
	}
	quoted := bytes.TrimSpace(needle.Bytes())
	return bytes.Contains(raw, quoted[1:len(quoted)-1]), nil
}
