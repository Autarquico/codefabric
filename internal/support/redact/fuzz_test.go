package redact

import (
	"bytes"
	"strings"
	"testing"
)

// FuzzWriterChunking checks that, however the stream is split into writes and
// wherever it ends, the Writer redacts as the oracle does and no complete
// needle reaches dst. Secrets and input share a tiny alphabet so that prefixes,
// overlaps, equal values and line breaks are the common case; the markers use
// none of its bytes, so no needle can straddle one.
func FuzzWriterChunking(f *testing.F) {
	f.Add([]byte("abababab\x00babababa"), []byte("xabababababa\nbabababa\r\nbb"), []byte{3, 0, 7, 1})
	f.Add([]byte("aaaaaaaa\x00aaaaaaaab"), []byte("aaaaaaaaaaaaaaaaab"), []byte{1})
	f.Add([]byte("abababab\nbabababa\n"), []byte("abababab\r\nbabababa\r\n"), []byte{2, 5})
	f.Add([]byte("abababab\x00abababab"), []byte("abababab"), []byte{0})
	f.Fuzz(func(t *testing.T, seed, input, cuts []byte) {
		var sec []Needle
		for i, part := range bytes.Split(seed, []byte{0}) {
			if i == 4 {
				break
			}
			value := make([]byte, len(part))
			for j, c := range part {
				value[j] = "aaabbb\n\r"[c%8]
			}
			sec = append(sec, Needle{Ref: "s" + string(rune('0'+i)), Value: value})
		}
		set, err := NewSet(sec)
		if err != nil {
			t.Skip() // short or unbalanced secrets are rejected, never dropped
		}
		in := make([]byte, len(input))
		for i, c := range input {
			in[i] = "ab\r\nx"[c%5]
		}
		end := len(in)
		if len(cuts) > 0 {
			end = int(cuts[0]) % (len(in) + 1)
		}
		in = in[:end]

		var chunks [][]byte
		for i, k := 0, 1; i < len(in); k++ {
			n := len(in) - i
			if k < len(cuts) {
				n = min(n, int(cuts[k]%9))
			}
			chunks = append(chunks, in[i:i+n])
			i += n
		}
		got := stream(t, set, chunks...)
		if want := oracle(in, sec, false); !bytes.Equal(got, want) {
			t.Fatalf("stream = %q, oracle = %q", got, want)
		}
		if batch, want := Bytes(in, set), oracle(in, sec, true); !bytes.Equal(batch, want) {
			t.Fatalf("Bytes = %q, oracle = %q", batch, want)
		}
		for n := range needlesOf(set) {
			if strings.Contains(string(got), n) {
				t.Fatalf("needle %q in %q", n, got)
			}
		}
	})
}
