package prompt_test

import (
	"bufio"
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/Autarquico/codefabric/internal/support/prompt"
)

func TestAsk_DefaultAndAnswer(t *testing.T) {
	var out strings.Builder
	p := prompt.New(strings.NewReader("\n  maria  \nlast"), &out)
	for _, want := range []string{"def", "maria", "last"} {
		got, err := p.Ask("Nombre", "def")
		if err != nil || got != want {
			t.Fatalf("Ask = %q, %v; want %q", got, err, want)
		}
	}
	if _, err := p.Ask("Nombre", "def"); !errors.Is(err, io.EOF) {
		t.Errorf("at the end: %v, want io.EOF", err)
	}
	if !strings.Contains(out.String(), "Nombre [def]: ") {
		t.Errorf("question = %q", out.String())
	}
}

func TestConfirm(t *testing.T) {
	var out strings.Builder
	p := prompt.New(strings.NewReader("\n\nS\nno\ntal\nsí\n"), &out)
	steps := []struct{ def, want bool }{{false, false}, {true, true}, {false, true}, {true, false}, {false, true}}
	for i, s := range steps {
		got, err := p.Confirm("¿Seguro?", s.def)
		if err != nil || got != s.want {
			t.Fatalf("answer %d = %v, %v; want %v", i, got, err, s.want)
		}
	}
	if !strings.Contains(out.String(), "Responde s o n.") {
		t.Errorf("an unknown answer is not corrected: %q", out.String())
	}
}

func TestNew_SharesABufferedReader(t *testing.T) {
	r := bufio.NewReader(strings.NewReader("a\nb\n"))
	first, second := prompt.New(r, io.Discard), prompt.New(r, io.Discard)
	a, _ := first.Ask("x", "")
	b, _ := second.Ask("x", "")
	if a != "a" || b != "b" {
		t.Errorf("got %q, %q", a, b)
	}
}
