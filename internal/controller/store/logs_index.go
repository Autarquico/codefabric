package store

import (
	"context"
	"fmt"
	"io"
	"path/filepath"

	"github.com/Autarquico/codefabric/internal/controller/blob"
	"github.com/Autarquico/codefabric/internal/domain"
)

// SegmentBytes is the size at which the hot bytes of an output are sealed into
// an immutable gzip segment of the blob store.
//
// An output is therefore: the segments of logs_index, which tile
// [Start, Sealed), and the hot chunks of SQLite, which hold [Sealed, End). A
// hot tail is under SegmentBytes plus what one append adds; nothing is ever
// dropped from it before it is sealed.
//
// Sealing order, in the transaction that appends: the gzip is written to the
// CAS and fsynced (blob.PutGzip), and only then the logs_index row is inserted
// and the sealed chunks retired, so the commit is the point of no return. A
// crash or a rollback before it leaves at most an orphan blob, which is
// content-addressed and idempotent to write again; collecting orphans is the
// job of retention-gc, which finds them by their absence from logs_index.
// Every digest sealed is recorded in blobs_meta, so that a collector can count
// references.
const SegmentBytes = 1 << 20

// sealOutput seals [o.Sealed, upTo) of the output into a segment, and moves
// o.Sealed. The row of run_output is written by the caller with the rest of
// its changes.
func (t *Tx) sealOutput(ctx context.Context, o *Output, upTo uint64) error {
	if upTo <= o.Sealed {
		return nil
	}
	raw, err := hotRange(ctx, t, make([]byte, 0, upTo-o.Sealed), o.RunID, o.Epoch, o.Sealed, upTo)
	if err != nil {
		return err
	}
	blobs, err := t.db.segments()
	if err != nil {
		return err
	}
	digest, stored, err := blobs.PutGzip(ctx, raw)
	if err != nil {
		return fmt.Errorf("store: writing a segment: %w", err)
	}
	if _, err := t.PutBlobMeta(ctx, Blob{Digest: string(digest), Size: stored, CreatedAt: t.db.clock.Now()}); err != nil {
		return err
	}
	if _, err := t.Exec(ctx,
		`INSERT INTO logs_index (run_id, lease_epoch, seg_no, first_offset, last_offset, raw_bytes, digest)
		 VALUES (?, ?, (SELECT COALESCE(MAX(seg_no) + 1, 0) FROM logs_index WHERE run_id = ? AND lease_epoch = ?), ?, ?, ?, ?)`,
		string(o.RunID), o.Epoch, string(o.RunID), o.Epoch, o.Sealed, upTo, len(raw), string(digest)); err != nil {
		return fmt.Errorf("store: indexing a segment: %w", err)
	}
	if err := t.trimOutput(ctx, o.RunID, o.Epoch, upTo); err != nil {
		return err
	}
	o.Sealed = upTo
	return nil
}

// segments is the blob store of the segments. It is opened on first use, so a
// database that never seals (the one `init` creates) leaves nothing on disk.
func (db *DB) segments() (*blob.Store, error) {
	db.blobsOnce.Do(func() {
		if db.blobs == nil {
			db.blobs, db.blobsErr = blob.New(filepath.Dir(db.path), db.clock)
		}
	})
	if db.blobsErr != nil {
		return nil, fmt.Errorf("store: opening the blob store: %w", db.blobsErr)
	}
	return db.blobs, nil
}

// sealFull seals every whole segment the hot tail holds.
func (t *Tx) sealFull(ctx context.Context, o *Output) error {
	for o.End-o.Sealed >= SegmentBytes {
		if err := t.sealOutput(ctx, o, o.Sealed+SegmentBytes); err != nil {
			return err
		}
	}
	return nil
}

// SealOutputs seals the hot tails that hold a whole segment or more, one output
// per transaction. It runs when the store opens: a ring written before
// segments existed holds up to 1 MiB, and a seal that failed leaves its bytes
// hot. A blob written by an earlier attempt that never got its index row is
// found by its digest and not written twice (the same bytes are the same blob);
// one that no attempt ever indexes is an orphan, which retention-gc collects.
func (db *DB) SealOutputs(ctx context.Context) error {
	pending, err := db.unsealed(ctx)
	if err != nil {
		return err
	}
	for _, k := range pending {
		err := db.WithTx(ctx, func(tx *Tx) error {
			o, err := OutputOf(ctx, tx, domain.ID(k.run), k.epoch)
			if err != nil {
				return err
			}
			if err := tx.sealFull(ctx, &o); err != nil {
				return err
			}
			_, err = tx.Exec(ctx, `UPDATE run_output SET sealed_offset = ? WHERE run_id = ? AND lease_epoch = ?`, o.Sealed, k.run, k.epoch)
			return err
		})
		if err != nil {
			return fmt.Errorf("store: sealing the output of %s: %w", k.run, err)
		}
	}
	return nil
}

// OutputRange returns the bytes [from, to) of the output, which must lie inside
// what it holds: the one read view over the segments and the hot tail. The
// memory it takes is the size of the range, whatever the size of the output.
func (db *DB) OutputRange(ctx context.Context, q Querier, run domain.ID, epoch, from, to uint64) ([]byte, error) {
	if to < from {
		return nil, fmt.Errorf("%w: [%d, %d)", ErrOutputInvalid, from, to)
	}
	out := make([]byte, 0, to-from)
	if to == from {
		return out, nil
	}
	var sealed uint64
	if err := q.QueryRow(ctx, `SELECT sealed_offset FROM run_output WHERE run_id = ? AND lease_epoch = ?`, string(run), epoch).
		Scan(&sealed); err != nil {
		return nil, scanErr(err, "run output")
	}
	var err error
	if from < sealed {
		if out, err = db.segmentRange(ctx, q, out, run, epoch, from, min(to, sealed)); err != nil {
			return nil, err
		}
	}
	if to > sealed {
		if out, err = hotRange(ctx, q, out, run, epoch, max(from, sealed), to); err != nil {
			return nil, err
		}
	}
	return out, nil
}

type outputKey struct {
	run   string
	epoch uint64
}

// unsealed lists the outputs whose hot tail holds a whole segment or more.
func (db *DB) unsealed(ctx context.Context) ([]outputKey, error) {
	rows, err := db.reader.QueryContext(ctx,
		`SELECT run_id, lease_epoch FROM run_output WHERE end_offset - sealed_offset >= ?`, SegmentBytes)
	if err != nil {
		return nil, fmt.Errorf("store: finding output to seal: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var keys []outputKey
	for rows.Next() {
		var k outputKey
		if err := rows.Scan(&k.run, &k.epoch); err != nil {
			return nil, fmt.Errorf("store: scanning output to seal: %w", err)
		}
		keys = append(keys, k)
	}
	return keys, rows.Err()
}

type segment struct {
	first, last uint64
	digest      string
}

// segmentRange appends to out the bytes [from, to) of the sealed output. Each
// segment is read as a stream: the bytes before the range are discarded, not
// kept, and at most to-from are.
func (db *DB) segmentRange(ctx context.Context, q Querier, out []byte, run domain.ID, epoch, from, to uint64) ([]byte, error) {
	segs, err := indexedSegments(ctx, q, run, epoch, from, to)
	if err != nil {
		return nil, err
	}
	next := from
	for _, s := range segs {
		if s.first > next {
			return nil, fmt.Errorf("store: the output of %s has a hole at %d", run, next)
		}
		hi := min(s.last, to)
		if out, err = db.readSegment(ctx, out, s, next, hi); err != nil {
			return nil, err
		}
		next = hi
	}
	if next != to {
		return nil, fmt.Errorf("store: the output of %s ends at %d, not %d", run, next, to)
	}
	return out, nil
}

// indexedSegments lists the segments that overlap [from, to), in order. The rows
// are read whole before any file is opened: a write transaction has one
// connection, and nothing else may run on it while rows are open.
func indexedSegments(ctx context.Context, q Querier, run domain.ID, epoch, from, to uint64) ([]segment, error) {
	rows, err := q.Query(ctx,
		`SELECT first_offset, last_offset, digest FROM logs_index
		 WHERE run_id = ? AND lease_epoch = ? AND last_offset > ? AND first_offset < ? ORDER BY first_offset`,
		string(run), epoch, from, to)
	if err != nil {
		return nil, fmt.Errorf("store: reading the index of an output: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var segs []segment
	for rows.Next() {
		var s segment
		if err := rows.Scan(&s.first, &s.last, &s.digest); err != nil {
			return nil, fmt.Errorf("store: scanning the index of an output: %w", err)
		}
		segs = append(segs, s)
	}
	return segs, rows.Err()
}

// readSegment appends [from, to) of the raw bytes of the segment s to out.
func (db *DB) readSegment(ctx context.Context, out []byte, s segment, from, to uint64) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	blobs, err := db.segments()
	if err != nil {
		return nil, err
	}
	rc, err := blobs.OpenGzip(blob.Digest(s.digest))
	if err != nil {
		return nil, fmt.Errorf("store: opening segment %s: %w", s.digest, err)
	}
	defer func() { _ = rc.Close() }()
	if _, err := io.CopyN(io.Discard, rc, int64(from-s.first)); err != nil { //nolint:gosec // within a segment of SegmentBytes
		return nil, fmt.Errorf("store: segment %s is shorter than its index: %w", s.digest, err)
	}
	n := len(out)
	out = out[:n+int(to-from)] //nolint:gosec // bounded by the capacity OutputRange made
	if _, err := io.ReadFull(rc, out[n:]); err != nil {
		return nil, fmt.Errorf("store: segment %s is shorter than its index: %w", s.digest, err)
	}
	return out, nil
}
