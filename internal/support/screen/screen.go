// Package screen cleans what a Worker captured of the screen of a Run before a
// terminal of ours draws it. The capture comes from a process the person does
// not control: it may carry sequences that move the cursor, set the title, write
// the clipboard (OSC 52), ask the terminal questions or switch modes. A panel
// that only watches must draw colours and text and nothing else.
package screen

import (
	"bytes"
	"unicode/utf8"
)

const (
	esc = 0x1b
	bel = 0x07
)

// Sanitize returns the screen with every escape sequence removed except SGR
// (CSI ... m: colours and attributes), the line ends normalized to \n (a \r\n
// and a lone \r are both one \n), and every other control character dropped but
// the tab. Bytes that are not UTF-8 become U+FFFD, so that none of them can be
// read as a C1 control by a terminal that takes 8-bit controls. An unterminated
// string sequence (OSC, DCS, APC, PM, SOS) swallows the rest of the screen: it
// would have swallowed the terminal's as well.
func Sanitize(in []byte) []byte {
	out := make([]byte, 0, len(in))
	for i := 0; i < len(in); {
		b := in[i]
		switch {
		case b == esc:
			i = skipEscape(in, i, &out)
		case b == '\r':
			out = append(out, '\n')
			i++
			if i < len(in) && in[i] == '\n' {
				i++
			}
		case b == '\n' || b == '\t':
			out = append(out, b)
			i++
		case b < 0x20 || b == 0x7f:
			i++ // bell, backspace, form feed, NUL, DEL...
		case b < utf8.RuneSelf:
			out = append(out, b)
			i++
		default:
			r, n := utf8.DecodeRune(in[i:])
			switch {
			case r == utf8.RuneError && n <= 1:
				out = utf8.AppendRune(out, utf8.RuneError)
				i++
			case r >= 0x80 && r <= 0x9f: // C1: the 8-bit forms of the sequences below
				i = skipC1(in, i, n, r)
			default:
				out = append(out, in[i:i+n]...)
				i += n
			}
		}
	}
	return out
}

// skipEscape handles the sequence that starts with ESC at in[i]: it appends it
// to out if it is an SGR and returns where the text goes on.
func skipEscape(in []byte, i int, out *[]byte) int {
	j := i + 1
	if j >= len(in) {
		return j
	}
	switch in[j] {
	case '[':
		end, final := csiEnd(in, j+1)
		if final == 'm' && plainSGR(in[j+1:end-1]) {
			*out = append(*out, in[i:end]...)
		}
		return end
	case ']':
		return stringEnd(in, j+1, true)
	case 'P', 'X', '^', '_':
		return stringEnd(in, j+1, false)
	}
	// Any other ESC: intermediates (0x20-0x2f) and one final byte (0x30-0x7e).
	for j < len(in) && in[j] >= 0x20 && in[j] <= 0x2f {
		j++
	}
	if j < len(in) && in[j] >= 0x30 && in[j] <= 0x7e {
		j++
	}
	return j
}

// skipC1 drops a C1 control; the ones that open a sequence take it with them.
func skipC1(in []byte, i, n int, r rune) int {
	j := i + n
	switch r {
	case 0x9b: // CSI
		end, _ := csiEnd(in, j)
		return end
	case 0x9d: // OSC
		return stringEnd(in, j, true)
	case 0x90, 0x98, 0x9e, 0x9f: // DCS, SOS, PM, APC
		return stringEnd(in, j, false)
	}
	return j
}

// csiEnd is where a CSI whose parameters start at in[j] ends, and its final
// byte (0 if the input ended first). Parameter bytes are 0x30-0x3f, intermediates
// 0x20-0x2f and the final one 0x40-0x7e; a byte that is none of them aborts the
// sequence before it, as a terminal does.
func csiEnd(in []byte, j int) (end int, final byte) {
	for j < len(in) {
		c := in[j]
		switch {
		case c >= 0x20 && c <= 0x3f:
			j++
		case c >= 0x40 && c <= 0x7e:
			return j + 1, c
		default:
			return j, 0
		}
	}
	return j, 0
}

// plainSGR says whether the parameters of an SGR are only digits, ';' and ':' (a
// private marker or an intermediate makes it another sequence that ends in m).
func plainSGR(p []byte) bool {
	for _, c := range p {
		if (c < '0' || c > '9') && c != ';' && c != ':' {
			return false
		}
	}
	return true
}

// stringEnd is where a string sequence that starts at in[j] ends: at ST (ESC \ or
// U+009C), or at BEL if bel is set (OSC only). An ESC that is not part of ST ends
// it too, and is read again as what it starts. With no end it is the end of the input.
func stringEnd(in []byte, j int, belEnds bool) int {
	for j < len(in) {
		switch {
		case belEnds && in[j] == bel:
			return j + 1
		case in[j] == esc:
			if j+1 < len(in) && in[j+1] == '\\' {
				return j + 2
			}
			return j
		case bytes.HasPrefix(in[j:], []byte{0xc2, 0x9c}):
			return j + 2
		}
		j++
	}
	return j
}
