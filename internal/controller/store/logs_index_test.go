package store

import (
	"bytes"
	"errors"
	"math/rand/v2"
	"testing"

	v1 "github.com/Autarquico/codefabric/internal/api/v1"
	"github.com/Autarquico/codefabric/internal/controller/blob"
	"github.com/Autarquico/codefabric/internal/domain"
)

// checkStorage states the invariants of the storage: the segments tile
// [Start, Sealed), the hot chunks are contiguous from Sealed to End and hold
// less than a segment, and none lies outside.
func (f *leaseFixture) checkStorage(run domain.ID, epoch uint64) {
	f.t.Helper()
	o, _ := f.held(run, epoch)
	segs, err := f.db.Reader().QueryContext(f.t.Context(),
		`SELECT first_offset, last_offset, raw_bytes FROM logs_index WHERE run_id = ? AND lease_epoch = ? AND first_offset >= ? ORDER BY seg_no`,
		string(run), epoch, o.Start)
	if err != nil {
		f.t.Fatal(err)
	}
	defer func() { _ = segs.Close() }()
	next := o.Start
	for segs.Next() {
		var first, last, raw uint64
		if err := segs.Scan(&first, &last, &raw); err != nil {
			f.t.Fatal(err)
		}
		if first != next || raw != last-first {
			f.t.Fatalf("segment [%d, %d) after %d", first, last, next)
		}
		next = last
	}
	if err := segs.Err(); err != nil {
		f.t.Fatal(err)
	}
	if next != o.Sealed {
		f.t.Fatalf("segments end at %d, sealed is %d", next, o.Sealed)
	}
	rows, err := f.db.Reader().QueryContext(f.t.Context(),
		`SELECT chunk_offset, length(data) FROM run_output_chunks WHERE run_id = ? AND lease_epoch = ? ORDER BY chunk_offset`, string(run), epoch)
	if err != nil {
		f.t.Fatal(err)
	}
	defer func() { _ = rows.Close() }()
	next, total := o.Sealed, uint64(0)
	for rows.Next() {
		var off, n uint64
		if err := rows.Scan(&off, &n); err != nil {
			f.t.Fatal(err)
		}
		if off != next {
			f.t.Fatalf("chunk at %d, want %d (sealed %d, end %d)", off, next, o.Sealed, o.End)
		}
		next, total = off+n, total+n
	}
	if err := rows.Err(); err != nil {
		f.t.Fatal(err)
	}
	if next != o.End || total != o.End-o.Sealed || total >= SegmentBytes {
		f.t.Fatalf("hot chunks end at %d for %d bytes; hot is [%d, %d)", next, total, o.Sealed, o.End)
	}
}

func randomBytes(rng *rand.Rand, n int) []byte {
	b := make([]byte, n)
	for i := range b {
		b[i] = byte(rng.UintN(256)) //nolint:gosec // below 256
	}
	return b
}

// sendAll appends all in chunks of the wire's size, one transaction each.
func (f *leaseFixture) sendAll(run domain.ID, epoch uint64, all []byte) {
	f.t.Helper()
	for off := 0; off < len(all); off += v1.OutputChunkBytes {
		if _, err := f.append(run, epoch, uint64(off), all[off:min(off+v1.OutputChunkBytes, len(all))]); err != nil { //nolint:gosec // small
			f.t.Fatal(err)
		}
	}
}

// Nothing is dropped: past a mebibyte the output is sealed into segments, whole
// and in order, and reads see the same bytes whether they lie in a segment, in
// the hot tail or across the two.
func TestAppendOutput_SealsSegmentsAndKeepsEverything(t *testing.T) {
	f, run := outputFixture(t)
	rng := rand.New(rand.NewPCG(1, 2)) //nolint:gosec // reproducible test data
	var all []byte
	for len(all) < 3*SegmentBytes+12345 {
		chunk := randomBytes(rng, 1+rng.IntN(v1.OutputChunkBytes))
		if _, err := f.append(run, 1, uint64(len(all)), chunk); err != nil {
			t.Fatal(err)
		}
		all = append(all, chunk...)
		f.checkStorage(run, 1)
	}
	o, data := f.held(run, 1)
	if o.Start != 0 || o.End != uint64(len(all)) || o.Sealed != 3*SegmentBytes || !bytes.Equal(data, all) {
		t.Fatalf("held [%d, %d) sealed %d of %d, %d bytes", o.Start, o.End, o.Sealed, len(all), len(data))
	}
	for _, r := range [][2]uint64{
		{777, 777 + 200000},                        // inside the first segment
		{SegmentBytes - 100, SegmentBytes + 100},   // across two segments
		{3*SegmentBytes - 50, 3*SegmentBytes + 50}, // across the last segment and the hot tail
		{3*SegmentBytes + 10, o.End},               // hot only
	} {
		var part []byte
		if err := f.db.Read(t.Context(), func(q Querier) (err error) {
			part, err = f.db.OutputRange(t.Context(), q, run, 1, r[0], r[1])
			return err
		}); err != nil || !bytes.Equal(part, all[r[0]:r[1]]) {
			t.Fatalf("OutputRange%v: %v", r, err)
		}
	}
	// The segments are in the blob store and recorded in blobs_meta.
	var indexed, recorded int
	if err := f.db.Reader().QueryRowContext(t.Context(),
		`SELECT COUNT(*), (SELECT COUNT(*) FROM blobs_meta WHERE digest IN (SELECT digest FROM logs_index)) FROM logs_index`).Scan(&indexed, &recorded); err != nil || indexed != 3 || recorded != 3 {
		t.Fatalf("indexed %d, recorded %d: %v", indexed, recorded, err)
	}
	// Bytes already sealed are still compared, not just dropped.
	if got, err := f.append(run, 1, 0, all[:100]); err != nil || got.End != o.End {
		t.Fatalf("resending sealed bytes = %+v, %v", got, err)
	}
	if _, err := f.append(run, 1, 0, bytes.Repeat([]byte{'!'}, 100)); !errors.Is(err, ErrOutputConflict) {
		t.Fatalf("resending different sealed bytes: %v", err)
	}
}

// A resend that straddles the boundary of a segment is compared in both: equal
// is a duplicate, different anywhere in it is a conflict.
func TestAppendOutput_OverlapAcrossASegmentBoundary(t *testing.T) {
	f, run := outputFixture(t)
	rng := rand.New(rand.NewPCG(3, 4)) //nolint:gosec // reproducible test data
	all := randomBytes(rng, SegmentBytes+1000)
	f.sendAll(run, 1, all)
	if o, _ := f.held(run, 1); o.Sealed != SegmentBytes {
		t.Fatalf("sealed = %d", o.Sealed)
	}
	if o, err := f.append(run, 1, SegmentBytes-500, all[SegmentBytes-500:]); err != nil || o.End != uint64(len(all)) {
		t.Fatalf("equal overlap = %+v, %v", o, err)
	}
	bad := bytes.Clone(all[SegmentBytes-500 : SegmentBytes+500])
	bad[10] ^= 0xff // in the segment
	if _, err := f.append(run, 1, SegmentBytes-500, bad); !errors.Is(err, ErrOutputConflict) {
		t.Fatalf("different sealed byte: %v", err)
	}
	bad = bytes.Clone(all[SegmentBytes-500 : SegmentBytes+500])
	bad[990] ^= 0xff // in the hot tail
	if _, err := f.append(run, 1, SegmentBytes-500, bad); !errors.Is(err, ErrOutputConflict) {
		t.Fatalf("different hot byte: %v", err)
	}
	// Continuing past the end works across the boundary too.
	if o, err := f.append(run, 1, SegmentBytes-500, append(bytes.Clone(all[SegmentBytes-500:]), "tail"...)); err != nil || o.End != uint64(len(all))+4 {
		t.Fatalf("overlapping append = %+v, %v", o, err)
	}
	f.checkStorage(run, 1)
}

// A rollback after the segment was written leaves the output as it was: the
// bytes are not acknowledged, and sending them again seals them onto the blob
// that is already there.
func TestAppendOutput_ARolledBackSealLosesNothingAndIsRepeatable(t *testing.T) {
	f, run := outputFixture(t)
	rng := rand.New(rand.NewPCG(5, 6)) //nolint:gosec // reproducible test data
	all := randomBytes(rng, SegmentBytes)
	f.sendAll(run, 1, all[:SegmentBytes-1])
	boom := errors.New("boom")
	err := f.db.WithTx(t.Context(), func(tx *Tx) error {
		if _, err := tx.AppendOutput(t.Context(), run, 1, SegmentBytes-1, all[SegmentBytes-1:], f.clk.Now()); err != nil {
			return err
		}
		return boom
	})
	if !errors.Is(err, boom) {
		t.Fatal(err)
	}
	if o, data := f.held(run, 1); o.End != SegmentBytes-1 || o.Sealed != 0 || !bytes.Equal(data, all[:SegmentBytes-1]) {
		t.Fatalf("after the rollback: [%d, %d) sealed %d", o.Start, o.End, o.Sealed)
	}
	if o, err := f.append(run, 1, SegmentBytes-1, all[SegmentBytes-1:]); err != nil || o.Sealed != SegmentBytes {
		t.Fatalf("append again = %+v, %v", o, err)
	}
	if _, data := f.held(run, 1); !bytes.Equal(data, all) {
		t.Fatal("the bytes are not the bytes sent")
	}
}

// A crash leaves hot bytes of a whole segment (a ring from before segments, or
// a seal that failed) and perhaps the blob already written, with no index row.
// SealOutputs, which runs when the store opens, seals them onto that blob.
func TestSealOutputs_RecoversHotBytesWhoseBlobExistsWithoutAnIndexRow(t *testing.T) {
	f, run := outputFixture(t)
	rng := rand.New(rand.NewPCG(7, 8)) //nolint:gosec // reproducible test data
	all := randomBytes(rng, SegmentBytes+10)
	f.tx(func(tx *Tx) error {
		ctx := t.Context()
		if _, err := tx.Exec(ctx, `INSERT INTO run_output (run_id, lease_epoch, start_offset, end_offset, sealed_offset, updated_at) VALUES (?, 1, 0, ?, 0, 0)`,
			string(run), len(all)); err != nil {
			return err
		}
		for off := 0; off < len(all); off += v1.OutputChunkBytes {
			if _, err := tx.Exec(ctx, `INSERT INTO run_output_chunks (run_id, lease_epoch, chunk_offset, data) VALUES (?, 1, ?, ?)`,
				string(run), off, all[off:min(off+v1.OutputChunkBytes, len(all))]); err != nil {
				return err
			}
		}
		return nil
	})
	orphan, _, err := f.putSegment(t, all[:SegmentBytes]) // written, never indexed
	if err != nil {
		t.Fatal(err)
	}
	if err := f.db.SealOutputs(t.Context()); err != nil {
		t.Fatal(err)
	}
	var (
		digest string
		sealed uint64
	)
	if err := f.db.Reader().QueryRowContext(t.Context(), `SELECT digest, (SELECT sealed_offset FROM run_output) FROM logs_index`).Scan(&digest, &sealed); err != nil {
		t.Fatal(err)
	}
	if digest != string(orphan) || sealed != SegmentBytes {
		t.Fatalf("segment %s (orphan %s), sealed %d", digest, orphan, sealed)
	}
	if o, data := f.held(run, 1); o.Sealed != SegmentBytes || !bytes.Equal(data, all) {
		t.Fatalf("held [%d, %d) sealed %d", o.Start, o.End, o.Sealed)
	}
	f.checkStorage(run, 1)
	if err := f.db.SealOutputs(t.Context()); err != nil { // nothing left to do
		t.Fatal(err)
	}
}

// Closing seals the remainder: a closed output is wholly in segments.
func TestCloseOutput_SealsTheRemainder(t *testing.T) {
	f, run := outputFixture(t)
	if _, err := f.append(run, 1, 0, []byte("short output")); err != nil {
		t.Fatal(err)
	}
	if o, err := f.close(run, 1, 12, ""); err != nil || o.Sealed != 12 {
		t.Fatalf("close = %+v, %v", o, err)
	}
	var hot int
	if err := f.db.Reader().QueryRowContext(t.Context(), `SELECT COUNT(*) FROM run_output_chunks`).Scan(&hot); err != nil || hot != 0 {
		t.Fatalf("%d hot chunks after the close: %v", hot, err)
	}
	if o, data := f.held(run, 1); string(data) != "short output" || !o.Closed {
		t.Fatalf("held %q, %+v", data, o)
	}
}

// A hole the Worker skipped seals what was held: it is not lost, though it is
// no longer part of what is read.
func TestAppendOutput_AHoleSealsWhatWasHeld(t *testing.T) {
	f, run := outputFixture(t)
	if _, err := f.append(run, 1, 0, []byte("abc")); err != nil {
		t.Fatal(err)
	}
	if _, err := f.append(run, 1, 100, []byte("xyz")); err != nil {
		t.Fatal(err)
	}
	var segs int
	if err := f.db.Reader().QueryRowContext(t.Context(), `SELECT COUNT(*) FROM logs_index WHERE last_offset = 3`).Scan(&segs); err != nil || segs != 1 {
		t.Fatalf("segments for the skipped bytes = %d: %v", segs, err)
	}
	if o, data := f.held(run, 1); o.Start != 100 || string(data) != "xyz" {
		t.Fatalf("held %q from %d", data, o.Start)
	}
	f.checkStorage(run, 1)
}

func (f *leaseFixture) putSegment(t *testing.T, raw []byte) (blob.Digest, int64, error) {
	t.Helper()
	blobs, err := f.db.segments()
	if err != nil {
		t.Fatal(err)
	}
	return blobs.PutGzip(t.Context(), raw)
}
