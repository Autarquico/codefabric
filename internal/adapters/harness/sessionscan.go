package harness

import (
	"bytes"
	"errors"
	"fmt"
)

// TranscriptLine is what an adapter says of one line of a transcript.
type TranscriptLine struct {
	// Version is the version of the harness the line names, if it does.
	Version string
	// ToolCall marks a line where the model calls a tool: the cut of a
	// handoff is the last one.
	ToolCall bool
}

// ScanTranscript checks that every line is JSON (inspect fails on one that is
// not), counts them and, when cut, drops what follows the last line inspect
// marks as a tool call. The result ends every line with a newline. The last
// line of a session that is still being written, or whose process was killed
// mid-write, may not be JSON: with a cut it is tolerated when it falls after
// the cut; with no cut it is left out if it is incomplete (no newline), with a
// warning in the manifest. Any other line that is not JSON is an error.
func ScanTranscript(adapter string, raw []byte, cut bool, inspect func(line []byte) (TranscriptLine, error)) ([]byte, SessionManifest, error) {
	var (
		manifest SessionManifest
		lines    [][]byte
		last     = -1
	)
	for n := 1; len(raw) > 0; n++ {
		var line []byte
		var hasNewline bool
		line, raw, hasNewline = bytes.Cut(raw, []byte("\n"))
		info, err := inspect(line)
		if err != nil {
			if len(raw) > 0 || (!cut && hasNewline) {
				return nil, manifest, fmt.Errorf("%s: line %d of the transcript is not a JSON object: %w", adapter, n, err)
			}
			// The last line: whether it survives is decided below.
			if !cut {
				manifest.Warnings = append(manifest.Warnings, fmt.Sprintf("line %d of the transcript is incomplete and was left out", n))
				break
			}
			lines = append(lines, line)
			continue
		}
		if info.Version != "" {
			manifest.Version = info.Version
		}
		if info.ToolCall {
			last = len(lines)
		}
		lines = append(lines, line)
	}
	if cut {
		if last < 0 {
			return nil, manifest, errors.New(adapter + ": the session has no tool call to cut at")
		}
		lines, manifest.Cut = lines[:last+1], true
	}
	manifest.Lines = len(lines)
	var out bytes.Buffer
	for _, l := range lines {
		out.Write(l)
		out.WriteByte('\n')
	}
	return out.Bytes(), manifest, nil
}
