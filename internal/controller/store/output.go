package store

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	v1 "github.com/Autarquico/codefabric/internal/api/v1"
	"github.com/Autarquico/codefabric/internal/domain"
)

// Errors of the output of a Run. The use case turns them into coded errors.
var (
	// ErrOutputInvalid: the offsets or the bytes are out of range.
	ErrOutputInvalid = errors.New("output out of range")
	// ErrOutputConflict: the bytes sent for a range the store still holds are
	// not the bytes it holds, or the Worker closed the output somewhere else.
	ErrOutputConflict = errors.New("output contradicts what the store holds")
	// ErrOutputClosed: the output was closed, and no byte is added to it.
	ErrOutputClosed = errors.New("output is closed")
	// ErrOutputIncomplete: the Worker closed an output whose last bytes the store
	// has not received.
	ErrOutputIncomplete = errors.New("output closed before its last bytes arrived")
	// ErrOutputBusy: the sealer is behind and the hot tail of the output is full.
	// Nothing was stored; the same bytes can be sent again later.
	ErrOutputBusy = errors.New("output is waiting to be sealed")
	// ErrOutputUnreadable: a sealed segment is missing, corrupt or shorter than its index.
	ErrOutputUnreadable = errors.New("sealed output cannot be read")
)

// OutputLimitNote is the note of an output that was closed for reaching the cap.
const OutputLimitNote = "output limit reached"

// Output is what the store holds of the output of a Run under a lease epoch:
// the bytes [Start, End). [Start, Sealed) are in immutable segments (logs_index)
// and [Sealed, End) in the hot tail. FinalOffset is where the Worker says it
// ends, when Closed.
type Output struct {
	RunID       domain.ID
	Epoch       uint64
	Start, End  uint64
	Sealed      uint64
	Accepted    uint64 // bytes accepted so far, which a gap does not give back: what the cap counts
	Closed      bool
	FinalOffset uint64
	Note        string
	UpdatedAt   time.Time
}

const outputColumns = `start_offset, end_offset, sealed_offset, accepted_bytes, closed_at IS NOT NULL, COALESCE(final_offset, 0), COALESCE(note, ''), updated_at`

func scanOutput(run domain.ID, epoch uint64, row rowScanner) (Output, error) {
	o := Output{RunID: run, Epoch: epoch}
	var updated int64
	if err := row.Scan(&o.Start, &o.End, &o.Sealed, &o.Accepted, &o.Closed, &o.FinalOffset, &o.Note, &updated); err != nil {
		return Output{}, scanErr(err, "run output")
	}
	o.UpdatedAt = time.Unix(0, updated).UTC()
	return o, nil
}

// OutputOf returns the output of the Run under the epoch, or ErrNotFound if
// nothing was ever received for it.
func OutputOf(ctx context.Context, q Querier, run domain.ID, epoch uint64) (Output, error) {
	return scanOutput(run, epoch, q.QueryRow(ctx,
		`SELECT `+outputColumns+` FROM run_output WHERE run_id = ? AND lease_epoch = ?`, string(run), epoch))
}

// LatestOutputEpoch is the highest lease epoch the Run has output for.
func LatestOutputEpoch(ctx context.Context, q Querier, run domain.ID) (epoch uint64, found bool, err error) {
	var n sql.NullInt64
	if err := q.QueryRow(ctx, `SELECT MAX(lease_epoch) FROM run_output WHERE run_id = ?`, string(run)).Scan(&n); err != nil {
		return 0, false, fmt.Errorf("store: finding the output of a run: %w", err)
	}
	return uint64(max(n.Int64, 0)), n.Valid, nil //nolint:gosec // a lease epoch is >= 1 by the table's check
}

// hotRange appends to out the bytes [from, to) of the hot chunks, which must
// lie inside the hot tail.
func hotRange(ctx context.Context, q Querier, out []byte, run domain.ID, epoch, from, to uint64) ([]byte, error) {
	rows, err := q.Query(ctx,
		`SELECT chunk_offset, data FROM run_output_chunks
		 WHERE run_id = ? AND lease_epoch = ? AND chunk_offset >= ? AND chunk_offset < ? AND chunk_offset + length(data) > ?
		 ORDER BY chunk_offset`, string(run), epoch, from-min(from, v1.OutputChunkBytes), to, from)
	if err != nil {
		return nil, fmt.Errorf("store: reading output: %w", err)
	}
	defer func() { _ = rows.Close() }()
	next := from
	for rows.Next() {
		var (
			offset uint64
			data   []byte
		)
		if err := rows.Scan(&offset, &data); err != nil {
			return nil, fmt.Errorf("store: scanning output: %w", err)
		}
		if offset > next {
			return nil, fmt.Errorf("store: the output of %s has a hole at %d", run, next)
		}
		lo := next - offset
		hi := min(uint64(len(data)), to-offset)
		out = append(out, data[lo:hi]...)
		next = offset + hi
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: reading output: %w", err)
	}
	if next != to {
		return nil, fmt.Errorf("store: the output of %s ends at %d, not %d", run, next, to)
	}
	return out, nil
}

// MarkOutput says that the transaction changed some output, so that the readers
// that follow it are woken once it commits.
func (t *Tx) MarkOutput() { t.output = true }

// AppendOutput adds data, which starts at offset, to the output of the Run under
// the epoch, and returns what the store holds after it. The output is
// append-only, so it is idempotent without a request ID:
//   - offset == End appends;
//   - bytes below End are the ones already stored (compared, while the store
//     still holds them: a difference is ErrOutputConflict), and only what lies
//     above End is added;
//   - offset > End leaves a hole the Worker skipped: what was held and not yet
//     sealed is dropped, as data the Worker abandoned, and the output restarts
//     at offset. A hole never writes a segment, so a Worker that skips ahead
//     over and over costs no more than the bytes it sends.
//
// The hot tail is sealed into immutable segments after the commit, by the
// sealer (see SegmentBytes); no byte is trimmed away before it is indexed. If
// there are MaxUnsealedBytes waiting, new bytes are refused with ErrOutputBusy.
//
// An output holds at most OutputMaxBytes of what was accepted. The append that
// would pass it closes the output at what it holds, with OutputLimitNote, and
// returns ErrOutputClosed: the close is part of the transaction, so the caller
// that turns the error into an answer must commit it (the use case does).
func (t *Tx) AppendOutput(ctx context.Context, run domain.ID, epoch, offset uint64, data []byte, at time.Time) (Output, error) {
	switch {
	case epoch < 1, len(data) == 0, len(data) > v1.OutputChunkBytes,
		offset > v1.OutputMaxOffset, uint64(len(data)) > v1.OutputMaxOffset-offset:
		return Output{}, fmt.Errorf("%w: %d bytes at %d of epoch %d", ErrOutputInvalid, len(data), offset, epoch)
	}
	o, err := OutputOf(ctx, t, run, epoch)
	switch {
	case errors.Is(err, ErrNotFound):
		o = Output{RunID: run, Epoch: epoch, Start: offset, End: offset, Sealed: offset}
		if _, err := t.Exec(ctx,
			`INSERT INTO run_output (run_id, lease_epoch, start_offset, end_offset, sealed_offset, updated_at) VALUES (?, ?, ?, ?, ?, ?)`,
			string(run), epoch, offset, offset, offset, at.UnixNano()); err != nil {
			return Output{}, fmt.Errorf("store: starting output: %w", err)
		}
	case err != nil:
		return Output{}, err
	case o.Closed:
		return Output{}, ErrOutputClosed
	}

	end := offset + uint64(len(data))
	switch {
	case offset > o.End:
		if t.overBackpressure(o) {
			// Not a gap to take while bytes wait to be sealed: dropping them
			// would lose output that was accepted.
			t.markSealDue(o)
			return Output{}, ErrOutputBusy
		}
		if o.Accepted+uint64(len(data)) > t.db.outputMax {
			// Over the cap: close what the output holds, before the gap drops any of it.
			return t.closeAtLimit(ctx, o, at)
		}
		if err := t.trimOutput(ctx, run, epoch, offset); err != nil {
			return Output{}, err
		}
		o.Start, o.End, o.Sealed = offset, offset, offset
	case offset < o.End:
		if err := t.checkOverlap(ctx, o, offset, data); err != nil {
			return Output{}, err
		}
		if end <= o.End {
			return o, nil // sent again: nothing to add, and not an append
		}
		data, offset = data[o.End-offset:], o.End
	}

	if t.overBackpressure(o) {
		t.markSealDue(o)
		return Output{}, ErrOutputBusy
	}
	if o.Accepted+uint64(len(data)) > t.db.outputMax {
		return t.closeAtLimit(ctx, o, at)
	}
	if err := t.appendChunks(ctx, run, epoch, offset, data); err != nil {
		return Output{}, err
	}
	o.End = offset + uint64(len(data))
	o.Accepted += uint64(len(data))
	if _, err := t.Exec(ctx,
		`UPDATE run_output SET start_offset = ?, end_offset = ?, sealed_offset = ?, accepted_bytes = ?, updated_at = ? WHERE run_id = ? AND lease_epoch = ?`,
		o.Start, o.End, o.Sealed, o.Accepted, at.UnixNano(), string(run), epoch); err != nil {
		return Output{}, fmt.Errorf("store: updating output: %w", err)
	}
	o.UpdatedAt = at
	t.MarkOutput()
	t.markSealDue(o)
	return o, nil
}

// overBackpressure says that the output holds MaxUnsealedBytes waiting to be sealed.
func (t *Tx) overBackpressure(o Output) bool {
	return t.db.sealer != nil && o.End-o.Sealed >= MaxUnsealedBytes
}

// closeAtLimit closes the output where it stands, for having reached the cap.
func (t *Tx) closeAtLimit(ctx context.Context, o Output, at time.Time) (Output, error) {
	if _, err := t.Exec(ctx,
		`UPDATE run_output SET start_offset = ?, end_offset = ?, sealed_offset = ?, closed_at = ?, final_offset = ?, note = ?, updated_at = ?
		 WHERE run_id = ? AND lease_epoch = ?`,
		o.Start, o.End, o.Sealed, at.UnixNano(), o.End, OutputLimitNote, at.UnixNano(), string(o.RunID), o.Epoch); err != nil {
		return Output{}, fmt.Errorf("store: closing output at its limit: %w", err)
	}
	o.Closed = true
	t.MarkOutput()
	t.markSealDue(o)
	return Output{}, ErrOutputClosed
}

// mergeBelow is the size under which the last chunk of an output takes what
// follows it. Filling it up to a whole chunk would rewrite up to 64 KiB for every
// byte a Worker sends; this way a rewrite is at most 8 KiB, and a lease has about
// 128 rows at most (1 MiB in chunks of 8 KiB) whatever the size of what is sent.
const mergeBelow = 8 << 10

// appendChunks adds data at offset, which is the end of the output. A last chunk
// under mergeBelow is filled first (a Worker that sends a byte at a time must
// not make a row of each).
func (t *Tx) appendChunks(ctx context.Context, run domain.ID, epoch, offset uint64, data []byte) error {
	var (
		lastOffset uint64
		lastLen    int
	)
	err := t.QueryRow(ctx,
		`SELECT chunk_offset, length(data) FROM run_output_chunks WHERE run_id = ? AND lease_epoch = ? ORDER BY chunk_offset DESC LIMIT 1`,
		string(run), epoch).Scan(&lastOffset, &lastLen)
	switch {
	case errors.Is(err, sql.ErrNoRows):
	case err != nil:
		return fmt.Errorf("store: finding the last chunk: %w", err)
	case lastLen < mergeBelow && lastOffset+uint64(lastLen) == offset: //nolint:gosec // length() of a chunk is not negative
		fill := min(len(data), v1.OutputChunkBytes-lastLen)
		if _, err := t.Exec(ctx, `UPDATE run_output_chunks SET data = CAST(data || ? AS BLOB) WHERE run_id = ? AND lease_epoch = ? AND chunk_offset = ?`,
			data[:fill], string(run), epoch, lastOffset); err != nil {
			return fmt.Errorf("store: filling a chunk: %w", err)
		}
		data, offset = data[fill:], offset+uint64(fill)
	}
	if len(data) == 0 {
		return nil
	}
	if _, err := t.Exec(ctx, `INSERT INTO run_output_chunks (run_id, lease_epoch, chunk_offset, data) VALUES (?, ?, ?, ?)`,
		string(run), epoch, offset, data); err != nil {
		return fmt.Errorf("store: appending output: %w", err)
	}
	return nil
}

// checkOverlap compares the bytes sent for [offset, End) with the ones held.
// Those below Start were skipped by the Worker and are not compared.
func (t *Tx) checkOverlap(ctx context.Context, o Output, offset uint64, data []byte) error {
	from, to := max(offset, o.Start), min(offset+uint64(len(data)), o.End)
	if from >= to {
		return nil
	}
	held, err := t.db.OutputRange(ctx, t, o.RunID, o.Epoch, from, to)
	if err != nil {
		return err
	}
	if !bytes.Equal(held, data[from-offset:to-offset]) {
		return fmt.Errorf("%w: bytes %d to %d differ from the ones received", ErrOutputConflict, from, to)
	}
	return nil
}

// trimOutput drops everything below newStart: the chunks that end before it, and
// the part of the chunk that crosses it. It is only called with the bytes below
// newStart already sealed.
func (t *Tx) trimOutput(ctx context.Context, run domain.ID, epoch, newStart uint64) error {
	if _, err := t.Exec(ctx,
		`DELETE FROM run_output_chunks WHERE run_id = ? AND lease_epoch = ? AND chunk_offset < ? AND chunk_offset + length(data) <= ?`,
		string(run), epoch, newStart, newStart); err != nil {
		return fmt.Errorf("store: trimming output: %w", err)
	}
	if _, err := t.Exec(ctx,
		`UPDATE run_output_chunks SET data = substr(data, ? - chunk_offset + 1), chunk_offset = ?
		 WHERE run_id = ? AND lease_epoch = ? AND chunk_offset < ?`,
		newStart, newStart, string(run), epoch, newStart); err != nil {
		return fmt.Errorf("store: trimming output: %w", err)
	}
	return nil
}

// CloseOutput records that the Worker says the output ends at final, with the
// note it gives (empty if none), and returns what the store holds. It is
// idempotent for the same final offset. The store must hold the output up to
// final: a Worker closes only what it has sent. An output the store has never
// seen, closed at final, holds nothing: it starts and ends there.
func (t *Tx) CloseOutput(ctx context.Context, run domain.ID, epoch, final uint64, note string, at time.Time) (Output, error) {
	if epoch < 1 || final > v1.OutputMaxOffset {
		return Output{}, fmt.Errorf("%w: final offset %d of epoch %d", ErrOutputInvalid, final, epoch)
	}
	o, err := OutputOf(ctx, t, run, epoch)
	switch {
	case errors.Is(err, ErrNotFound):
		o = Output{RunID: run, Epoch: epoch, Start: final, End: final, Sealed: final}
		if _, err := t.Exec(ctx,
			`INSERT INTO run_output (run_id, lease_epoch, start_offset, end_offset, sealed_offset, updated_at) VALUES (?, ?, ?, ?, ?, ?)`,
			string(run), epoch, final, final, final, at.UnixNano()); err != nil {
			return Output{}, fmt.Errorf("store: starting output: %w", err)
		}
	case err != nil:
		return Output{}, err
	case o.Closed && o.FinalOffset == final:
		return o, nil
	case o.Closed:
		return Output{}, fmt.Errorf("%w: closed at %d, not %d", ErrOutputConflict, o.FinalOffset, final)
	case o.End < final:
		return Output{}, fmt.Errorf("%w: holds up to %d, closed at %d", ErrOutputIncomplete, o.End, final)
	case o.End > final:
		return Output{}, fmt.Errorf("%w: holds up to %d, closed at %d", ErrOutputConflict, o.End, final)
	}
	if _, err := t.Exec(ctx,
		`UPDATE run_output SET closed_at = ?, final_offset = ?, note = ?, updated_at = ? WHERE run_id = ? AND lease_epoch = ?`,
		at.UnixNano(), final, nullable(note), at.UnixNano(), string(run), epoch); err != nil {
		return Output{}, fmt.Errorf("store: closing output: %w", err)
	}
	o.Closed, o.FinalOffset, o.Note, o.UpdatedAt = true, final, note, at
	t.MarkOutput()
	t.markSealDue(o) // the sealer seals what is left hot: a closed output ends up wholly in segments
	return o, nil
}
