package store

import (
	"context"
	"fmt"
	"io"

	"github.com/Autarquico/codefabric/internal/controller/blob"
	"github.com/Autarquico/codefabric/internal/domain"
)

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
	if db.blobs == nil {
		return nil, fmt.Errorf("%w: there is no blob store for segment %s", ErrOutputUnreadable, s.digest)
	}
	rc, err := db.blobs.OpenGzip(blob.Digest(s.digest))
	if err != nil {
		return nil, fmt.Errorf("%w: opening segment %s: %w", ErrOutputUnreadable, s.digest, err)
	}
	defer func() { _ = rc.Close() }()
	if _, err := io.CopyN(io.Discard, rc, int64(from-s.first)); err != nil { //nolint:gosec // within a segment of SegmentBytes
		return nil, fmt.Errorf("%w: segment %s is shorter than its index: %w", ErrOutputUnreadable, s.digest, err)
	}
	n := len(out)
	out = out[:n+int(to-from)] //nolint:gosec // bounded by the capacity OutputRange made
	if _, err := io.ReadFull(rc, out[n:]); err != nil {
		return nil, fmt.Errorf("%w: segment %s is shorter than its index: %w", ErrOutputUnreadable, s.digest, err)
	}
	if to == s.last {
		// The range reaches the end of the segment: reading to EOF makes gzip check its CRC.
		if _, err := io.Copy(io.Discard, rc); err != nil {
			return nil, fmt.Errorf("%w: segment %s fails its checksum: %w", ErrOutputUnreadable, s.digest, err)
		}
	}
	return out, nil
}
