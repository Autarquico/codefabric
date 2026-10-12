package domain

import (
	"regexp"
	"strings"
	"unicode"
	"unicode/utf8"
)

var commitID = regexp.MustCompile(`^(?:[0-9a-f]{40}|[0-9a-f]{64})$`)

// ValidCommitID reports whether s is a full, lower-case hex commit ID (SHA-1 or SHA-256).
func ValidCommitID(s string) bool { return commitID.MatchString(s) }

// Printable reports whether s is valid UTF-8 with no control or format
// characters: text that can be shown, logged and put in a report as it is.
func Printable(s string) bool {
	return utf8.ValidString(s) && !strings.ContainsFunc(s, func(r rune) bool { return unicode.IsControl(r) || unicode.Is(unicode.Cf, r) })
}

// MaxSourceRefLen bounds a source ref.
const MaxSourceRefLen = 255

// ValidSourceRef reports whether s is a ref (a branch or tag name) and nothing
// that git could read as more: not a refspec (`:`, a leading `+`, `*`), not a
// revision expression (`^`, `~`, `@{`, `..`), not an option, and no spaces or
// control characters. git has the last word on the rest.
func ValidSourceRef(s string) bool {
	if s == "" || len(s) > MaxSourceRefLen || s[0] == '-' || s[0] == '+' || strings.Contains(s, "..") || strings.Contains(s, "@{") {
		return false
	}
	return !strings.ContainsFunc(s, func(r rune) bool {
		return r <= ' ' || r == 0x7f || strings.ContainsRune(":*^~?[\\", r) || unicode.IsControl(r) || unicode.Is(unicode.Cf, r)
	})
}
