package execwrap

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"io"

	"github.com/Autarquico/codefabric/internal/support/redact"
)

const (
	// maxEventLine bounds one line of the structured stream held in memory.
	maxEventLine = 16 << 20
	// redactedLine and truncatedLine replace a line that cannot be kept as it
	// is; both are valid JSON, so a reader of the stream never meets a broken line.
	redactedLine  = `{"cf_redacted_line":true}`
	truncatedLine = `{"cf_truncated_line":true}`
	// cutLine stands for the unfinished line of a stream that was cut.
	cutLine = `{"cf_capture_cut":true}`
)

// copyEvents copies the structured stream line by line, redacting each line
// with set. A line that was valid JSON and no longer is after the redaction
// (a match that swallowed a quote) becomes redactedLine; one beyond
// maxEventLine becomes truncatedLine. Lines are the unit because the
// redaction must not break the JSON of one: the final line, with no newline,
// is kept without one. If the read fails (the stream was cut), the line in
// progress is dropped, for it may end in the start of a secret, and cutLine is
// written in its place.
func copyEvents(dst io.Writer, src io.Reader, set *redact.Set) error {
	r := bufio.NewReaderSize(src, 64<<10)
	var line []byte
	tooLong := false
	for {
		chunk, err := r.ReadSlice('\n')
		if !tooLong {
			line = append(line, chunk...)
			if len(line) > maxEventLine {
				line, tooLong = nil, true
			}
		}
		if errors.Is(err, bufio.ErrBufferFull) {
			continue
		}
		if err != nil && err != io.EOF {
			_, _ = io.WriteString(dst, cutLine+"\n")
			return err
		}
		if len(chunk) > 0 || len(line) > 0 || tooLong {
			if werr := writeEventLine(dst, line, tooLong, set); werr != nil {
				return werr
			}
		}
		line, tooLong = line[:0], false
		if err != nil {
			if err == io.EOF {
				return nil
			}
			return err
		}
	}
}

func writeEventLine(dst io.Writer, line []byte, tooLong bool, set *redact.Set) error {
	body := bytes.TrimSuffix(line, []byte("\n"))
	hasNL := len(body) < len(line)
	var out []byte
	switch {
	case tooLong:
		out = []byte(truncatedLine)
		hasNL = true
	default:
		out = redact.Bytes(body, set)
		if json.Valid(body) && !json.Valid(out) {
			out = []byte(redactedLine)
		}
	}
	if hasNL {
		out = append(out, '\n')
	}
	_, err := dst.Write(out)
	return err
}
