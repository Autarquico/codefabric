package notify

import (
	"regexp"
	"strings"
	"unicode"
)

// maxText bounds what an event may put on a screen or in a notification.
const maxText = 200

var (
	oscSequence = regexp.MustCompile(`\x1b\][^\x07\x1b]*(?:\x07|\x1b\\)?`)
	csiSequence = regexp.MustCompile(`\x1b\[[0-?]*[ -/]*[@-~]?`)
)

// Sanitize makes text from an event fit to show: ANSI sequences, control
// and format characters (bidi overrides, zero-width) are gone, whitespace runs are one space, and it is cut to maxText runes.
func Sanitize(text string) string {
	text = oscSequence.ReplaceAllString(text, "")
	text = csiSequence.ReplaceAllString(text, "")
	var b strings.Builder
	space := false
	for _, r := range text {
		switch {
		case unicode.Is(unicode.Cf, r):
			// dropped: it joins the words around it
		case unicode.IsSpace(r) || unicode.IsControl(r):
			space = b.Len() > 0
		default:
			if space {
				b.WriteByte(' ')
				space = false
			}
			b.WriteRune(r)
		}
	}
	runes := []rune(b.String())
	if len(runes) > maxText {
		return string(runes[:maxText-1]) + "…"
	}
	return string(runes)
}
