// Package outputship sends the output of the Runs of this Worker to the
// Controller (#14, §5.4: the Worker connects to the Controller, never the other
// way). Every second, if there is something to say, it makes one request with
// the new bytes of the output.log of every Run that has any, so a Worker costs
// the Controller at most one transaction a second however many Runs it has.
//
// The output of a lease epoch is append-only, and the shipper treats it so: it
// remembers which file it read (device and inode) and how far, and a file that
// is another or shorter than what was sent is not read again; the output is
// closed there with a note. The log is read the way an untrusted file is, since
// the agent of the Run can write in the directory: opened without following a
// symlink and without blocking, checked to be a regular file on the descriptor
// it was opened as, and read no further than the size that descriptor reports.
// The intermediate directories belong to the same user as the daemon, and the
// tmux Runtime does not isolate the two: that limit is documented in the
// decisions of the milestone.
package outputship

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"syscall"
	"time"

	v1 "github.com/Autarquico/codefabric/internal/api/v1"
	"github.com/Autarquico/codefabric/internal/config"
	"github.com/Autarquico/codefabric/internal/domain"
	"github.com/Autarquico/codefabric/internal/support/clock"
	"github.com/Autarquico/codefabric/internal/worker/execwrap"
	"github.com/Autarquico/codefabric/internal/worker/state"
)

// Limits and timings. The ring of the Controller holds the last MiB, so there is
// no point in sending what is further back; a batch carries at most batchBytes of
// output, which is base64 of at most 4/3 of that in a body the Controller reads up to
// v1.OutputBatchBytes.
const (
	interval    = time.Second
	batchBytes  = 1 << 20
	perRunBytes = 2 * v1.OutputChunkBytes
	settle      = time.Second // the file must not grow for this long before it is closed
	backoffMax  = 30 * time.Second

	noteReplaced = "output.log was replaced or truncated"
)

// Uploader is the Controller as the shipper uses it.
type Uploader interface {
	UploadOutput(ctx context.Context, batch v1.OutputBatch) (v1.OutputBatchResponse, error)
}

// Options parameterise New.
type Options struct {
	DB       *state.DB
	Uploader Uploader
	Paths    config.Paths
	Clock    clock.Clock
	Log      *slog.Logger
	// Interval is the time between passes; zero is one second, which is also the
	// least a Worker may send at.
	Interval time.Duration
}

// Shipper sends the output.
type Shipper struct {
	o        Options
	failures int
	retryAt  time.Time
	rotate   int
	final    map[state.Key]finalLook // the size of a finished Run's log at the last look
}

type finalLook struct {
	size uint64
	at   time.Time
}

// New builds the shipper.
func New(o Options) *Shipper {
	if o.Interval < interval {
		o.Interval = interval
	}
	return &Shipper{o: o, final: map[state.Key]finalLook{}}
}

// Run sends until ctx ends.
func (s *Shipper) Run(ctx context.Context) error {
	ticker := s.o.Clock.NewTicker(s.o.Interval)
	defer ticker.Stop()
	for {
		if err := s.Once(ctx); err != nil && ctx.Err() == nil {
			s.o.Log.WarnContext(ctx, "sending output failed", "err", err)
		}
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C():
		}
	}
}

// item is what a pass plans to send for one assignment, and what to remember
// when the Controller acknowledges it.
type item struct {
	cursor  state.OutputCursor
	size    uint64 // of the file when it was read
	entries []v1.OutputEntry
	// reaches is the offset the cursor moves to when the data entry is acknowledged.
	reaches uint64
	closing bool // the last entry closes the output
}

// Once is one pass: it looks at the output of every assignment that may have
// some, and sends what is new in one request. It sends nothing if there is
// nothing, and waits out the backoff after a failure.
func (s *Shipper) Once(ctx context.Context) error {
	if s.o.Clock.Now().Before(s.retryAt) {
		return nil
	}
	runs, err := state.OutputRuns(ctx, s.o.DB.Reader())
	if err != nil {
		return err
	}
	var (
		items   []*item
		batch   v1.OutputBatch
		budget  = batchBytes
		visited int
	)
	// The pass starts where the last one stopped, so that when a batch cannot
	// hold everything the Runs at the end of the list are not always the ones left out.
	if len(runs) > 0 {
		runs = append(runs[s.rotate%len(runs):], runs[:s.rotate%len(runs)]...)
	}
	defer func() { s.rotate += visited }()
	for _, r := range runs {
		if budget <= 0 || len(batch.Entries) >= v1.OutputBatchMaxItems {
			break
		}
		it, err := s.plan(ctx, r, min(perRunBytes, budget))
		if err != nil {
			return err
		}
		if it == nil {
			visited++
			continue
		}
		if len(batch.Entries)+len(it.entries) > v1.OutputBatchMaxItems {
			break // its entries go whole, in the next pass: the Controller refuses a batch of more
		}
		visited++
		for _, e := range it.entries {
			budget -= len(e.Data)
		}
		items = append(items, it)
		batch.Entries = append(batch.Entries, it.entries...)
	}
	if len(batch.Entries) == 0 {
		return nil
	}
	resp, err := s.o.Uploader.UploadOutput(ctx, batch)
	if err != nil {
		s.failures++
		s.retryAt = s.o.Clock.Now().Add(min(time.Duration(1<<min(s.failures, 5))*time.Second, backoffMax))
		return fmt.Errorf("outputship: uploading: %w", err)
	}
	s.failures = 0
	if len(resp.Acks) != len(batch.Entries) {
		return fmt.Errorf("outputship: %d acknowledgements for %d entries", len(resp.Acks), len(batch.Entries))
	}
	acks := resp.Acks
	for _, it := range items {
		if err := s.settle(ctx, it, acks[:len(it.entries)]); err != nil {
			return err
		}
		acks = acks[len(it.entries):]
	}
	return nil
}

func isFinal(r state.Run) bool { return r.Phase == state.PhaseStopped }

// plan decides what to send of one assignment: the new bytes of its log, then
// its end if the Run is over and the log has stopped growing. nil is nothing.
func (s *Shipper) plan(ctx context.Context, r state.Run, room int) (*item, error) {
	cur, err := state.OutputCursorOf(ctx, s.o.DB.Reader(), r.Key)
	if err != nil {
		return nil, err
	}
	if cur.Done {
		return nil, nil
	}
	it := &item{cursor: cur}
	entry := func(e v1.OutputEntry) {
		e.RunID, e.LeaseEpoch = r.RunID, r.LeaseEpoch
		it.entries = append(it.entries, e)
	}
	closeAt := func(final uint64, note string) {
		entry(v1.OutputEntry{EOF: true, FinalOffset: &final, Note: note})
		it.closing = true
	}

	path := filepath.Join(s.o.Paths.WorkerRunDir(r.RunID, r.LeaseEpoch), execwrap.OutputLog)
	f, err := openLog(path)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		if !isFinal(r) {
			return nil, nil // the harness has not written yet
		}
		closeAt(cur.Sent, "") // it never wrote: the output is empty
		return it, nil
	case err != nil:
		// A symlink, a device, a pipe where the log should be: not read, and said so.
		s.o.Log.WarnContext(ctx, "output.log is not a regular file: closing its output", "run", r.RunID, "err", err)
		closeAt(cur.Sent, noteReplaced)
		return it, nil
	}
	defer func() { _ = f.Close() }()
	info, err := f.Stat()
	if err != nil {
		return nil, fmt.Errorf("outputship: stat of the log of %s: %w", r.RunID, err)
	}
	dev, ino := identityOf(info)
	size := uint64(info.Size()) //nolint:gosec // a regular file's size is not negative
	it.size = size
	if cur.HasFile && (cur.Dev != dev || cur.Ino != ino || size < cur.Sent) {
		closeAt(cur.Sent, noteReplaced)
		return it, nil
	}
	if !cur.HasFile {
		it.cursor.HasFile, it.cursor.Dev, it.cursor.Ino = true, dev, ino
	}

	pos := cur.Sent
	if size > pos && size-pos > v1.OutputTailBytes {
		pos = size - v1.OutputTailBytes // the Controller keeps the last MiB: the rest would be dropped on arrival
	}
	for size > pos && room > 0 {
		buf := make([]byte, min(uint64(room), size-pos, v1.OutputChunkBytes))
		n, err := f.ReadAt(buf, int64(pos)) //nolint:gosec // below the file's size
		if n == 0 {
			if err == nil {
				err = fs.ErrInvalid
			}
			return nil, fmt.Errorf("outputship: reading the log of %s: %w", r.RunID, err)
		}
		entry(v1.OutputEntry{Offset: pos, Data: buf[:n]})
		pos += uint64(n) //nolint:gosec // n is a count of bytes read
		room -= n
		it.reaches = pos
	}
	if isFinal(r) && pos == size {
		if s.stable(r.Key, size) {
			closeAt(size, "")
		}
	} else {
		delete(s.final, r.Key)
	}
	if len(it.entries) == 0 {
		// Nothing to send, but the identity of the file was learnt: keep it.
		if !cur.HasFile {
			return nil, s.o.DB.WithTx(ctx, func(tx *state.Tx) error { return state.SaveOutputCursor(ctx, tx, it.cursor) })
		}
		return nil, nil
	}
	return it, nil
}

// stable says whether the log of a finished Run has had the same size for a
// whole settle: the capture of a terminal ends a moment after its session does.
func (s *Shipper) stable(k state.Key, size uint64) bool {
	now := s.o.Clock.Now()
	seen, ok := s.final[k]
	if !ok || seen.size != size {
		s.final[k] = finalLook{size: size, at: now}
		return false
	}
	return now.Sub(seen.at) >= settle
}

// settle applies the acknowledgements of one assignment's entries.
func (s *Shipper) settle(ctx context.Context, it *item, acks []v1.OutputAck) error {
	cur := it.cursor
	for i, ack := range acks {
		e := it.entries[i]
		switch ack.Code {
		case "":
			if e.EOF {
				cur.Done, cur.EOFSent = true, true
				delete(s.final, cur.Key)
				continue
			}
			cur.Sent = it.reaches
			if ack.End > cur.Sent && ack.End <= it.size {
				cur.Sent = ack.End // the Controller has more than this Worker remembered: do not send it again
			}
		case domain.CodeStaleLease, domain.CodeNotFound, domain.CodePolicyDenied, domain.CodeInvalidRequest, domain.CodeInvalidTransition:
			// Not this Worker's any more, or not reconcilable: it will not be accepted by trying again.
			s.o.Log.WarnContext(ctx, "the Controller refused the output of a Run; no more of it is sent",
				"run", e.RunID, "epoch", e.LeaseEpoch, "code", ack.Code)
			cur.Done = true
			delete(s.final, cur.Key)
			continue
		default:
			s.o.Log.WarnContext(ctx, "the Controller could not take output; it will be sent again", "run", e.RunID, "code", ack.Code)
			return nil
		}
	}
	return s.o.DB.WithTx(ctx, func(tx *state.Tx) error { return state.SaveOutputCursor(ctx, tx, cur) })
}

// openLog opens output.log for reading, safely: a symlink is refused, a FIFO or
// device does not block the open, and anything that is not a regular file is
// refused on the descriptor, not on a path that may change under us.
func openLog(path string) (*os.File, error) {
	f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0) //nolint:gosec // the path is a run directory of the daemon's
	if err != nil {
		return nil, err
	}
	info, err := f.Stat()
	if err != nil {
		_ = f.Close()
		return nil, err
	}
	if !info.Mode().IsRegular() {
		_ = f.Close()
		return nil, fmt.Errorf("outputship: %s is not a regular file", filepath.Base(path))
	}
	return f, nil
}

func identityOf(info os.FileInfo) (dev, ino uint64) {
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return 0, 0
	}
	return uint64(st.Dev), uint64(st.Ino) //nolint:gosec,unconvert // the identity, compared as it was taken
}
