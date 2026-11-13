package harness

import (
	"bytes"
	"fmt"
)

// TranscriptLine is what an adapter says of one line of a transcript.
type TranscriptLine struct {
	// Version is the version of the harness the line names, if it does.
	Version string
	// ToolCall marks a line where the model calls a tool: the cut of a
	// handoff is the last one, when no tool result follows it.
	ToolCall bool
	// ToolResult marks a line that carries the result of a tool call.
	ToolResult bool
}

// ScanTranscript checks that every line is JSON (inspect fails on one that is
// not), counts them and, when cut, drops what follows the last line inspect
// marks as a tool call if no tool result follows it: that call is the caller's
// own, still running. The harness may not have written it yet, and then the
// last call has its result, or there is none: the transcript is kept whole,
// since cutting at an earlier call would lose its result and every turn after
// it. The result ends every line with a newline. The last line of a session
// that is still being written, or whose process was killed mid-write, may not
// be JSON: it is tolerated when a cut leaves it out, and otherwise left out if
// it is incomplete (no newline), with a warning in the manifest. Any other line
// that is not JSON is an error.
func ScanTranscript(adapter string, raw []byte, cut bool, inspect func(line []byte) (TranscriptLine, error)) ([]byte, SessionManifest, error) {
	var (
		manifest SessionManifest
		lines    [][]byte
		last     = -1
		answered bool
		broken   error // the last line is not JSON
		complete bool  // and ends with a newline
	)
	for n := 1; len(raw) > 0; n++ {
		var line []byte
		line, raw, complete = bytes.Cut(raw, []byte("\n"))
		info, err := inspect(line)
		if err != nil {
			if len(raw) > 0 {
				return nil, manifest, fmt.Errorf("%s: line %d of the transcript is not a JSON object: %w", adapter, n, err)
			}
			broken = err
			break
		}
		if info.Version != "" {
			manifest.Version = info.Version
		}
		if info.ToolCall {
			last, answered = len(lines), false
		}
		if info.ToolResult && last >= 0 {
			answered = true
		}
		lines = append(lines, line)
	}
	switch {
	case cut && last >= 0 && !answered:
		lines, manifest.Cut = lines[:last+1], true
	case broken != nil && complete:
		return nil, manifest, fmt.Errorf("%s: line %d of the transcript is not a JSON object: %w", adapter, len(lines)+1, broken)
	case broken != nil:
		manifest.Warnings = append(manifest.Warnings, fmt.Sprintf("line %d of the transcript is incomplete and was left out", len(lines)+1))
	}
	manifest.Lines = len(lines)
	var out bytes.Buffer
	for _, l := range lines {
		out.Write(l)
		out.WriteByte('\n')
	}
	return out.Bytes(), manifest, nil
}
