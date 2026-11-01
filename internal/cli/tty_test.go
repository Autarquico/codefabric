package cli

import (
	"io"
	"strings"
	"testing"

	"github.com/Autarquico/codefabric/internal/support/prompt"
)

func TestIsTerminal_AnInputThatSaysSoIsOne(t *testing.T) {
	in := strings.NewReader("")
	if isTerminal(prompt.New(in, io.Discard).Input()) {
		t.Error("a pipe behind a prompter is a terminal")
	}
	if !isTerminal(prompt.New(in, io.Discard, prompt.AsTerminal(true)).Input()) {
		t.Error("the terminal behind a prompter is lost")
	}
}

func TestBufferedInput_ReusesTheBufferOfASharedInput(t *testing.T) {
	p := prompt.New(strings.NewReader("a\nb\n"), io.Discard, prompt.AsTerminal(true))
	r := bufferedInput(p.Input())
	if line, _ := r.ReadString('\n'); line != "a\n" {
		t.Fatalf("read %q", line)
	}
	if got, _ := p.Ask("x", ""); got != "b" {
		t.Errorf("the prompter lost the input the command buffered: %q", got)
	}
}
