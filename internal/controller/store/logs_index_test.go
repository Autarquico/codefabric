package store

import (
	"bytes"
	"context"
	"errors"
	"math/rand/v2"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	v1 "github.com/Autarquico/codefabric/internal/api/v1"
	"github.com/Autarquico/codefabric/internal/controller/blob"
	"github.com/Autarquico/codefabric/internal/domain"
	"github.com/Autarquico/codefabric/internal/support/clock"
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

// sendAllNoSeal is sendAll with the sealer idle.
func (f *leaseFixture) sendAllNoSeal(run domain.ID, epoch uint64, all []byte) {
	f.t.Helper()
	for off := 0; off < len(all); off += v1.OutputChunkBytes {
		if _, err := f.appendNoSeal(run, epoch, uint64(off), all[off:min(off+v1.OutputChunkBytes, len(all))]); err != nil { //nolint:gosec // small
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
	// The segments are in the blob store, and only logs_index counts them.
	var indexed, recorded int
	if err := f.db.Reader().QueryRowContext(t.Context(),
		`SELECT COUNT(*), (SELECT COUNT(*) FROM blobs_meta) FROM logs_index`).Scan(&indexed, &recorded); err != nil || indexed != 3 || recorded != 0 {
		t.Fatalf("indexed %d, in blobs_meta %d: %v", indexed, recorded, err)
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

// A gap that lands while a segment is being written moves the output past the
// range: the segment is not indexed (it is an orphan), nothing is retired, and
// the output is what the gap left.
func TestSealer_AGapDuringTheSealLeavesAnOrphanAndIndexesNothing(t *testing.T) {
	f, run := outputFixture(t)
	rng := rand.New(rand.NewPCG(5, 6)) //nolint:gosec // reproducible test data
	all := randomBytes(rng, SegmentBytes+100)
	for off := 0; off < len(all); off += v1.OutputChunkBytes {
		if _, err := f.appendNoSeal(run, 1, uint64(off), all[off:min(off+v1.OutputChunkBytes, len(all))]); err != nil { //nolint:gosec // small
			t.Fatal(err)
		}
	}
	p, err := f.db.sealer.prepare(t.Context(), outputKey{string(run), 1})
	if err != nil || p == nil {
		t.Fatalf("prepare = %v, %v", p, err)
	}
	if _, err := f.appendNoSeal(run, 1, 5*SegmentBytes, []byte("after the gap")); err != nil {
		t.Fatal(err)
	}
	if installed, err := f.db.sealer.install(t.Context(), p); err != nil || installed {
		t.Fatalf("install = %v, %v", installed, err)
	}
	var indexed int
	if err := f.db.Reader().QueryRowContext(t.Context(), `SELECT COUNT(*) FROM logs_index`).Scan(&indexed); err != nil || indexed != 0 {
		t.Fatalf("%d segments indexed: %v", indexed, err)
	}
	if o, data := f.held(run, 1); o.Start != 5*SegmentBytes || string(data) != "after the gap" {
		t.Fatalf("held %q from %d", data, o.Start)
	}
	f.checkStorage(run, 1)
}

// A segment that is installed is the bytes that were hot, and a sealer that
// finds the range changed at its end (an append after the read) still installs it.
func TestSealer_AnAppendDuringTheSealDoesNotStopIt(t *testing.T) {
	f, run := outputFixture(t)
	rng := rand.New(rand.NewPCG(9, 10)) //nolint:gosec // reproducible test data
	all := randomBytes(rng, SegmentBytes+v1.OutputChunkBytes)
	for off := 0; off < SegmentBytes+100; off += v1.OutputChunkBytes {
		if _, err := f.appendNoSeal(run, 1, uint64(off), all[off:min(off+v1.OutputChunkBytes, SegmentBytes+100)]); err != nil { //nolint:gosec // small
			t.Fatal(err)
		}
	}
	p, err := f.db.sealer.prepare(t.Context(), outputKey{string(run), 1})
	if err != nil || p == nil {
		t.Fatalf("prepare = %v, %v", p, err)
	}
	if _, err := f.appendNoSeal(run, 1, SegmentBytes+100, all[SegmentBytes+100:]); err != nil {
		t.Fatal(err)
	}
	if installed, err := f.db.sealer.install(t.Context(), p); err != nil || !installed {
		t.Fatalf("install = %v, %v", installed, err)
	}
	f.seal()
	if _, data := f.held(run, 1); !bytes.Equal(data, all) {
		t.Fatal("the bytes are not the bytes sent")
	}
	f.checkStorage(run, 1)
}

// The hot tail has a bound: when the sealer is behind, new bytes are refused
// with ErrOutputBusy and stored nowhere; sending them again after it caught up works.
func TestAppendOutput_WhenTheSealerIsBehindTheOutputIsBusy(t *testing.T) {
	f, run := outputFixture(t)
	chunk := bytes.Repeat([]byte("b"), v1.OutputChunkBytes)
	var off uint64
	for ; off < MaxUnsealedBytes; off += uint64(len(chunk)) {
		if _, err := f.appendNoSeal(run, 1, off, chunk); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := f.appendNoSeal(run, 1, off, chunk); !errors.Is(err, ErrOutputBusy) {
		t.Fatalf("append over the bound: %v", err)
	}
	if o, _ := f.held(run, 1); o.End != off {
		t.Fatalf("end = %d after a refused append, want %d", o.End, off)
	}
	if o, err := f.appendNoSeal(run, 1, off-uint64(len(chunk)), chunk); err != nil || o.End != off {
		t.Fatalf("a resend of what is held must still be acknowledged: %+v, %v", o, err)
	}
	f.seal()
	if o, err := f.append(run, 1, off, chunk); err != nil || o.End != off+uint64(len(chunk)) {
		t.Fatalf("append after the seal = %+v, %v", o, err)
	}
	f.checkStorage(run, 1)
}

// Without a blob store nothing is sealed: the hot tail keeps everything, there
// is no bound but the cap, and what is held reads back.
func TestAppendOutput_WithoutABlobStoreTheHotTailKeepsEverything(t *testing.T) {
	f, run := outputFixtureWith(t, newWorkerFixtureWith(t, Options{}))
	rng := rand.New(rand.NewPCG(11, 12)) //nolint:gosec // reproducible test data
	all := randomBytes(rng, MaxUnsealedBytes+3*v1.OutputChunkBytes)
	for off := 0; off < len(all); off += v1.OutputChunkBytes {
		if _, err := f.append(run, 1, uint64(off), all[off:min(off+v1.OutputChunkBytes, len(all))]); err != nil { //nolint:gosec // small
			t.Fatal(err)
		}
	}
	if o, data := f.held(run, 1); o.Sealed != 0 || !bytes.Equal(data, all) {
		t.Fatalf("sealed %d, %d bytes held of %d", o.Sealed, len(data), len(all))
	}
	if _, err := f.close(run, 1, uint64(len(all)), ""); err != nil {
		t.Fatal(err)
	}
	var indexed int
	if err := f.db.Reader().QueryRowContext(t.Context(), `SELECT COUNT(*) FROM logs_index`).Scan(&indexed); err != nil || indexed != 0 {
		t.Fatalf("%d segments without a blob store: %v", indexed, err)
	}
}

// The output of a (Run, epoch) is capped. The append that passes the cap closes
// the output where it stands, with a note; what came before is kept, and later
// entries find the output closed, which the Worker takes as final.
func TestAppendOutput_TheCapClosesTheOutput(t *testing.T) {
	f, run := outputFixtureWith(t, newWorkerFixtureWith(t, Options{Blobs: newBlobs(t), manualSeal: true, OutputMaxBytes: 100}))
	if _, err := f.append(run, 1, 0, bytes.Repeat([]byte("a"), 60)); err != nil {
		t.Fatal(err)
	}
	// The error rolls nothing back here: the use case acknowledges it and commits.
	err := f.db.WithTx(t.Context(), func(tx *Tx) error {
		_, err := tx.AppendOutput(t.Context(), run, 1, 60, bytes.Repeat([]byte("b"), 41), f.clk.Now())
		if !errors.Is(err, ErrOutputClosed) {
			t.Fatalf("append past the cap: %v", err)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	o, data := f.held(run, 1)
	if !o.Closed || o.FinalOffset != 60 || o.Note != OutputLimitNote || o.End != 60 || len(data) != 60 {
		t.Fatalf("held %+v, %d bytes", o, len(data))
	}
	if _, err := f.append(run, 1, 60, []byte("c")); !errors.Is(err, ErrOutputClosed) {
		t.Fatalf("append to the closed output: %v", err)
	}
	f.seal()
	if o, data := f.held(run, 1); o.Sealed != 60 || len(data) != 60 {
		t.Fatalf("after the seal: %+v, %d bytes", o, len(data))
	}
	// Exactly the cap fits; another epoch has its own.
	if o, err := f.append(run, 2, 0, bytes.Repeat([]byte("d"), 100)); err != nil || o.End != 100 || o.Closed {
		t.Fatalf("an output of exactly the cap = %+v, %v", o, err)
	}
}

// A Worker that skips ahead over and over does not get more room, and does not
// make a segment, a blob or a row of each gap.
func TestAppendOutput_GapsDoNotAmplifyWrites(t *testing.T) {
	f, run := outputFixtureWith(t, newWorkerFixtureWith(t, Options{Blobs: newBlobs(t), manualSeal: true, OutputMaxBytes: 100}))
	var off uint64
	var last error
	for i := 0; i < 200 && last == nil; i++ {
		_, last = f.append(run, 1, off, []byte("x"))
		off += 10 // one byte, then a hole
	}
	if !errors.Is(last, ErrOutputClosed) {
		t.Fatalf("the cap never closed the output: %v", last)
	}
	var indexed, blobs, chunks int
	if err := f.db.Reader().QueryRowContext(t.Context(),
		`SELECT (SELECT COUNT(*) FROM logs_index), (SELECT COUNT(*) FROM blobs_meta), (SELECT COUNT(*) FROM run_output_chunks)`).Scan(&indexed, &blobs, &chunks); err != nil {
		t.Fatal(err)
	}
	if indexed != 0 || blobs != 0 || chunks > 1 {
		t.Fatalf("%d segments, %d blobs, %d chunks after a burst of gaps", indexed, blobs, chunks)
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

// Closing leaves the remainder to the sealer: a closed output ends up wholly in segments.
func TestCloseOutput_SealsTheRemainder(t *testing.T) {
	f, run := outputFixture(t)
	if _, err := f.append(run, 1, 0, []byte("short output")); err != nil {
		t.Fatal(err)
	}
	if o, err := f.close(run, 1, 12, ""); err != nil || !o.Closed {
		t.Fatalf("close = %+v, %v", o, err)
	}
	var hot int
	if err := f.db.Reader().QueryRowContext(t.Context(), `SELECT COUNT(*) FROM run_output_chunks`).Scan(&hot); err != nil || hot != 0 {
		t.Fatalf("%d hot chunks after the close: %v", hot, err)
	}
	if o, data := f.held(run, 1); string(data) != "short output" || !o.Closed || o.Sealed != 12 {
		t.Fatalf("held %q, %+v", data, o)
	}
}

// A hole the Worker skipped drops what was held and not sealed: no segment is
// written for it.
func TestAppendOutput_AHoleDropsWhatWasHeld(t *testing.T) {
	f, run := outputFixture(t)
	if _, err := f.append(run, 1, 0, []byte("abc")); err != nil {
		t.Fatal(err)
	}
	if _, err := f.append(run, 1, 100, []byte("xyz")); err != nil {
		t.Fatal(err)
	}
	var segs int
	if err := f.db.Reader().QueryRowContext(t.Context(), `SELECT COUNT(*) FROM logs_index`).Scan(&segs); err != nil || segs != 0 {
		t.Fatalf("segments for the skipped bytes = %d: %v", segs, err)
	}
	if o, data := f.held(run, 1); o.Start != 100 || string(data) != "xyz" || o.Accepted != 6 {
		t.Fatalf("held %q from %d, accepted %d", data, o.Start, o.Accepted)
	}
	f.checkStorage(run, 1)
}

func (f *leaseFixture) putSegment(t *testing.T, raw []byte) (blob.Digest, int64, error) {
	t.Helper()
	return f.db.blobs.PutGzip(t.Context(), raw)
}

func (f *leaseFixture) fill(run domain.ID, n int) {
	f.t.Helper()
	chunk := bytes.Repeat([]byte("x"), v1.OutputChunkBytes)
	for off := 0; off < n; off += v1.OutputChunkBytes {
		if _, err := f.appendNoSeal(run, 1, uint64(off), chunk[:min(v1.OutputChunkBytes, n-off)]); err != nil { //nolint:gosec // small
			f.t.Fatal(err)
		}
	}
}

func (f *leaseFixture) sealedOffset(run domain.ID) uint64 {
	f.t.Helper()
	o, _ := f.held(run, 1)
	return o.Sealed
}

// One busy output cannot hold the sealer: a pass seals at most one segment of
// each due output, so two busy outputs advance in step and neither starves.
func TestSealer_APassSealsOneSegmentPerOutput(t *testing.T) {
	f, a := outputFixture(t)
	b := f.queuedRun(2).ID
	f.fill(a, 4*SegmentBytes)
	f.fill(b, 4*SegmentBytes)

	for pass := uint64(1); pass <= 4; pass++ {
		progressed, err := f.db.sealer.pass1(t.Context())
		if err != nil || !progressed {
			t.Fatalf("pass %d = %v, %v", pass, progressed, err)
		}
		if sa, sb := f.sealedOffset(a), f.sealedOffset(b); sa != pass*SegmentBytes || sb != pass*SegmentBytes {
			t.Fatalf("after pass %d sealed %d and %d, want %d each", pass, sa, sb, pass*SegmentBytes)
		}
	}
	if progressed, err := f.db.sealer.pass1(t.Context()); err != nil || progressed {
		t.Fatalf("a pass with nothing left = %v, %v", progressed, err)
	}
}

// The production path: the goroutine seals after the commit, and Close stops it
// and returns.
func TestSealer_BackgroundSealsAndCloseReturns(t *testing.T) {
	blobs := newBlobs(t)
	w := newWorkerFixtureWith(t, Options{Blobs: blobs})
	f := newLeaseFixtureWith(t, w)
	run := f.queuedRun(1).ID
	f.fill(run, SegmentBytes+10)

	deadline := time.Now().Add(10 * time.Second)
	for f.sealedOffset(run) != SegmentBytes {
		if time.Now().After(deadline) {
			t.Fatal("the background sealer did not seal")
		}
		time.Sleep(5 * time.Millisecond)
	}
	if err := f.db.Close(); err != nil {
		t.Fatal(err)
	}
}

// A seal that fails for a while (the blob store cannot write) is tried again
// by itself, with backoff on the clock of the DB, even if nothing else is ever
// appended: the bytes do not wait in SQLite for the next activity.
func TestSealer_ATransientFailureIsRetriedWithoutNewAppends(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("permissions do not stop root")
	}
	root := t.TempDir()
	blobs, err := blob.New(root, clock.NewFake(epoch0))
	if err != nil {
		t.Fatal(err)
	}
	tmp := filepath.Join(root, "tmp")
	if err := os.Chmod(tmp, 0o500); err != nil { //nolint:gosec // a directory the test makes unwritable
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(tmp, 0o700) }) //nolint:gosec // so that TempDir can be removed
	f := newLeaseFixtureWith(t, newWorkerFixtureWith(t, Options{Blobs: blobs}))
	run := f.queuedRun(1).ID
	f.fill(run, SegmentBytes+10)

	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	if err := f.clk.BlockUntil(ctx, 1); err != nil {
		t.Fatalf("no retry was scheduled: %v", err)
	}
	if got := f.sealedOffset(run); got != 0 {
		t.Fatalf("sealed %d with the blob store failing", got)
	}
	if err := os.Chmod(tmp, 0o700); err != nil { //nolint:gosec // the store recovers
		t.Fatal(err)
	}
	for f.sealedOffset(run) != SegmentBytes {
		if ctx.Err() != nil {
			t.Fatal("the segment was not sealed after the store recovered")
		}
		f.clk.Advance(sealRetryMax)
		time.Sleep(5 * time.Millisecond)
	}
	f.checkStorage(run, 1)
}

// A hole is not taken while the hot tail is over its bound: dropping it would
// lose bytes that were accepted and are only waiting to be sealed.
func TestAppendOutput_AHoleIsRefusedWhileTheOutputIsBusy(t *testing.T) {
	f, run := outputFixture(t)
	f.fill(run, MaxUnsealedBytes)
	if _, err := f.appendNoSeal(run, 1, MaxUnsealedBytes+100, []byte("beyond")); !errors.Is(err, ErrOutputBusy) {
		t.Fatalf("a hole over the bound: %v", err)
	}
	if o, _ := f.held(run, 1); o.Start != 0 || o.End != MaxUnsealedBytes {
		t.Fatalf("held [%d, %d) after a refused hole", o.Start, o.End)
	}
	f.seal()
	f.checkStorage(run, 1)
}

// A Controller that stops with unsealed output (the hot tail of a ring written
// before segments, or a seal that never ran) seals it by itself when it starts
// again, and the output reads back as it was sent.
func TestOpen_AfterARestartTheUnsealedOutputIsSealedAndReadsBack(t *testing.T) {
	blobs := newBlobs(t)
	path := filepath.Join(t.TempDir(), "controller.db")
	clk := clock.NewFake(epoch0)
	first := reopen(t, path, Options{Clock: clk, Blobs: blobs, manualSeal: true})
	f := newLeaseFixtureWith(t, &workerFixture{t: t, db: first, clk: clk})
	run := f.queuedRun(1).ID
	rng := rand.New(rand.NewPCG(21, 22)) //nolint:gosec // reproducible test data
	all := randomBytes(rng, 3*SegmentBytes+1000)
	f.sendAllNoSeal(run, 1, all)
	if got := f.sealedOffset(run); got != 0 {
		t.Fatalf("sealed %d before the restart", got)
	}
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}

	f.db = reopen(t, path, Options{Clock: clk, Blobs: blobs})
	deadline := time.Now().Add(10 * time.Second)
	for f.sealedOffset(run) != 3*SegmentBytes {
		if time.Now().After(deadline) {
			t.Fatalf("sealed %d after the restart", f.sealedOffset(run))
		}
		time.Sleep(5 * time.Millisecond)
	}
	if _, data := f.held(run, 1); !bytes.Equal(data, all) {
		t.Fatal("the output after the restart is not the bytes sent")
	}
	f.checkStorage(run, 1)
}

// What a read says when a sealed segment cannot be read back: ErrOutputUnreadable
// whichever way the blob is wrong, and never wrong bytes.
func TestOutputRange_ASegmentThatCannotBeReadIsUnreadable(t *testing.T) {
	corrupt := map[string]func(t *testing.T, path string){
		"missing blob": func(t *testing.T, path string) {
			if err := os.Remove(path); err != nil {
				t.Fatal(err)
			}
		},
		"truncated gzip": func(t *testing.T, path string) {
			st, err := os.Stat(path)
			if err != nil {
				t.Fatal(err)
			}
			if err := os.Truncate(path, st.Size()/2); err != nil {
				t.Fatal(err)
			}
		},
		"bad checksum": func(t *testing.T, path string) {
			b, err := os.ReadFile(path) //nolint:gosec // a path of the test's own TempDir
			if err != nil {
				t.Fatal(err)
			}
			b[len(b)-8] ^= 0xff                                  // the CRC-32 of the gzip trailer
			if err := os.WriteFile(path, b, 0o600); err != nil { //nolint:gosec // a path of the test's own TempDir
				t.Fatal(err)
			}
		},
	}
	for name, damage := range corrupt {
		t.Run(name, func(t *testing.T) {
			root := t.TempDir()
			blobs, err := blob.New(root, clock.NewFake(epoch0))
			if err != nil {
				t.Fatal(err)
			}
			f, run := outputFixtureWith(t, newWorkerFixtureWith(t, Options{Blobs: blobs, manualSeal: true}))
			rng := rand.New(rand.NewPCG(31, 32)) //nolint:gosec // reproducible test data
			f.sendAll(run, 1, randomBytes(rng, SegmentBytes+10))
			var digest string
			if err := f.db.Reader().QueryRowContext(t.Context(), `SELECT digest FROM logs_index`).Scan(&digest); err != nil {
				t.Fatal(err)
			}
			hex := digest[strings.LastIndex(digest, ":")+1:]
			damage(t, filepath.Join(root, "blobs", "sha256", hex[0:2], hex[2:4], hex))

			// Whole, and a page that stops well before the end: neither serves a byte.
			for _, to := range []uint64{SegmentBytes, 100} {
				err = f.db.Read(t.Context(), func(q Querier) error {
					_, err := f.db.OutputRange(t.Context(), q, run, 1, 0, to)
					return err
				})
				if !errors.Is(err, ErrOutputUnreadable) {
					t.Fatalf("read of [0, %d) of a damaged segment = %v, want ErrOutputUnreadable", to, err)
				}
			}
		})
	}
}
