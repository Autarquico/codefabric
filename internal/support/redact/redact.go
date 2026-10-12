// Package redact is the shared, blunt redaction of text that comes from git or
// from a child process before it is logged, reported or shown (Appendix B5).
// It is defence in depth, never a guarantee: the credentials of CodeFabric do
// not travel in the text it sees in the first place.
package redact

import (
	"regexp"
	"strings"
)

const placeholder = "<redacted>"

var word = regexp.MustCompile(`\S+`)

// Text replaces whole every white-space separated word that has an '@', or a
// "://" together with a '?' or '#'; the rest of s is untouched.
func Text(s string) string {
	return word.ReplaceAllStringFunc(s, func(w string) string {
		if strings.Contains(w, "@") || (strings.Contains(w, "://") && strings.ContainsAny(w, "?#")) {
			return placeholder
		}
		return w
	})
}

// Secrets removes every occurrence of each secret from s, then applies Text.
// Secrets shorter than 8 bytes are ignored: they would shred unrelated text.
func Secrets(s string, secrets ...string) string {
	for _, secret := range secrets {
		if len(secret) >= 8 {
			s = strings.ReplaceAll(s, secret, placeholder)
		}
	}
	return Text(s)
}

// Tail keeps the last limit bytes of s, cut at a character boundary, and says so.
// It is for the stderr of a process, whose end is where the cause is.
func Tail(s string, limit int) string {
	if len(s) <= limit {
		return s
	}
	cut := len(s) - limit
	for cut < len(s) && s[cut]&0xC0 == 0x80 {
		cut++
	}
	return "..." + s[cut:]
}
