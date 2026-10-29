package redact

import (
	"bytes"
	"errors"
	"io"
)

// ErrClosed is returned by a Write after Close.
var ErrClosed = errors.New("redact: writer closed")

// Writer redacts a stream on its way to dst: each leftmost-longest occurrence
// of a needle of its Set becomes "[REDACTED:<ref>]", however the stream is
// split into writes. It holds back only the end of the stream that a match in
// progress spans, always shorter than the longest needle, and has no flush
// timer: flushing on idleness would leak a secret printed in two goes. What it
// holds when Close ends the stream becomes [PartialMarker].
//
// A Writer is not safe for concurrent use. After an error of dst every call
// returns that error and nothing else reaches dst.
type Writer struct {
	dst   io.Writer
	set   *Set
	state int32
	// held[head:] is the undecided end of the stream; best[i] is the longest
	// needle seen so far that starts at held[i], or -1.
	held []byte
	best []int32
	head int
	out  []byte
	err  error
	// matched says a whole needle has been redacted.
	matched bool
	// whole says the input is a whole text, not a stream that may have been
	// cut: a start of a needle at its end is text, not a secret cut short.
	whole bool
}

// NewWriter returns a Writer that redacts set on its way to dst.
func NewWriter(dst io.Writer, set *Set) *Writer {
	return &Writer{dst: dst, set: set}
}

// Write redacts p; it may hold back the end of p until the next Write or Close.
func (w *Writer) Write(p []byte) (int, error) {
	if w.err != nil {
		return 0, w.err
	}
	s := w.set
	for _, c := range p {
		w.state = s.step(w.state, c)
		w.held = append(w.held, c)
		w.best = append(w.best, -1)
		w.record()
		w.emit(len(w.held) - int(s.live[w.state]))
	}
	if err := w.flush(); err != nil {
		return 0, err
	}
	return len(p), nil
}

// Matched says whether a whole needle has been redacted so far; the start of
// one that a Close turns into PartialMarker is not.
func (w *Writer) Matched() bool { return w.matched }

// PartialMarker stands in the output for the end of a stream that stopped
// inside a match: the start of a secret whose rest never came. It is not
// written as it is, for a prefix of a secret is part of it.
const PartialMarker = "[REDACTED]"

// Close writes what was held back, redacting the longest needle at each
// position, and leaves dst open. Later Writes fail with ErrClosed. The held
// bytes that are only the start of a needle become PartialMarker, one per run.
func (w *Writer) Close() error {
	if w.err != nil {
		if errors.Is(w.err, ErrClosed) {
			return nil
		}
		return w.err
	}
	w.emitEnd()
	err := w.flush()
	if err == nil {
		w.err = ErrClosed
	}
	return err
}

// record notes every needle that ends at the last held byte at its start.
func (w *Writer) record() {
	s := w.set
	node := w.state
	if s.term[node] < 0 {
		node = s.dict[node]
	}
	for ; node >= 0; node = s.dict[node] {
		needle := s.term[node]
		start := len(w.held) - int(s.lens[needle])
		if start < w.head {
			continue // it starts in output already decided
		}
		if b := w.best[start]; b < 0 || s.lens[b] < s.lens[needle] {
			w.best[start] = needle
		}
	}
}

// emit decides the held bytes before index final, whose longest needle can no
// longer grow, and drops them; a needle decided there may reach past final.
func (w *Writer) emit(final int) {
	s := w.set
	i := w.head
	for i < final {
		if needle := w.best[i]; needle >= 0 {
			w.out = append(w.out, s.markers[s.refOf[needle]]...)
			w.matched = true
			i += int(s.lens[needle])
			continue
		}
		w.out = append(w.out, w.held[i])
		i++
	}
	w.head = i
	if w.head*2 >= len(w.held) { // amortised: it copies no more than it dropped
		w.held = w.held[:copy(w.held, w.held[w.head:])]
		w.best = w.best[:copy(w.best, w.best[w.head:])]
		w.head = 0
	}
}

// emitEnd decides the whole held end of the stream. What no needle matched
// inside the bytes of a match in progress is not text: it may be the start of
// a secret, so it is replaced rather than flushed.
func (w *Writer) emitEnd() {
	s := w.set
	partial := len(w.held) - int(s.live[w.state])
	w.emit(partial)
	marked := false
	for i := w.head; i < len(w.held); {
		if needle := w.best[i]; needle >= 0 {
			w.out = append(w.out, s.markers[s.refOf[needle]]...)
			w.matched = true
			i += int(s.lens[needle])
			marked = false
			continue
		}
		switch {
		case w.whole:
			w.out = append(w.out, w.held[i])
		case !marked:
			w.out = append(w.out, PartialMarker...)
			marked = true
		}
		i++
	}
	w.held, w.best, w.head = w.held[:0], w.best[:0], 0
}

func (w *Writer) flush() error {
	if len(w.out) == 0 {
		return nil
	}
	n, err := w.dst.Write(w.out)
	if err == nil && n < len(w.out) {
		err = io.ErrShortWrite
	}
	w.out = w.out[:0]
	if err != nil {
		w.err = err
	}
	return err
}

// Bytes is the batch form of a Writer: b with every needle of set redacted.
// b is the whole text, so its end is never PartialMarker.
func Bytes(b []byte, set *Set) []byte {
	var out bytes.Buffer
	w := NewWriter(&out, set)
	w.whole = true
	_, _ = w.Write(b)
	_ = w.Close()
	return out.Bytes()
}
