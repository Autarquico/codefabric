package cli

import (
	"bytes"
	"strings"
	"testing"
	"unicode/utf8"
)

func filterWhole(in string) string {
	s := &sanitizer{}
	return string(s.Filter([]byte(in)))
}

// filterBy feeds the input in chunks of n bytes.
func filterBy(in string, n int) string {
	s := &sanitizer{}
	var out []byte
	for i := 0; i < len(in); i += n {
		out = append(out, s.Filter([]byte(in[i:min(i+n, len(in))]))...)
	}
	return string(out)
}

var corpus = map[string]string{
	"plain":            "hello\tworld\nsecond line\n",
	"csi colour":       "a\x1b[31mred\x1b[0mb",
	"csi with params":  "x\x1b[1;38;5;196mY\x1b[Kz\x1b[2J\x1b[Hq",
	"osc title bel":    "a\x1b]0;evil title\x07b",
	"osc st":           "a\x1b]8;;http://x\x1b\\link\x1b]8;;\x1b\\b",
	"dcs":              "a\x1bPq#0;2;0;0;0\x1b\\b",
	"apc pm sos":       "a\x1b_apc\x1b\\b\x1b^pm\x1b\\c\x1bXsos\x1b\\d",
	"two byte esc":     "a\x1b7b\x1b=c\x1bcd",
	"esc intermediate": "a\x1b(Bb\x1b)0c",
	"controls":         "a\x00b\x01c\x07d\x08e\rf\x7fg\x0bh\x0ci",
	"c1 controls":      "a\u0085b\u009bc\u009dd",
	"multibyte":        "héllo wörld — 日本語 🚀 ok\n",
	"mixed":            "\x1b[32m✓\x1b[0m done\r\n\x1b]0;t\x07next \x1b[?25l",
	"esc in string":    "a\x1b]0;title\x1b[31mred\x1b\\b",
	"cancelled":        "a\x1b[12\x18b\x1b]0;x\x1ac",
	"double esc":       "a\x1b\x1b[31mb",
	"lone esc at end":  "text\x1b",
}

func TestSanitizer_StripsWhatActsOnATerminalAndKeepsTheText(t *testing.T) {
	for name, want := range map[string]string{
		"plain":            "hello\tworld\nsecond line\n",
		"csi colour":       "aredb",
		"csi with params":  "xYzq",
		"osc title bel":    "ab",
		"osc st":           "alinkb",
		"dcs":              "ab",
		"apc pm sos":       "abcd",
		"two byte esc":     "abcd",
		"esc intermediate": "abc",
		"controls":         "abcdefghi",
		"c1 controls":      "abcd",
		"multibyte":        "héllo wörld — 日本語 🚀 ok\n",
		"mixed":            "✓ done\nnext ",
		"cancelled":        "abc",
		"double esc":       "ab",
		"lone esc at end":  "text",
	} {
		if got := filterWhole(corpus[name]); got != want {
			t.Errorf("%s: %q, want %q", name, got, want)
		}
	}
	// An ESC inside a string that does not make ST ends the string and starts a
	// sequence of its own: the title and the colour are both gone.
	if got := filterWhole(corpus["esc in string"]); got != "aredb" {
		t.Errorf("esc in string: %q", got)
	}
}

func TestSanitizer_ACutAtAnyByteGivesTheSameOutputAsNoCut(t *testing.T) {
	for name, in := range corpus {
		whole := filterWhole(in)
		for n := 1; n <= len(in)+1; n++ {
			if got := filterBy(in, n); got != whole {
				t.Errorf("%s in chunks of %d: %q, want %q", name, n, got, whole)
			}
		}
	}
}

func TestSanitizer_ACharacterCutBetweenChunksIsNotBroken(t *testing.T) {
	in := "日本語"
	s := &sanitizer{}
	var out []byte
	for i := 0; i < len(in); i++ {
		out = append(out, s.Filter([]byte{in[i]})...)
	}
	if string(out) != in || !utf8.Valid(out) {
		t.Fatalf("out = %q", out)
	}
	// A character that never completes is replaced, once, and what follows is kept.
	s = &sanitizer{}
	got := string(s.Filter([]byte{0xe6, 0x97, 'a'}))
	if got != "�a" {
		t.Fatalf("got %q", got)
	}
}

func TestSanitizer_NoOutputIsEverInvalidUTF8OrContainsAControl(t *testing.T) {
	var all bytes.Buffer
	for b := range 256 {
		all.WriteByte(byte(b))
	}
	for _, in := range []string{all.String(), strings.Repeat(all.String(), 3), "\xff\xfe\xc3\x28\xe2\x82"} {
		for _, n := range []int{1, 3, 7, 1 << 20} {
			out := filterBy(in, n)
			if !utf8.ValidString(out) {
				t.Fatalf("invalid UTF-8 out of chunks of %d: %q", n, out)
			}
			for _, r := range out {
				if (r < 0x20 && r != '\n' && r != '\t') || r == 0x7f || (r >= 0x80 && r <= 0x9f) {
					t.Fatalf("control %U survived", r)
				}
			}
		}
	}
}

func TestSanitizer_AnUnterminatedSequenceIsGivenUpAfterFourKiB(t *testing.T) {
	s := &sanitizer{}
	long := "\x1b]0;" + strings.Repeat("a", maxSequence+100) + "visible"
	out := string(s.Filter([]byte(long)))
	if strings.Contains(out, "\x1b") || !strings.HasSuffix(out, "visible") {
		t.Fatalf("out = %q", out[max(0, len(out)-40):])
	}
	// Inside the limit, an unterminated string hides what follows it, as a terminal would.
	short := "\x1b]0;" + strings.Repeat("a", 100) + " still hidden"
	if got := filterWhole(short); got != "" {
		t.Fatalf("got %q", got)
	}
}

func TestSanitizer_ResetForgetsASequenceInProgress(t *testing.T) {
	s := &sanitizer{}
	s.Filter([]byte("a\x1b]0;half a title"))
	s.Reset()
	if got := string(s.Filter([]byte("visible"))); got != "visible" {
		t.Fatalf("after Reset: %q", got)
	}
	s.Filter([]byte{0xe6})
	s.Reset()
	if got := string(s.Filter([]byte("ok"))); got != "ok" {
		t.Fatalf("after Reset with half a character: %q", got)
	}
}

func TestSanitizer_RawPassesEverythingThrough(t *testing.T) {
	s := &sanitizer{raw: true}
	in := "\x1b[31mred\x1b[0m\r\x00"
	if got := string(s.Filter([]byte(in))); got != in {
		t.Fatalf("raw = %q", got)
	}
}
