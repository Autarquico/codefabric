package execwrap

import (
	"bufio"
	"bytes"
	"encoding/json"
	"io"
	"strings"

	"github.com/Autarquico/codefabric/internal/support/redact"
)

const (
	// maxEventLine bounds one line of the structured stream held in memory.
	maxEventLine = 16 << 20
	// redactedLine and truncatedLine replace a line that cannot be kept as it
	// is; both are valid JSON, so a reader of the stream never meets a broken line.
	redactedLine  = `{"cf_redacted_line":true}`
	truncatedLine = `{"cf_truncated_line":true}`
)

// jsonRedaction is the matcher of the structured stream: the plain needles of
// the output plus every form in which a JSON encoder may print them (quotes,
// backslashes, control characters, \uXXXX of any case, HTML and non-ASCII
// escapes, "\/"), so that a value is redacted whichever way the harness wrote it.
func jsonRedaction(needles []redact.Needle) (*redact.Set, error) {
	all := append([]redact.Needle(nil), needles...)
	for _, n := range needles {
		seen := map[string]bool{string(n.Value): true}
		for flags := 1; flags < 1<<5; flags++ {
			v := jsonEscape(string(n.Value), flags)
			if !seen[v] {
				seen[v] = true
				all = append(all, redact.Needle{Ref: n.Ref, Value: []byte(v)})
			}
		}
	}
	return redact.NewSet(all)
}

// Escape styles of jsonEscape, combined as flags.
const (
	escHTML        = 1 << iota // < > & U+2028 U+2029 as \u escapes
	escSlash                   // / as \/
	escNonASCII                // every non-ASCII rune as \u escapes
	escLongControl             // control characters as \u00XX, never \n \t ...
	escUpperHex                // ÿ rather than ÿ
)

func jsonEscape(s string, flags int) string {
	hex := "0123456789abcdef"
	if flags&escUpperHex != 0 {
		hex = "0123456789ABCDEF"
	}
	u := func(b *strings.Builder, r rune) {
		b.WriteString(`\u`)
		for shift := 12; shift >= 0; shift -= 4 {
			b.WriteByte(hex[(r>>shift)&0xF])
		}
	}
	var b strings.Builder
	for _, r := range s {
		switch {
		case r == '"' || r == '\\':
			b.WriteByte('\\')
			b.WriteRune(r)
		case r == '/' && flags&escSlash != 0:
			b.WriteString(`\/`)
		case r < 0x20 && flags&escLongControl == 0 && strings.ContainsRune("\n\r\t\b\f", r):
			b.WriteString(map[rune]string{'\n': `\n`, '\r': `\r`, '\t': `\t`, '\b': `\b`, '\f': `\f`}[r])
		case r < 0x20:
			u(&b, r)
		case flags&escHTML != 0 && (r == '<' || r == '>' || r == '&' || r == 0x2028 || r == 0x2029):
			u(&b, r)
		case r >= 0x80 && flags&escNonASCII != 0:
			if r > 0xFFFF {
				r -= 0x10000
				u(&b, 0xD800+(r>>10))
				u(&b, 0xDC00+(r&0x3FF))
			} else {
				u(&b, r)
			}
		default:
			b.WriteRune(r)
		}
	}
	return b.String()
}

// copyEvents copies the structured stream line by line, redacting each line
// with set. A line that was valid JSON and no longer is after the redaction
// (a match that swallowed a quote) becomes redactedLine; one beyond
// maxEventLine becomes truncatedLine. Lines are the unit because the
// redaction must not break the JSON of one: the final line, with no newline,
// is kept without one.
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
		if err == bufio.ErrBufferFull {
			continue
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
