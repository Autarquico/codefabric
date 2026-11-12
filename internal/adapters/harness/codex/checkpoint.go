package codex

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"

	"github.com/Autarquico/codefabric/internal/adapters/harness"
	"github.com/Autarquico/codefabric/internal/domain"
)

// CheckpointFormat is the layout of the Data of a Checkpoint: the tar.gz of
// Export (manifest and rollout of one session).
const CheckpointFormat = "codex-session-tar-gz/v1"

// Checkpoint exports the session of the Run, which is no longer running: the
// Worker stopped the process before asking. It is h.SessionID, the one Resume
// said in its Launch; a Run that was started has none (Codex does not let Start
// name the session) and its session is the only rollout of its CODEX_HOME.
func (a Adapter) Checkpoint(ctx context.Context, _ harness.Host, h harness.Handle) (harness.Checkpoint, error) {
	if h.ConfigDir == "" {
		return harness.Checkpoint{}, errors.New("codex: the Run has no config directory to find its session in")
	}
	id := h.SessionID
	if id == "" {
		var err error
		if id, err = onlyRollout(h.ConfigDir); err != nil {
			return harness.Checkpoint{}, err
		}
	}
	var buf bytes.Buffer
	ref := harness.SessionRef{ConfigDir: h.ConfigDir, Cwd: h.Worktree, SessionID: id}
	if _, err := a.Export(ctx, ref, harness.ExportOptions{}, &buf); err != nil {
		return harness.Checkpoint{}, err
	}
	return harness.Checkpoint{Harness: ID, Format: CheckpointFormat, Data: buf.Bytes()}, nil
}

// Resume installs the session of the checkpoint under the sessions of the Run's
// CODEX_HOME, with the cwd of the Run, and starts the process with `resume` of
// it. The session keeps its id, so two checkpoints are two different resumes.
// Headless the prompt is what the process is asked to continue with, and it is
// required; interactive it is optional. Everything else is Start's.
//
// It is idempotent: a session that is already there is the one resumed (the
// Run restarted in the same epoch), and its prompt is sent again only if the
// session does not hold it.
func (a Adapter) Resume(ctx context.Context, host harness.Host, run harness.RunSpec, cp harness.Checkpoint) (harness.Launch, error) {
	if err := a.validate(run); err != nil {
		return harness.Launch{}, err
	}
	if cp.Harness != ID || cp.Format != CheckpointFormat {
		return harness.Launch{}, fmt.Errorf("codex: cannot resume from a checkpoint of %q in format %q", cp.Harness, cp.Format)
	}
	if run.Mode == domain.ModeHeadless && run.Prompt == "" {
		return harness.Launch{}, errors.New("codex: the prompt is empty: a headless resume needs one to continue with")
	}
	imported, err := a.Import(ctx, harness.SessionImport{ConfigDir: run.ConfigDir, Cwd: run.Worktree}, bytes.NewReader(cp.Data))
	prompt := run.Prompt
	switch {
	case err == nil:
	case errors.Is(err, harness.ErrSessionExists):
		if prompt, err = promptToSend(ctx, run, imported.SessionID); err != nil {
			return harness.Launch{}, err
		}
	default:
		return harness.Launch{}, err
	}
	return launch(host, run, imported.SessionID, prompt)
}

// promptToSend is run.Prompt unless the session already holds it: a process
// restarted in the same epoch continues, it does not ask twice. Headless it is
// asked harness.ContinuePrompt instead; interactive, nothing.
func promptToSend(ctx context.Context, run harness.RunSpec, id string) (string, error) {
	files, err := Adapter{}.Locate(ctx, harness.SessionRef{ConfigDir: run.ConfigDir, SessionID: id})
	if err != nil {
		return "", err
	}
	raw, err := os.ReadFile(files.Transcript)
	if err != nil {
		return "", fmt.Errorf("codex: reading the session %s: %w", id, err)
	}
	asked, err := harness.TranscriptHolds(raw, run.Prompt)
	switch {
	case err != nil:
		return "", err
	case !asked:
		return run.Prompt, nil
	case run.Mode == domain.ModeHeadless:
		return harness.ContinuePrompt, nil
	}
	return "", nil
}
