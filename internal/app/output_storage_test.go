package app_test

import (
	"bytes"
	"math/rand/v2"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	v1 "github.com/Autarquico/codefabric/internal/api/v1"
	"github.com/Autarquico/codefabric/internal/app/runs"
	"github.com/Autarquico/codefabric/internal/controller/store"
	"github.com/Autarquico/codefabric/internal/domain"
)

// breakBlobWrites makes the blob store unable to write until the returned
// function is called, which stalls the sealer without touching the code.
func (e *env) breakBlobWrites() (restore func()) {
	e.t.Helper()
	if os.Geteuid() == 0 {
		e.t.Skip("permissions do not stop root")
	}
	tmp := filepath.Join(e.blobRoot, "tmp")
	must(e.t, os.Chmod(tmp, 0o500))               //nolint:gosec // a directory the test makes unwritable
	restore = func() { _ = os.Chmod(tmp, 0o700) } //nolint:gosec // so that TempDir can be removed
	e.t.Cleanup(restore)
	return restore
}

// readAll pages through the output the Controller holds and returns it.
func (e *env) readAll(o outputRun) (start uint64, data []byte) {
	e.t.Helper()
	first := e.readOutput(runs.OutputRead{Ref: o.run.ID, HasOffset: true, Offset: 0, Limit: v1.OutputPageBytes})
	start, data = first.Start, first.Data
	for end := first.End; first.Offset+uint64(len(data)) < end; {
		next := e.readOutput(runs.OutputRead{Ref: o.run.ID, HasOffset: true, Offset: first.Offset + uint64(len(data)), Limit: v1.OutputPageBytes})
		data = append(data, next.Data...)
	}
	return start, data
}

// With the hot tail full, a refused chunk must not turn the next chunk of the
// same batch into a "hole" that drops the bytes already accepted: both are
// answered as retryable, nothing is lost, and sending them again completes the
// output.
func TestOutput_ABusyChunkDoesNotLetTheNextOneDropWhatWasAccepted(t *testing.T) {
	testBusyBatch(t, 3)
}

// The same with a batch that does not fit in one transaction (outputTxBytes):
// the refusal holds across the transactions of the batch.
func TestOutput_ABusyBatchLargerThanATransactionLosesNothing(t *testing.T) {
	testBusyBatch(t, 8)
}

func testBusyBatch(t *testing.T, refused int) {
	e := newEnv(t)
	o := e.outputRun("alpha")
	restore := e.breakBlobWrites()

	rng := rand.New(rand.NewPCG(1, 2)) //nolint:gosec // reproducible test data
	source := make([]byte, store.MaxUnsealedBytes+(refused-1)*v1.OutputChunkBytes+1234)
	for i := range source {
		source[i] = byte(rng.Uint32()) //nolint:gosec // truncation is the point: a random byte
	}
	chunk := func(i int) v1.OutputEntry {
		off := i * v1.OutputChunkBytes
		return v1.OutputEntry{RunID: o.run.ID, LeaseEpoch: o.lease.Epoch, Offset: uint64(off), Data: source[off:min(off+v1.OutputChunkBytes, len(source))]} //nolint:gosec // small
	}
	full := store.MaxUnsealedBytes / v1.OutputChunkBytes
	for i := 0; i < full; i += 16 {
		var batch []v1.OutputEntry
		for j := i; j < min(i+16, full); j++ {
			batch = append(batch, chunk(j))
		}
		for _, ack := range e.send(o.worker, batch...) {
			if ack.Code != "" {
				t.Fatalf("filling the hot tail: %+v", ack)
			}
		}
	}

	tail := func() (batch []v1.OutputEntry) {
		for i := full; i < full+refused; i++ {
			batch = append(batch, chunk(i))
		}
		return batch
	}
	acks := e.send(o.worker, tail()...)
	for i, ack := range acks {
		if ack.Code != domain.CodeInProgress {
			t.Fatalf("entry %d = %+v, want CF_IN_PROGRESS", i, ack)
		}
	}
	if start, data := e.readAll(o); start != 0 || !bytes.Equal(data, source[:store.MaxUnsealedBytes]) {
		t.Fatalf("after the refused batch the output holds %d bytes from %d, want the %d accepted from 0", len(data), start, store.MaxUnsealedBytes)
	}

	restore()
	waitUntil(t, "the resend to be accepted", func() bool {
		e.clock.Advance(time.Minute) // fires the sealer's retry
		for _, ack := range e.send(o.worker, tail()...) {
			if ack.Code != "" {
				return false
			}
		}
		return true
	})
	if start, data := e.readAll(o); start != 0 || !bytes.Equal(data, source) {
		t.Fatalf("the final output holds %d bytes from %d, want the %d sent from 0", len(data), start, len(source))
	}
}

// What the API says when a sealed segment is gone: a CF_INTERNAL error that
// names nothing of the storage.
func TestOutput_AReadWithAMissingSegmentIsAnInternalErrorWithoutDetails(t *testing.T) {
	e := newEnv(t)
	o := e.outputRun("alpha")
	e.send(o.worker, entry(o, 0, strings.Repeat("z", v1.OutputChunkBytes)))
	for off := uint64(v1.OutputChunkBytes); off < 1<<20+v1.OutputChunkBytes; off += v1.OutputChunkBytes {
		e.send(o.worker, entry(o, off, strings.Repeat("z", v1.OutputChunkBytes)))
	}
	waitUntil(t, "the segment", func() bool { return e.sealedOffset(domain.ID(o.run.ID), o.lease.Epoch) >= 1<<20 })
	must(t, os.RemoveAll(filepath.Join(e.blobRoot, "blobs")))

	_, err := e.app.Runs.ReadOutput(t.Context(), e.owner, runs.OutputRead{Ref: o.run.ID, HasOffset: true, Offset: 0, Limit: 10})
	wantCode(t, err, domain.CodeInternal)
	if strings.Contains(err.Error(), "sha256") || strings.Contains(err.Error(), e.blobRoot) {
		t.Fatalf("the error shows the storage: %v", err)
	}
}

// A jump that would pass the cap closes the output as it stood: the bytes it
// had, its offsets and its final offset are kept, not dropped by the gap.
func TestOutput_AJumpOverTheCapClosesTheOutputIntact(t *testing.T) {
	e := newEnvStore(t, nil, store.Options{OutputMaxBytes: 100})
	o := e.outputRun("alpha")
	kept := strings.Repeat("k", 60)
	e.send(o.worker, entry(o, 0, kept))

	acks := e.send(o.worker, entry(o, 1000, strings.Repeat("j", 41)))
	if acks[0].Code != domain.CodeInvalidTransition {
		t.Fatalf("ack = %+v, want the output closed", acks[0])
	}
	got := e.readOutput(runs.OutputRead{Ref: o.run.ID, HasOffset: true, Offset: 0, Limit: v1.OutputPageBytes})
	if string(got.Data) != kept || got.Start != 0 || got.End != 60 || !got.Closed {
		t.Fatalf("read = start %d end %d closed %v with %d bytes", got.Start, got.End, got.Closed, len(got.Data))
	}
}

// A partial page of a segment whose blob is damaged is an internal error with a
// fixed message, never the bytes in front of the damage.
func TestOutput_APartialReadOfADamagedSegmentIsAnInternalError(t *testing.T) {
	damage := map[string]func(b []byte) []byte{
		"truncated gzip": func(b []byte) []byte { return b[:len(b)/2] },
		"bad checksum":   func(b []byte) []byte { b[len(b)-8] ^= 0xff; return b },
	}
	for name, d := range damage {
		t.Run(name, func(t *testing.T) {
			e := newEnv(t)
			o := e.outputRun("alpha")
			rng := rand.New(rand.NewPCG(5, 6)) //nolint:gosec // reproducible test data
			for off := 0; off < 1<<20+v1.OutputChunkBytes; off += v1.OutputChunkBytes {
				chunk := make([]byte, v1.OutputChunkBytes)
				for i := range chunk {
					chunk[i] = byte(rng.Uint32()) //nolint:gosec // truncation is the point: a random byte
				}
				e.send(o.worker, v1.OutputEntry{RunID: o.run.ID, LeaseEpoch: o.lease.Epoch, Offset: uint64(off), Data: chunk}) //nolint:gosec // small
			}
			waitUntil(t, "the segment", func() bool { return e.sealedOffset(domain.ID(o.run.ID), o.lease.Epoch) >= 1<<20 })

			var digest string
			must(t, e.db.Reader().QueryRowContext(t.Context(), `SELECT digest FROM logs_index`).Scan(&digest))
			hex := digest[strings.LastIndex(digest, ":")+1:]
			path := filepath.Join(e.blobRoot, "blobs", "sha256", hex[0:2], hex[2:4], hex)
			raw, err := os.ReadFile(path) //nolint:gosec // a path of the test's own TempDir
			must(t, err)
			must(t, os.WriteFile(path, d(raw), 0o600)) //nolint:gosec // a path of the test's own TempDir

			_, err = e.app.Runs.ReadOutput(t.Context(), e.owner, runs.OutputRead{Ref: o.run.ID, HasOffset: true, Offset: 0, Limit: 100})
			wantCode(t, err, domain.CodeInternal)
			if strings.Contains(err.Error(), "sha256") || strings.Contains(err.Error(), e.blobRoot) || strings.Contains(err.Error(), "gzip") {
				t.Fatalf("the error shows the storage: %v", err)
			}
		})
	}
}
