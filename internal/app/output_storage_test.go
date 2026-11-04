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
	e := newEnv(t)
	o := e.outputRun("alpha")
	restore := e.breakBlobWrites()

	rng := rand.New(rand.NewPCG(1, 2)) //nolint:gosec // reproducible test data
	source := make([]byte, store.MaxUnsealedBytes+2*v1.OutputChunkBytes+1234)
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

	acks := e.send(o.worker, chunk(full), chunk(full+1), chunk(full+2))
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
		for _, ack := range e.send(o.worker, chunk(full), chunk(full+1), chunk(full+2)) {
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
