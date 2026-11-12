package gitops

import (
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/Autarquico/codefabric/internal/support/redact"
)

func TestLinesWithSecrets_NamesTheLinesAndNeverTheValue(t *testing.T) {
	f := Forbidden{Values: []redact.Needle{{Ref: "team/token", Value: []byte(tok)}}}
	text := "clean\n{\"a\":\"x " + tok + "\"}\nclean again\n" + tok + " at the end without a break"
	got, err := LinesWithSecrets(strings.NewReader(text), f)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(got, []int{2, 4}) {
		t.Errorf("lines = %v, want [2 4]", got)
	}
}

func TestLinesWithSecrets_NothingToLookForReadsNothing(t *testing.T) {
	got, err := LinesWithSecrets(strings.NewReader(tok), Forbidden{})
	if err != nil || got != nil {
		t.Errorf("got %v, %v", got, err)
	}
}

func TestLinesWithSecrets_RefusesAValueThatLooksLikeAMarker(t *testing.T) {
	f := Forbidden{Values: []redact.Needle{{Ref: "r", Value: []byte("[REDACTED:x]")}}}
	var be *BlockedError
	if _, err := LinesWithSecrets(strings.NewReader("a\n"), f); !errors.As(err, &be) {
		t.Errorf("err = %v, want a BlockedError", err)
	}
}
