package cli

import (
	"unicode"
	"unicode/utf8"
)

// maxSequence bounds how much of one escape sequence is read before it is given
// up: an unterminated string must not hide the output that follows it forever.
const maxSequence = 4 << 10

type sanitizeState uint8

const (
	groundState  sanitizeState = iota
	escState                   // after ESC
	escInterm                  // ESC and intermediate bytes (0x20-0x2f), waiting for the final byte
	csiState                   // ESC [ ...
	stringState                // OSC, DCS, SOS, PM or APC: up to BEL or ST
	stringEscape               // an ESC inside a string: ST (ESC \) if a backslash follows
)

// sanitizer removes from output that came from a Run what could act on the
// terminal of the person reading it: escape sequences (ESC, CSI, OSC, DCS and
// the other strings), and every control character but newline and tab. Text,
// including multi-byte, goes through. It is incremental: the output arrives in
// chunks that can cut a sequence or a character anywhere, and what it has read
// of one chunk is kept for the next. Reset it where the stream breaks (a gap,
// a new epoch), since what was cut then will not continue.
type sanitizer struct {
	raw     bool
	state   sanitizeState
	length  int // bytes read of the sequence in progress
	pending []byte
}

// Reset forgets a sequence or a character in progress.
func (s *sanitizer) Reset() {
	s.state, s.length, s.pending = groundState, 0, s.pending[:0]
}

// Filter returns the part of chunk that is safe to print. In raw mode it is the chunk.
func (s *sanitizer) Filter(chunk []byte) []byte {
	if s.raw {
		return chunk
	}
	out := make([]byte, 0, len(chunk))
	for _, b := range chunk {
		out = s.step(out, b)
	}
	return out
}

func (s *sanitizer) step(out []byte, b byte) []byte {
	if s.state != groundState {
		return s.inSequence(out, b)
	}
	if len(s.pending) > 0 {
		return s.inCharacter(out, b)
	}
	switch {
	case b == 0x1b:
		s.state, s.length = escState, 1
	case b == '\n' || b == '\t':
		out = append(out, b)
	case b < 0x20 || b == 0x7f:
	case b < utf8.RuneSelf:
		out = append(out, b)
	default:
		s.pending = append(s.pending, b)
		if utf8.FullRune(s.pending) {
			out = s.flushCharacter(out)
		}
	}
	return out
}

// inCharacter continues a multi-byte character that a chunk cut.
func (s *sanitizer) inCharacter(out []byte, b byte) []byte {
	if !utf8.RuneStart(b) {
		s.pending = append(s.pending, b)
		if utf8.FullRune(s.pending) {
			out = s.flushCharacter(out)
		}
		return out
	}
	// A new character begins before the last was complete: that one was broken.
	out = append(out, string(utf8.RuneError)...)
	s.pending = s.pending[:0]
	return s.step(out, b)
}

func (s *sanitizer) flushCharacter(out []byte) []byte {
	r, _ := utf8.DecodeRune(s.pending)
	s.pending = s.pending[:0]
	if r == utf8.RuneError || unicode.IsControl(r) {
		if r == utf8.RuneError {
			return append(out, string(utf8.RuneError)...)
		}
		return out // a C1 control (U+0080 to U+009F)
	}
	return utf8.AppendRune(out, r)
}

func (s *sanitizer) inSequence(out []byte, b byte) []byte {
	if s.length++; s.length > maxSequence {
		s.Reset()
		return out
	}
	switch s.state {
	case escState:
		switch {
		case b == '[':
			s.state = csiState
		case b == ']' || b == 'P' || b == 'X' || b == '^' || b == '_':
			s.state = stringState
		case b >= 0x20 && b <= 0x2f:
			s.state = escInterm
		case b == 0x1b:
			s.length = 1 // ESC ESC: the first was nothing
		case b == 0x18 || b == 0x1a:
			s.Reset()
		default:
			s.Reset() // a two-byte sequence (ESC 7, ESC =, ESC c ...) is complete
		}
	case escInterm:
		switch {
		case b >= 0x20 && b <= 0x2f:
		case b == 0x1b:
			s.state, s.length = escState, 1
		default:
			s.Reset()
		}
	case csiState:
		switch {
		case b >= 0x40 && b <= 0x7e:
			s.Reset()
		case b == 0x1b:
			s.state, s.length = escState, 1
		case b == 0x18 || b == 0x1a:
			s.Reset()
		}
	case stringState:
		switch b {
		case 0x07, 0x18, 0x1a:
			s.Reset()
		case 0x1b:
			s.state = stringEscape
		}
	case stringEscape:
		switch b {
		case '\\':
			s.Reset()
		case 0x1b:
		default:
			// ESC inside a string that is not ST ends the string and starts a sequence.
			s.state, s.length = escState, 1
			return s.inSequence(out, b)
		}
	}
	return out
}
