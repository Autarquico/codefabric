//go:build integration

package store

import (
	"crypto/sha256"
	"encoding/binary"
	"runtime"
	"testing"

	v1 "github.com/Autarquico/codefabric/internal/api/v1"
)

// TestScenario_LogsOutputStorage_Volumen: 100 MB of output is stored and paged
// back without the memory ever holding it. The output is built chunk by chunk
// and never kept; what is read is compared by hash, page by page.
func TestScenario_LogsOutputStorage_Volumen(t *testing.T) {
	const (
		total      = 100 << 20
		heapBudget = 48 << 20 // a page is 256 KiB and a segment 1 MiB: the budget is generous
	)
	f, run := outputFixture(t)

	// Text that compresses, as logs do, from a counter so that every chunk differs.
	chunk := func(n int) []byte {
		b := make([]byte, v1.OutputChunkBytes)
		for line := b; len(line) >= 32; line = line[32:] {
			copy(line, "log line number ...............\n")
			binary.BigEndian.PutUint64(line[20:], uint64(n)*uint64(len(b))+uint64(len(b)-len(line))) //nolint:gosec // small
		}
		return b
	}
	written := sha256.New()
	chunks := total / v1.OutputChunkBytes
	for first := 0; first < chunks; first += 16 { // a mebibyte per transaction
		f.tx(func(tx *Tx) error {
			for n := first; n < min(first+16, chunks); n++ {
				c := chunk(n)
				if _, err := tx.AppendOutput(t.Context(), run, 1, uint64(n)*v1.OutputChunkBytes, c, f.clk.Now()); err != nil { //nolint:gosec // small
					return err
				}
			}
			return nil
		})
	}
	for n := range chunks {
		written.Write(chunk(n))
	}

	var o Output
	if err := f.db.Read(t.Context(), func(q Querier) (err error) {
		o, err = OutputOf(t.Context(), q, run, 1)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if o.Start != 0 || o.End != total || o.Sealed != total {
		t.Fatalf("held [%d, %d) sealed %d", o.Start, o.End, o.Sealed)
	}

	runtime.GC()
	var base runtime.MemStats
	runtime.ReadMemStats(&base)
	peak := uint64(0)
	read := sha256.New()
	for from := uint64(0); from < o.End; from += v1.OutputPageBytes {
		to := min(from+v1.OutputPageBytes, o.End)
		var page []byte
		if err := f.db.Read(t.Context(), func(q Querier) (err error) {
			page, err = f.db.OutputRange(t.Context(), q, run, 1, from, to)
			return err
		}); err != nil {
			t.Fatal(err)
		}
		read.Write(page)
		var m runtime.MemStats
		runtime.ReadMemStats(&m)
		peak = max(peak, m.HeapAlloc)
	}
	if string(read.Sum(nil)) != string(written.Sum(nil)) {
		t.Fatal("the bytes paged are not the bytes written")
	}
	if peak > base.HeapAlloc+heapBudget {
		t.Fatalf("peak heap %d MiB over a baseline of %d MiB paging %d MiB: the output was not paged",
			peak>>20, base.HeapAlloc>>20, total>>20)
	}
	t.Logf("paged %d MiB; peak heap %d MiB (baseline %d MiB)", total>>20, peak>>20, base.HeapAlloc>>20)
}
