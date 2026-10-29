package redact

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"math/rand/v2"
	"strings"
	"testing"
)

// oracle is the specification of the redaction, written apart from the
// automaton: it derives the needles from scratch and, at each position, takes
// the longest needle that starts there, the earlier secret on a tie.
func oracle(in []byte, secrets []Needle) []byte {
	type needle struct{ value, ref string }
	var needles []needle
	for _, s := range secrets {
		v := string(s.Value)
		t := v
		if strings.HasSuffix(t, "\n") {
			t = strings.TrimSuffix(strings.TrimSuffix(t, "\n"), "\r")
		}
		pty := func(x string) string { return strings.ReplaceAll(x, "\n", "\r\n") }
		for _, x := range []string{v, t, pty(v), pty(t)} {
			needles = append(needles, needle{x, s.Ref})
		}
		if !strings.Contains(t, "\n") {
			continue
		}
		for _, line := range strings.Split(t, "\n") {
			if line = strings.TrimSuffix(line, "\r"); line != "" {
				needles = append(needles, needle{line, s.Ref})
			}
		}
	}
	// The end of the stream that starts a needle (the longest suffix that is a
	// proper prefix of one) is not text: what no needle matches there is PartialMarker.
	partial := len(in)
	for i := len(in) - 1; i >= 0; i-- {
		for _, n := range needles {
			if len(n.value) > len(in)-i && strings.HasPrefix(n.value, string(in[i:])) {
				partial = i
			}
		}
	}
	var out []byte
	marked := false
	for i := 0; i < len(in); {
		best := -1
		for k, n := range needles {
			if bytes.HasPrefix(in[i:], []byte(n.value)) && (best < 0 || len(n.value) > len(needles[best].value)) {
				best = k
			}
		}
		if best < 0 && i >= partial {
			if !marked {
				out = append(out, PartialMarker...)
				marked = true
			}
			i++
			continue
		}
		if best < 0 {
			out = append(out, in[i])
			i++
			continue
		}
		marked = false
		out = append(out, "[REDACTED:"+needles[best].ref+"]"...)
		i += len(needles[best].value)
	}
	return out
}

func mustSet(t testing.TB, secrets []Needle) *Set {
	t.Helper()
	set, err := NewSet(secrets)
	if err != nil {
		t.Fatal(err)
	}
	return set
}

func secrets(pairs ...string) []Needle {
	var out []Needle
	for ; len(pairs) >= 2; pairs = pairs[2:] {
		out = append(out, Needle{Ref: pairs[0], Value: []byte(pairs[1])})
	}
	return out
}

// stream redacts the chunks one Write each, then closes, checking that the
// Writer holds back only the end of a match in progress: shorter than the
// longest needle. After a redaction that match may have begun inside the
// redacted needle: the bound is kept, not minimality, so the work stays linear.
func stream(t testing.TB, set *Set, chunks ...[]byte) []byte {
	t.Helper()
	var out bytes.Buffer
	w := NewWriter(&out, set)
	for _, c := range chunks {
		if n, err := w.Write(c); err != nil || n != len(c) {
			t.Fatalf("Write = %d, %v", n, err)
		}
		checkHeld(t, w)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	return out.Bytes()
}

func checkHeld(t testing.TB, w *Writer) {
	t.Helper()
	held := string(w.held[w.head:])
	if held == "" {
		return
	}
	for n := range needlesOf(w.set) {
		for k := len(held); k < len(n); k++ {
			if strings.HasSuffix(n[:k], held) {
				return
			}
		}
	}
	t.Fatalf("held %q is no end of a proper prefix of a needle", held)
}

func TestWriterRedactsAgainstTheOracle(t *testing.T) {
	for name, tc := range map[string]struct {
		secrets []Needle
		in      string
		want    string
	}{
		"plain": {secrets("tok", "s3cr3t-value"), "nothing here\n", "nothing here\n"},
		"every occurrence": {
			secrets("tok", "s3cr3t-value"),
			"a s3cr3t-values3cr3t-value b", "a [REDACTED:tok][REDACTED:tok] b",
		},
		"prefix of the secret is not redacted": {
			secrets("tok", "s3cr3t-value"),
			"s3cr3t-valu and s3cr3t-", "s3cr3t-valu and [REDACTED]",
		},
		"one secret is a prefix of another": {
			secrets("short", "abcdefgh", "long", "abcdefghijkl"),
			"abcdefghijkl abcdefghijk", "[REDACTED:long] [REDACTED:short][REDACTED]",
		},
		"overlap takes the leftmost": {
			secrets("a", "01234567", "b", "45678901"),
			"0123456789012", "[REDACTED:a]89[REDACTED]",
		},
		"overlap later starts": {
			secrets("a", "01234567", "b", "45678901"),
			"x45678901234567", "x[REDACTED:b]23[REDACTED]",
		},
		"inner needle is swallowed": {
			secrets("outer", "xxABCDEFGHxx", "inner", "ABCDEFGH"),
			"-xxABCDEFGHxx-xABCDEFGHx", "-[REDACTED:outer]-x[REDACTED:inner][REDACTED]",
		},
		"self overlap": {
			secrets("a", "aaaaaaaa"),
			strings.Repeat("a", 19), "[REDACTED:a][REDACTED:a][REDACTED]",
		},
		"equal values keep the first ref": {
			secrets("one", "same-value", "two", "same-value"),
			"same-value", "[REDACTED:one]",
		},
		"CRLF line ending": {
			secrets("tok", "s3cr3t-value\n"),
			"x=s3cr3t-value\r\ny", "x=[REDACTED:tok]y",
		},
		"PTY multi-line": {
			secrets("key", "first-line\nsecond-line\n"),
			"<first-line\r\nsecond-line\r\n>", "<[REDACTED:key]>",
		},
		"one line printed": {
			secrets("key", "first-line\nsecond-line\n"),
			"cat: first-line\r\n... second-line", "cat: [REDACTED:key]\r\n... [REDACTED:key]",
		},
		"the marker is plain": {
			secrets("tok", "s3cr3t-value"),
			"[REDACTED:tok]", "[REDACTED:tok]",
		},
		"EOF inside a longer needle": {
			secrets("short", "abcdefgh", "long", "abcdefghijkl"),
			"abcdefghij", "[REDACTED:short][REDACTED]",
		},
		"no secrets": {nil, "pass through", "pass through"},
	} {
		t.Run(name, func(t *testing.T) {
			set := mustSet(t, tc.secrets)
			if got := string(oracle([]byte(tc.in), tc.secrets)); got != tc.want {
				t.Fatalf("the oracle disagrees with the case: %q", got)
			}
			if got := string(Bytes([]byte(tc.in), set)); got != tc.want {
				t.Errorf("Bytes = %q, want %q", got, tc.want)
			}
			in := []byte(tc.in)
			for cut := range len(in) + 1 {
				if got := string(stream(t, set, in[:cut], in[cut:])); got != tc.want {
					t.Errorf("cut at %d = %q, want %q", cut, got, tc.want)
				}
			}
			var bytewise [][]byte
			for i := range in {
				bytewise = append(bytewise, in[i:i+1])
			}
			if got := string(stream(t, set, bytewise...)); got != tc.want {
				t.Errorf("byte by byte = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestWriterEOFAtEveryPosition closes the stream after each byte of inputs
// dense in partial and overlapping needles.
func TestWriterEOFAtEveryPosition(t *testing.T) {
	sec := secrets("a", "abababab", "b", "abababab\nbabababa\n", "c", "babababb", "d", "aabbaabb")
	set := mustSet(t, sec)
	rng := rand.New(rand.NewPCG(1, 2)) //nolint:gosec // reproducible test input
	for range 200 {
		in := make([]byte, rng.IntN(64))
		for i := range in {
			in[i] = "ab\r\n"[rng.IntN(4)]
		}
		for end := range len(in) + 1 {
			want := oracle(in[:end], sec)
			var chunks [][]byte
			for i := 0; i < end; {
				n := min(end-i, rng.IntN(5))
				chunks = append(chunks, in[i:i+n])
				i += n
			}
			if got := stream(t, set, chunks...); !bytes.Equal(got, want) {
				t.Fatalf("EOF at %d of %q = %q, want %q", end, in, got, want)
			}
		}
	}
}

func TestWriterHoldsBackNoCompleteSecret(t *testing.T) {
	set := mustSet(t, secrets("tok", "s3cr3t-value"))
	var out bytes.Buffer
	w := NewWriter(&out, set)
	if _, err := w.Write([]byte("say s3cr3t-")); err != nil {
		t.Fatal(err)
	}
	if out.String() != "say " {
		t.Fatalf("before the end of the secret = %q", out.String())
	}
	if _, err := w.Write([]byte("value!")); err != nil {
		t.Fatal(err)
	}
	if out.String() != "say [REDACTED:tok]!" {
		t.Fatalf("after the end of the secret = %q", out.String())
	}
}

func TestWriterReplacesAPartialMatchAtTheEnd(t *testing.T) {
	set := mustSet(t, secrets("tok", "s3cr3t-value"))
	var out bytes.Buffer
	w := NewWriter(&out, set)
	_, _ = w.Write([]byte("say s3cr3t-val"))
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	if got := out.String(); got != "say "+PartialMarker {
		t.Errorf("output = %q, want the held start of the secret replaced", got)
	}
}

func TestWriterMatchedCountsOnlyWholeValues(t *testing.T) {
	set := mustSet(t, secrets("tok", "s3cr3t-value"))
	for _, tc := range []struct {
		in   string
		want bool
	}{
		{"say s3cr3t-val", false},
		{"say s3cr3t-value", true},
		{"say s3cr3t-value and s3cr", true},
		{"nothing here", false},
	} {
		w := NewWriter(io.Discard, set)
		_, _ = w.Write([]byte(tc.in))
		if err := w.Close(); err != nil {
			t.Fatal(err)
		}
		if got := w.Matched(); got != tc.want {
			t.Errorf("%q: Matched = %v, want %v", tc.in, got, tc.want)
		}
	}
}

type failingWriter struct {
	budget int // bytes accepted before failing
	short  bool
	got    bytes.Buffer
	calls  int
}

var errDisk = errors.New("disk full")

func (f *failingWriter) Write(p []byte) (int, error) {
	f.calls++
	n := min(len(p), f.budget)
	f.budget -= n
	f.got.Write(p[:n])
	switch {
	case n == len(p):
		return n, nil
	case f.short:
		return n, nil
	default:
		return n, errDisk
	}
}

func TestWriterStopsAtTheFirstWriteError(t *testing.T) {
	set := mustSet(t, secrets("tok", "s3cr3t-value"))
	for _, short := range []bool{false, true} {
		want := errDisk
		if short {
			want = io.ErrShortWrite
		}
		t.Run(fmt.Sprint("short=", short), func(t *testing.T) {
			dst := &failingWriter{budget: 6, short: short}
			w := NewWriter(dst, set)
			if _, err := w.Write([]byte("hello s3cr3t-value world")); !errors.Is(err, want) {
				t.Fatalf("Write err = %v, want %v", err, want)
			}
			calls := dst.calls
			if _, err := w.Write([]byte("more")); !errors.Is(err, want) {
				t.Errorf("Write after the error = %v", err)
			}
			if err := w.Close(); !errors.Is(err, want) {
				t.Errorf("Close after the error = %v", err)
			}
			if dst.calls != calls {
				t.Errorf("dst written after its error")
			}
			if strings.Contains(dst.got.String(), "s3cr3t") {
				t.Errorf("dst got %q", dst.got.String())
			}
		})
	}
}

func TestWriterFailsOnTheFlushOfClose(t *testing.T) {
	set := mustSet(t, secrets("tok", "s3cr3t-value"))
	dst := &failingWriter{budget: 0}
	w := NewWriter(dst, set)
	if _, err := w.Write([]byte("s3cr3t")); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); !errors.Is(err, errDisk) {
		t.Errorf("Close = %v, want the error of dst", err)
	}
}

func TestWriterAfterClose(t *testing.T) {
	var out bytes.Buffer
	w := NewWriter(&out, mustSet(t, nil))
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Errorf("second Close = %v", err)
	}
	if _, err := w.Write([]byte("x")); !errors.Is(err, ErrClosed) {
		t.Errorf("Write after Close = %v", err)
	}
}

// TestWriterRetentionIsBounded feeds a long stream that keeps partial needles
// alive and checks that the Writer's memory does not grow with it.
func TestWriterRetentionIsBounded(t *testing.T) {
	set := mustSet(t, secrets("a", strings.Repeat("a", 32)+"b"))
	w := NewWriter(io.Discard, set)
	chunk := []byte(strings.Repeat("a", 4096))
	for range 256 {
		if _, err := w.Write(chunk); err != nil {
			t.Fatal(err)
		}
		if held := len(w.held) - w.head; held > 32 {
			t.Fatalf("held %d bytes", held)
		}
		if cap(w.held) > 2*4096+64 {
			t.Fatalf("buffer grew to %d", cap(w.held))
		}
	}
}

func BenchmarkWriter32Secrets(b *testing.B) {
	var sec []Needle
	for i := range 32 {
		sec = append(sec, Needle{Ref: fmt.Sprint("s", i), Value: []byte(fmt.Sprintf("sk-%02d-%040d", i, i*7919))})
	}
	set := mustSet(b, sec)
	rng := rand.New(rand.NewPCG(3, 4)) //nolint:gosec // reproducible benchmark input
	const text = "abcdefghijklmnopqrstuvwxyz0123456789 -_=:/\n"
	chunk := make([]byte, 32<<10)
	for i := range chunk {
		chunk[i] = text[rng.IntN(len(text))]
	}
	copy(chunk[1000:], sec[3].Value)
	w := NewWriter(io.Discard, set)
	b.SetBytes(int64(len(chunk)))
	for b.Loop() {
		if _, err := w.Write(chunk); err != nil {
			b.Fatal(err)
		}
	}
}
