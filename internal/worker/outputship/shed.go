package outputship

import (
	"context"
	"os"
	"path/filepath"
	"slices"

	"github.com/Autarquico/codefabric/internal/worker/execwrap"
	"github.com/Autarquico/codefabric/internal/worker/state"
)

// unsent is the output of one assignment that the Controller does not have.
type unsent struct {
	cursor state.OutputCursor
	size   uint64
	bytes  uint64
	dev    uint64
	ino    uint64
}

// Unsent is how many bytes of output the Controller has not confirmed, summed
// over the assignments whose output is still being sent. It is what the journal
// counts of output: the file itself stays on disk, whatever was sent of it.
func (s *Shipper) Unsent(ctx context.Context) (uint64, error) {
	pending, err := s.unsent(ctx)
	if err != nil {
		return 0, err
	}
	var total uint64
	for _, u := range pending {
		total += u.bytes
	}
	return total, nil
}

// Shed gives up sending at least need bytes of output, the Runs with most
// waiting first, by moving their cursors to the end of what their logs hold now.
// The output.log files are not touched: the Controller sees a gap at the offset
// where sending resumes, and the bytes skipped remain on this machine. It
// records what it skipped in the open truncation and answers how many bytes.
func (s *Shipper) Shed(ctx context.Context, need uint64) (uint64, error) {
	pending, err := s.unsent(ctx)
	if err != nil {
		return 0, err
	}
	slices.SortStableFunc(pending, func(a, b unsent) int {
		switch {
		case a.bytes > b.bytes:
			return -1
		case a.bytes < b.bytes:
			return 1
		}
		return 0
	})
	var shed uint64
	for _, u := range pending {
		if shed >= need {
			break
		}
		skipped, err := s.skip(ctx, u)
		if err != nil {
			return shed, err
		}
		shed += skipped
	}
	return shed, nil
}

// skip moves the cursor of one assignment to the size its log had when it was
// measured, unless the cursor moved meanwhile. It answers the bytes skipped.
func (s *Shipper) skip(ctx context.Context, u unsent) (uint64, error) {
	var skipped uint64
	err := s.o.DB.WithTx(ctx, func(tx *state.Tx) error {
		cur, err := state.OutputCursorOf(ctx, tx, u.cursor.Key)
		if err != nil {
			return err
		}
		if cur.Done || cur.Sent >= u.size {
			return nil
		}
		skipped = u.size - cur.Sent
		cur.Sent = u.size
		if !cur.HasFile {
			cur.HasFile, cur.Dev, cur.Ino = true, u.dev, u.ino
		}
		if err := state.SaveOutputCursor(ctx, tx, cur); err != nil {
			return err
		}
		return state.RecordShed(ctx, tx, state.Shed{OutputBytes: skipped}, s.o.Clock.Now())
	})
	return skipped, err
}

func (s *Shipper) unsent(ctx context.Context) ([]unsent, error) {
	runs, err := state.OutputRuns(ctx, s.o.DB.Reader())
	if err != nil {
		return nil, err
	}
	var out []unsent
	for _, r := range runs {
		cur, err := state.OutputCursorOf(ctx, s.o.DB.Reader(), r.Key)
		if err != nil {
			return nil, err
		}
		if cur.Done {
			continue
		}
		info, err := os.Lstat(filepath.Join(s.o.Paths.WorkerRunDir(r.RunID, r.LeaseEpoch), execwrap.OutputLog))
		if err != nil || !info.Mode().IsRegular() {
			continue // no log yet, or not a file the shipper reads
		}
		size := uint64(info.Size()) //nolint:gosec // a regular file's size is not negative
		dev, ino := identityOf(info)
		if cur.HasFile && (cur.Dev != dev || cur.Ino != ino) {
			continue // another file: the shipper closes this output, there is nothing to skip
		}
		if size <= cur.Sent {
			continue
		}
		out = append(out, unsent{cursor: cur, size: size, bytes: size - cur.Sent, dev: dev, ino: ino})
	}
	return out, ctx.Err()
}
