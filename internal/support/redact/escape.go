package redact

import (
	"errors"
	"fmt"
	"strings"
	"unicode/utf8"
)

// NewEscapedSet is NewSet over the secrets and every escaped form in which a
// harness may print them: the TOML basic string a configuration file is
// rendered with (TOMLBasicString) and every JSON string encoding (the
// structured stream of a harness), also of the TOML form: an event that holds
// the text of the file. It is the one place that derives them, so
// the log, the terminal and the event stream redact the same forms. A form
// equal to the raw value is not repeated; one that overflows the budget is
// ErrTooLarge, as any needle.
func NewEscapedSet(secrets []Needle) (*Set, error) {
	all, err := withEscapes(secrets)
	if err != nil {
		return nil, err
	}
	return NewSet(all)
}

// withEscapes is secrets and their escaped forms. It stops with ErrTooLarge
// as soon as the forms pass the budget, before deriving the rest.
func withEscapes(secrets []Needle) ([]Needle, error) {
	all := append([]Needle(nil), secrets...)
	total := 0
	for _, n := range secrets {
		total += len(n.Value)
		if total > MaxNeedleBytes {
			return nil, fmt.Errorf("%w: at %q", ErrTooLarge, n.Ref)
		}
		seen := map[string]bool{string(n.Value): true}
		add := func(v string) error {
			if seen[v] {
				return nil
			}
			seen[v] = true
			if total += len(v); total > MaxNeedleBytes {
				return fmt.Errorf("%w: at %q", ErrTooLarge, n.Ref)
			}
			all = append(all, Needle{Ref: n.Ref, Value: []byte(v)})
			return nil
		}
		forms := []string{string(n.Value)}
		if toml, err := TOMLBasicString(n.Value); err == nil {
			if err := add(string(toml)); err != nil {
				return nil, err
			}
			forms = append(forms, string(toml)) // and the JSON of the file's text, in an event that carries it
		}
		for _, form := range forms {
			for flags := 0; flags < 1<<5; flags++ {
				if err := add(jsonEscape(form, flags)); err != nil {
					return nil, err
				}
			}
		}
	}
	return all, nil
}

// TOMLBasicString escapes value for the inside of a TOML basic string: the
// quote, the backslash and every control character (U+0000 to U+001F and
// U+007F, which TOML does not allow bare); the rest, Unicode included, is
// written as it is. A value that is not UTF-8 cannot be a TOML string.
func TOMLBasicString(value []byte) ([]byte, error) {
	if !utf8.Valid(value) {
		return nil, errors.New("the value is not valid UTF-8, and a TOML string is")
	}
	const hex = "0123456789abcdef"
	out := make([]byte, 0, len(value)+8)
	for _, r := range string(value) {
		switch {
		case r == '"' || r == '\\':
			out = append(out, '\\', byte(r))
		case r == '\b':
			out = append(out, `\b`...)
		case r == '\t':
			out = append(out, `\t`...)
		case r == '\n':
			out = append(out, `\n`...)
		case r == '\f':
			out = append(out, `\f`...)
		case r == '\r':
			out = append(out, `\r`...)
		case r < 0x20 || r == 0x7f:
			out = append(out, `\u00`...)
			out = append(out, hex[r>>4], hex[r&0xf])
		default:
			out = utf8.AppendRune(out, r)
		}
	}
	return out, nil
}

// Escape styles of jsonEscape, combined as flags.
const (
	escHTML        = 1 << iota // < > & U+2028 U+2029 as \u escapes
	escSlash                   // / as \/
	escNonASCII                // every non-ASCII rune as \u escapes
	escLongControl             // control characters as \u00XX, never \n \t ...
	escUpperHex                // ÿ rather than ÿ
)

// jsonEscape is s as a JSON encoder prints it inside a string, in the style
// flags choose. DEL is escaped as \u007f too: encoders differ and the form is
// harmless to match.
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
		case r < 0x20 || r == 0x7f:
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
