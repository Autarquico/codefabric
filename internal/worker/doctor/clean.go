package doctor

import (
	"strings"
	"unicode"
	"unicode/utf8"

	v1 "github.com/Autarquico/codefabric/internal/api/v1"
)

// MaxText is the longest Name, Message or Action, in bytes, that the Controller
// accepts in a heartbeat. A text of the machine (a file name, a list of
// components) can be anything, and one that is refused would take the whole
// heartbeat with it.
const MaxText = v1.MaxHealthText

const ellipsis = "…"

// Clean makes s fit a heartbeat and a terminal: control and format characters
// become '?', and a text over MaxText bytes is cut, on a rune boundary, with an
// ellipsis.
func Clean(s string) string {
	s = strings.Map(func(r rune) rune {
		if r == utf8.RuneError || unicode.IsControl(r) || unicode.Is(unicode.Cf, r) {
			return '?'
		}
		return r
	}, s)
	if len(s) <= MaxText {
		return s
	}
	cut := MaxText - len(ellipsis)
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut] + ellipsis
}

// CleanComponent applies Clean to every text of c.
func CleanComponent(c v1.ComponentHealth) v1.ComponentHealth {
	c.Name, c.Message, c.Action = Clean(c.Name), Clean(c.Message), Clean(c.Action)
	return c
}
