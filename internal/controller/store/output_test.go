package store

import (
	"bytes"
	"errors"
	"math/rand/v2"
	"testing"
	"time"

	v1 "github.com/Autarquico/codefabric/internal/api/v1"
	"github.com/Autarquico/codefabric/internal/domain"
)

func outputFixture(t *testing.T) (*leaseFixture, domain.ID) {
	t.Helper()
	f := newLeaseFixture(t)
	return f, f.queuedRun(1).ID
}

func (f *leaseFixture) append(run domain.ID, epoch, offset uint64, data []byte) (Output, error) {
	f.t.Helper()
	var o Output
	err := f.db.WithTx(f.t.Context(), func(tx *Tx) (err error) {
		o, err = tx.AppendOutput(f.t.Context(), run, epoch, offset, data, f.clk.Now())
		return err
	})
	return o, err
}

func (f *leaseFixture) close(run domain.ID, epoch, final uint64, note string) (Output, error) {
	f.t.Helper()
	var o Output
	err := f.db.WithTx(f.t.Context(), func(tx *Tx) (err error) {
		o, err = tx.CloseOutput(f.t.Context(), run, epoch, final, note, f.clk.Now())
		return err
	})
	return o, err
}

// held reads the bytes of an output as a reader would.
func (f *leaseFixture) held(run domain.ID, epoch uint64) (Output, []byte) {
	f.t.Helper()
	var (
		o    Output
		data []byte
	)
	err := f.db.Read(f.t.Context(), func(q Querier) (err error) {
		if o, err = OutputOf(f.t.Context(), q, run, epoch); err != nil {
			return err
		}
		data, err = OutputRange(f.t.Context(), q, run, epoch, o.Start, o.End)
		return err
	})
	if err != nil {
		f.t.Fatal(err)
	}
	return o, data
}

// checkRing states the invariants of the ring: the chunks are contiguous from
// Start to End, none lies outside, and the bytes kept are at most 1 MiB.
func (f *leaseFixture) checkRing(run domain.ID, epoch uint64) {
	f.t.Helper()
	o, _ := f.held(run, epoch)
	if o.End-o.Start > v1.OutputTailBytes {
		f.t.Fatalf("holds %d bytes, over the ring", o.End-o.Start)
	}
	rows, err := f.db.Reader().QueryContext(f.t.Context(),
		`SELECT chunk_offset, length(data) FROM run_output_chunks WHERE run_id = ? AND lease_epoch = ? ORDER BY chunk_offset`, string(run), epoch)
	if err != nil {
		f.t.Fatal(err)
	}
	defer func() { _ = rows.Close() }()
	next, total := o.Start, uint64(0)
	defer func() {
		if err := rows.Err(); err != nil {
			f.t.Fatal(err)
		}
	}()
	for rows.Next() {
		var off, n uint64
		if err := rows.Scan(&off, &n); err != nil {
			f.t.Fatal(err)
		}
		if off != next {
			f.t.Fatalf("chunk at %d, want %d (start %d, end %d)", off, next, o.Start, o.End)
		}
		next, total = off+n, total+n
	}
	if next != o.End || total != o.End-o.Start || total > v1.OutputTailBytes {
		f.t.Fatalf("chunks end at %d for %d bytes; output is [%d, %d)", next, total, o.Start, o.End)
	}
}

func TestAppendOutput_AppendsAndIsIdempotent(t *testing.T) {
	f, run := outputFixture(t)
	o, err := f.append(run, 1, 0, []byte("hello "))
	if err != nil || o.Start != 0 || o.End != 6 {
		t.Fatalf("first append = %+v, %v", o, err)
	}
	if o, err = f.append(run, 1, 6, []byte("world")); err != nil || o.End != 11 {
		t.Fatalf("second append = %+v, %v", o, err)
	}
	updated := o.UpdatedAt
	f.clk.Advance(time.Minute)

	// Sent again, whole or in part: nothing changes, and it is not an append.
	for _, again := range []struct {
		offset uint64
		data   string
	}{{0, "hello "}, {6, "world"}, {3, "lo wor"}, {0, "hello world"}} {
		o, err := f.append(run, 1, again.offset, []byte(again.data))
		if err != nil || o.End != 11 || !o.UpdatedAt.Equal(updated) {
			t.Fatalf("again %+v: %+v, %v", again, o, err)
		}
	}
	// Overlapping the end: only the new part is added.
	if o, err = f.append(run, 1, 8, []byte("rld!!")); err != nil || o.End != 13 {
		t.Fatalf("overlapping append = %+v, %v", o, err)
	}
	if o, data := f.held(run, 1); string(data) != "hello world!!" || o.Start != 0 {
		t.Fatalf("held %q from %d", data, o.Start)
	}
	f.checkRing(run, 1)
}

func TestAppendOutput_ADifferentOverlapIsAConflictAndStoresNothing(t *testing.T) {
	f, run := outputFixture(t)
	if _, err := f.append(run, 1, 0, []byte("abcdef")); err != nil {
		t.Fatal(err)
	}
	if _, err := f.append(run, 1, 2, []byte("XXXXXXXX")); !errors.Is(err, ErrOutputConflict) {
		t.Fatalf("err = %v, want ErrOutputConflict", err)
	}
	if o, data := f.held(run, 1); string(data) != "abcdef" || o.End != 6 {
		t.Fatalf("a refused append changed the output: %q", data)
	}
}

func TestAppendOutput_AHoleRestartsTheOutput(t *testing.T) {
	f, run := outputFixture(t)
	if _, err := f.append(run, 1, 0, []byte("abc")); err != nil {
		t.Fatal(err)
	}
	o, err := f.append(run, 1, 100, []byte("xyz"))
	if err != nil || o.Start != 100 || o.End != 103 {
		t.Fatalf("after a hole = %+v, %v", o, err)
	}
	if _, data := f.held(run, 1); string(data) != "xyz" {
		t.Fatalf("held %q", data)
	}
	f.checkRing(run, 1)

	// The first thing ever received may start anywhere: the Worker skipped what came before.
	if o, err = f.append(run, 2, 5000, []byte("q")); err != nil || o.Start != 5000 || o.End != 5001 {
		t.Fatalf("first append of an epoch = %+v, %v", o, err)
	}
}

func TestAppendOutput_TheRingKeepsAMebibyteAndCutsTheChunkOnTheBoundary(t *testing.T) {
	f, run := outputFixture(t)
	rng := rand.New(rand.NewPCG(1, 2)) //nolint:gosec // reproducible test data
	var all []byte
	for len(all) < 3*v1.OutputTailBytes+12345 {
		n := 1 + rng.IntN(v1.OutputChunkBytes)
		chunk := make([]byte, n)
		for i := range chunk {
			chunk[i] = byte(rng.UintN(256)) //nolint:gosec // below 256
		}
		if _, err := f.append(run, 1, uint64(len(all)), chunk); err != nil {
			t.Fatal(err)
		}
		all = append(all, chunk...)
		f.checkRing(run, 1)
	}
	o, data := f.held(run, 1)
	if o.End != uint64(len(all)) || o.End-o.Start != v1.OutputTailBytes || !bytes.Equal(data, all[o.Start:]) {
		t.Fatalf("held [%d, %d) of %d, %d bytes", o.Start, o.End, len(all), len(data))
	}
	// A read of any part of it is the same bytes.
	var part []byte
	if err := f.db.Read(t.Context(), func(q Querier) (err error) {
		part, err = OutputRange(t.Context(), q, run, 1, o.Start+777, o.Start+777+200000)
		return err
	}); err != nil || !bytes.Equal(part, all[o.Start+777:o.Start+777+200000]) {
		t.Fatalf("OutputRange: %v", err)
	}
	// Bytes below Start are not compared, only dropped.
	if got, err := f.append(run, 1, 0, []byte("old")); err != nil || got.End != o.End {
		t.Fatalf("resending dropped bytes = %+v, %v", got, err)
	}
}

func TestAppendOutput_TheEpochsAreSeparate(t *testing.T) {
	f, run := outputFixture(t)
	for epoch, text := range map[uint64]string{1: "one", 2: "two!"} {
		if _, err := f.append(run, epoch, 0, []byte(text)); err != nil {
			t.Fatal(err)
		}
	}
	if _, data := f.held(run, 1); string(data) != "one" {
		t.Errorf("epoch 1 = %q", data)
	}
	if _, data := f.held(run, 2); string(data) != "two!" {
		t.Errorf("epoch 2 = %q", data)
	}
	var latest uint64
	_ = f.db.Read(t.Context(), func(q Querier) (err error) {
		latest, _, err = LatestOutputEpoch(t.Context(), q, run)
		return err
	})
	if latest != 2 {
		t.Errorf("latest epoch = %d", latest)
	}
}

func TestAppendOutput_RejectsWhatIsOutOfRange(t *testing.T) {
	f, run := outputFixture(t)
	for name, tc := range map[string]struct {
		epoch, offset uint64
		data          []byte
	}{
		"epoch zero":       {0, 0, []byte("x")},
		"no bytes":         {1, 0, nil},
		"chunk too big":    {1, 0, make([]byte, v1.OutputChunkBytes+1)},
		"offset too big":   {1, v1.OutputMaxOffset + 1, []byte("x")},
		"sum overflows":    {1, v1.OutputMaxOffset, []byte("x")},
		"offset max-ish":   {1, 1<<64 - 1, []byte("xx")},
		"epoch is a short": {0, 5, []byte("x")},
	} {
		if _, err := f.append(run, tc.epoch, tc.offset, tc.data); !errors.Is(err, ErrOutputInvalid) {
			t.Errorf("%s: err = %v, want ErrOutputInvalid", name, err)
		}
	}
}

func TestCloseOutput(t *testing.T) {
	f, run := outputFixture(t)
	if _, err := f.append(run, 1, 0, []byte("done")); err != nil {
		t.Fatal(err)
	}
	if _, err := f.close(run, 1, 10, ""); !errors.Is(err, ErrOutputIncomplete) {
		t.Errorf("closing past what is held: %v", err)
	}
	if _, err := f.close(run, 1, 2, ""); !errors.Is(err, ErrOutputConflict) {
		t.Errorf("closing before what is held: %v", err)
	}
	o, err := f.close(run, 1, 4, "output.log was replaced or truncated")
	if err != nil || !o.Closed || o.FinalOffset != 4 || o.Note == "" {
		t.Fatalf("close = %+v, %v", o, err)
	}
	if o, err = f.close(run, 1, 4, ""); err != nil || !o.Closed {
		t.Errorf("closing again at the same place = %+v, %v", o, err)
	}
	if _, err := f.close(run, 1, 5, ""); !errors.Is(err, ErrOutputConflict) {
		t.Errorf("closing again elsewhere: %v", err)
	}
	if _, err := f.append(run, 1, 4, []byte("more")); !errors.Is(err, ErrOutputClosed) {
		t.Errorf("appending after the close: %v", err)
	}
	// An output the store never saw, closed, holds nothing and ends there.
	if o, err = f.close(run, 3, 0, ""); err != nil || !o.Closed || o.Start != 0 || o.End != 0 {
		t.Errorf("closing an unseen output = %+v, %v", o, err)
	}
}

func TestOutputNotifier_RingsOnlyAfterACommittedChange(t *testing.T) {
	f, run := outputFixture(t)
	n := f.db.OutputNotifier()
	before := n.Latest()
	boom := errors.New("boom")
	_ = f.db.WithTx(t.Context(), func(tx *Tx) error {
		if _, err := tx.AppendOutput(t.Context(), run, 1, 0, []byte("x"), f.clk.Now()); err != nil {
			return err
		}
		return boom
	})
	if n.Latest() != before {
		t.Fatal("a rolled back append woke the readers")
	}
	if _, err := f.append(run, 1, 0, []byte("x")); err != nil {
		t.Fatal(err)
	}
	if n.Latest() == before {
		t.Fatal("a committed append did not wake the readers")
	}
	again := n.Latest()
	if _, err := f.append(run, 1, 0, []byte("x")); err != nil || n.Latest() != again {
		t.Fatal("an append sent again woke the readers")
	}
}
