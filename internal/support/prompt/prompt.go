// Package prompt asks a person questions over a reader and a writer, so a
// wizard can be driven by a terminal or by a script in a test.
package prompt

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"golang.org/x/sys/unix"
)

// Prompter asks questions on out and reads the answers, a line each, from in.
type Prompter struct {
	in       *bufio.Reader
	out      io.Writer
	terminal bool
}

// Option adjusts a Prompter.
type Option func(*Prompter)

// AsTerminal states whether the input is a terminal, for the cases where it
// cannot be told from the reader (and for tests).
func AsTerminal(terminal bool) Option { return func(p *Prompter) { p.terminal = terminal } }

// New builds a Prompter. A *bufio.Reader is used as is, so that several
// Prompters (or commands that read the same stream) never lose input to each
// other's buffer.
func New(in io.Reader, out io.Writer, opts ...Option) *Prompter {
	p := &Prompter{out: out, terminal: isTerminal(in)}
	switch r := in.(type) {
	case *Input:
		p.in, p.terminal = r.Reader, r.terminal
	case *bufio.Reader:
		p.in = r
	default:
		p.in = bufio.NewReader(in)
	}
	for _, o := range opts {
		o(p)
	}
	return p
}

// isTerminal says whether r is a file whose descriptor answers the
// window-size request: a pipe, a file and /dev/null do not.
func isTerminal(r io.Reader) bool {
	f, ok := r.(*os.File)
	if !ok {
		return false
	}
	_, err := unix.IoctlGetWinsize(int(f.Fd()), unix.TIOCGWINSZ) //nolint:gosec // a descriptor
	return err == nil
}

// Input is the stream of a Prompter handed to a command that reads the same
// input: it shares the buffer, and it keeps saying whether the stream is a
// terminal, which a bare *bufio.Reader cannot.
type Input struct {
	*bufio.Reader
	terminal bool
}

// IsTerminal says whether the stream behind the buffer is a terminal.
func (i *Input) IsTerminal() bool { return i.terminal }

// Buffer is the shared buffer, for a reader that wraps its input in one.
func (i *Input) Buffer() *bufio.Reader { return i.Reader }

// Input is the stream the Prompter reads, to hand to commands that read the same input.
func (p *Prompter) Input() *Input { return &Input{Reader: p.in, terminal: p.terminal} }

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
