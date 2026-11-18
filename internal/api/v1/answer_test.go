package v1_test

import (
	"strings"
	"testing"
	"unicode/utf8"

	v1 "github.com/Autarquico/codefabric/internal/api/v1"
)

func TestBoundAnswerKeepsWhatFitsAndDropsWhatCannotBeShown(t *testing.T) {
	if got := v1.BoundAnswer("line\n\tindented é"); got != "line\n\tindented é" {
		t.Errorf("answer = %q", got)
	}
	if got := v1.BoundAnswer("a\x00b\x1b[31mc‍d\xffe"); !v1.ValidAnswer(got) || strings.ContainsAny(got, "\x00\x1b‍") {
		t.Errorf("answer = %q", got)
	}
}

func TestBoundAnswerCutsOnACharacterBoundaryAndSaysSo(t *testing.T) {
	for _, unit := range []string{"a", "é", "€", "😀"} {
		got := v1.BoundAnswer(strings.Repeat(unit, v1.MaxAnswerLen+1))
		if len(got) > v1.MaxAnswerLen || !utf8.ValidString(got) || !strings.HasSuffix(got, v1.AnswerTruncatedMark) {
			t.Errorf("%q: %d bytes, valid=%v, ends %q", unit, len(got), utf8.ValidString(got), got[len(got)-20:])
		}
	}
}

func TestValidAnswerRefusesWhatTheControllerWouldNotShow(t *testing.T) {
	for name, s := range map[string]string{
		"too long":    strings.Repeat("a", v1.MaxAnswerLen+1),
		"not UTF-8":   "a\xffb",
		"control":     "a\x07b",
		"escape code": "\x1b[2J",
	} {
		if v1.ValidAnswer(s) {
			t.Errorf("%s: accepted", name)
		}
	}
	if !v1.ValidAnswer("") || !v1.ValidAnswer("ok\n\tfine") {
		t.Error("a plain answer was refused")
	}
}
