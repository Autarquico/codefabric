package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/spf13/cobra"

	v1 "github.com/Autarquico/codefabric/internal/api/v1"
	"github.com/Autarquico/codefabric/internal/domain"
)

const (
	statusLineKey     = "statusLine"
	statusLineCommand = "cf claude statusline"
	// statusLineRefreshSeconds keeps the line fresh at rest: Claude Code runs the
	// command again that often even when nothing happens in the session.
	statusLineRefreshSeconds = 10
	// statusLineTimeout is well under the refresh interval, so a slow Controller
	// never stacks up commands.
	statusLineTimeout = 3 * time.Second
	// statusLineNames is how many Tasks the line names before it says "+N".
	statusLineNames = 2
)

// StatusLineResult is what `cf claude install --statusline` did to the settings file.
type StatusLineResult struct {
	Path   string
	DryRun bool
	// Kept is true when the file had a status line already, which is not replaced.
	Kept bool
}

// settingsPath is the settings file of Claude Code under a scope base.
func settingsPath(base string) string { return filepath.Join(base, ".claude", "settings.json") }

// installStatusLine sets the statusLine of the settings file when it has none, not
// even a null one.
// The other keys stay as they are, byte for byte: the new key goes in before the
// closing brace.
func installStatusLine(path string, dryRun bool) (StatusLineResult, error) {
	res := StatusLineResult{Path: path, DryRun: dryRun}
	mode := fs.FileMode(0o600)
	var have []byte
	switch read, err := readRegular(path); {
	case errors.Is(err, fs.ErrNotExist):
	case err != nil:
		return res, err
	default:
		have = bytes.TrimSpace(read)
		if info, err := os.Stat(path); err == nil {
			mode = info.Mode().Perm()
		}
	}
	settings := map[string]json.RawMessage{}
	if len(have) > 0 {
		if err := json.Unmarshal(have, &settings); err != nil {
			return res, domain.NewError(domain.CodeInvalidRequest, fmt.Sprintf("%s is not a JSON object, so the status line was not installed: %v", path, err), nil)
		}
	}
	if _, ok := settings[statusLineKey]; ok {
		res.Kept = true
		return res, nil
	}
	if dryRun {
		return res, nil
	}
	entry, err := statusLineEntry()
	if err != nil {
		return res, err
	}
	out := []byte("{\n" + entry + "\n}\n")
	if len(settings) > 0 {
		body := bytes.TrimRight(have[:len(have)-1], " \t\r\n")
		out = append(body, ",\n"+entry+"\n}\n"...)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil { //nolint:gosec // the directory of Claude Code settings is readable
		return res, err
	}
	return res, writeAtomic(path, out, mode)
}

// statusLineEntry is the `"statusLine": {...}` member, indented one level, with the
// characters of the command as they are.
func statusLineEntry() (string, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(map[string]any{"type": "command", "command": statusLineCommand, "refreshInterval": statusLineRefreshSeconds}); err != nil {
		return "", err
	}
	var indented bytes.Buffer
	if err := json.Indent(&indented, bytes.TrimSpace(buf.Bytes()), "  ", "  "); err != nil {
		return "", err
	}
	return `  "` + statusLineKey + `": ` + indented.String(), nil
}

func printStatusLine(out io.Writer, r StatusLineResult) error {
	verb := "installed"
	switch {
	case r.Kept:
		verb = "kept"
	case r.DryRun:
		verb = "would be installed"
	}
	_, err := fmt.Fprintf(out, "statusline: %s  %s\n", verb, printable(r.Path))
	if err == nil && r.Kept {
		_, err = fmt.Fprintln(out, "there is a status line already and it was not replaced")
	}
	return err
}

// remoteRunsLine is the status line for the Runs that are live on Workers and that
// a person can attach to: "" when there are none.
func remoteRunsLine(runs []v1.PsEntry) string {
	var names []string
	for _, r := range runs {
		if r.Mode == domain.ModeInteractive && r.Worker != nil {
			names = append(names, printable(r.Task.Name)+"@"+printable(r.Worker.Name))
		}
	}
	if len(names) == 0 {
		return ""
	}
	shown := names[:min(len(names), statusLineNames)]
	line := fmt.Sprintf("⇄ %d remote: %s", len(names), strings.Join(shown, ", "))
	if more := len(names) - len(shown); more > 0 {
		line += fmt.Sprintf(" +%d", more)
	}
	return line
}

// newClaudeStatusLineCmd is the command that Claude Code runs for its status line.
// It never fails: a status line that is not there is better than an error in every
// refresh, so with no Controller it prints nothing.
func newClaudeStatusLineCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "statusline",
		Short: "Print one short line with the Runs that are live on Workers, for the status line of Claude Code",
		Long:  "Print one short line with the interactive Runs that are live on Workers. It prints nothing when there are none or when the Controller does not answer within a few seconds.\nInstall it with `cf claude install --statusline`.",
		Args:  cobra.NoArgs,
		Run: func(cmd *cobra.Command, _ []string) {
			// Claude Code sends the session as JSON on stdin; the line does not need it.
			ctx, cancel := context.WithTimeout(cmd.Context(), statusLineTimeout)
			defer cancel()
			c, err := openClient(cmd)
			if err != nil {
				return
			}
			ps, err := c.Ps(ctx, "")
			if err != nil {
				return
			}
			if line := remoteRunsLine(ps.Runs); line != "" {
				fmt.Fprintln(cmd.OutOrStdout(), line)
			}
		},
	}
}
