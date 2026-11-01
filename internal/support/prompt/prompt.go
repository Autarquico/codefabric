// Package prompt asks a person questions over a reader and a writer, so a
// wizard can be driven by a terminal or by a script in a test.
package prompt

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"strings"
)

// Prompter asks questions on out and reads the answers, a line each, from in.
type Prompter struct {
	in  *bufio.Reader
	out io.Writer
}

// New builds a Prompter. A *bufio.Reader is used as is, so that several
// Prompters (or commands that read the same stream) never lose input to each
// other's buffer.
func New(in io.Reader, out io.Writer) *Prompter {
	r, ok := in.(*bufio.Reader)
	if !ok {
		r = bufio.NewReader(in)
	}
	return &Prompter{in: r, out: out}
}

// Reader is the stream the Prompter reads, to hand to commands that read the
// same input.
func (p *Prompter) Reader() *bufio.Reader { return p.in }

// Out is where the questions go.
func (p *Prompter) Out() io.Writer { return p.out }

// Ask shows the question with its default and returns the answer, or the
// default if the line is empty. The end of the input with nothing typed is
// io.EOF.
func (p *Prompter) Ask(question, def string) (string, error) {
	if def != "" {
		_, _ = fmt.Fprintf(p.out, "%s [%s]: ", question, def)
	} else {
		_, _ = fmt.Fprintf(p.out, "%s: ", question)
	}
	line, err := p.in.ReadString('\n')
	if err != nil && (!errors.Is(err, io.EOF) || line == "") {
		_, _ = fmt.Fprintln(p.out)
		return "", err
	}
	if answer := strings.TrimSpace(line); answer != "" {
		return answer, nil
	}
	return def, nil
}

// Confirm asks a yes/no question in Spanish (s/si/sí/y/yes, n/no); an empty
// answer is def.
func (p *Prompter) Confirm(question string, def bool) (bool, error) {
	hint := "s/N"
	if def {
		hint = "S/n"
	}
	for {
		answer, err := p.Ask(question+" ("+hint+")", "")
		if err != nil {
			return false, err
		}
		switch strings.ToLower(answer) {
		case "":
			return def, nil
		case "s", "si", "sí", "y", "yes":
			return true, nil
		case "n", "no":
			return false, nil
		}
		_, _ = fmt.Fprintln(p.out, "Responde s o n.")
	}
}
