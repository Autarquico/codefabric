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
		pageMargin = 256 << 10 // per page, besides the page: the gzip reader and its copy buffers
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
		f.seal() // the sealer between two transactions
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
	pages := uint64(0)
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
		pages++
	}
	if string(read.Sum(nil)) != string(written.Sum(nil)) {
		t.Fatal("the bytes paged are not the bytes written")
	}
	var end runtime.MemStats
	runtime.ReadMemStats(&end)
	// Bytes allocated, not the heap: it does not depend on when the GC runs, and a
	// page that loads a whole segment shows up in it.
	allocated := end.TotalAlloc - base.TotalAlloc
	if budget := pages * (v1.OutputPageBytes + pageMargin); allocated > budget {
		t.Fatalf("paging %d MiB in %d pages allocated %d MiB, over %d MiB: the output was not paged", total>>20, pages, allocated>>20, budget>>20)
	}
	t.Logf("paged %d MiB in %d pages; %d MiB allocated", total>>20, pages, allocated>>20)
}
