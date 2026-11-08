package store

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/Autarquico/codefabric/internal/domain"
	"github.com/Autarquico/codefabric/internal/support/clock"
)

// SegmentBytes is the size at which the hot bytes of an output are sealed into
// an immutable gzip segment of the blob store.
//
// An output is therefore: the segments of logs_index, which tile
// [Start, Sealed), and the hot chunks of SQLite, which hold [Sealed, End).
// Nothing is dropped from the hot tail before it is indexed in a segment.
//
// Sealing never happens inside the transaction that appends: gzip and fsync
// would hold the Controller's only writer. The append commits, and a sealer
// (one per DB, in the background) takes it from there: it reads the range from
// a snapshot, writes the gzip to the CAS (fsynced), and only then opens a short
// write transaction that checks the range is still the one it read, indexes it
// and retires its chunks. A crash, or a range that changed meanwhile (a gap),
// leaves at most an orphan blob, content-addressed and idempotent to write
// again; collecting orphans is retention-gc's job, which finds them by their
// absence from logs_index. The segments are not recorded in blobs_meta, so no
// route of the blob API reaches them: logs_index is their only reference count.
const SegmentBytes = 1 << 20

// MaxUnsealedBytes is how much unsealed output of one (Run, epoch) the
// Controller holds hot before it asks the Worker to wait: AppendOutput returns
// ErrOutputBusy and the Worker sends the bytes again, which keeps a sealer that
// falls behind (or fails) from growing the hot tail without bound. It is only
// enforced when there is a sealer.
const MaxUnsealedBytes = 8 * SegmentBytes

// DefaultOutputMaxBytes is Options.OutputMaxBytes when it is zero.
const DefaultOutputMaxBytes = 2 << 30

type outputKey struct {
	run   string
	epoch uint64
}

// sealTarget is where the next segment of the output ends, or o.Sealed if there
// is nothing to seal: a whole segment, or the remainder of a closed output.
func sealTarget(o Output) uint64 {
	switch n := o.End - o.Sealed; {
	case n >= SegmentBytes:
		return o.Sealed + SegmentBytes
	case n > 0 && o.Closed:
		return o.End
	}
	return o.Sealed
}

// markSealDue says that the output may have bytes to seal once this
// transaction commits.
func (t *Tx) markSealDue(o Output) {
	if t.db.sealer != nil && sealTarget(o) > o.Sealed {
		t.sealDue = append(t.sealDue, outputKey{string(o.RunID), o.Epoch})
	}
}

type sealer struct {
	db     *DB
	pass   sync.Mutex // one pass at a time, whoever runs it
	mu     sync.Mutex // guards due and failing
	due    map[outputKey]struct{}
	failed map[outputKey]sealFailure
	wake   chan struct{}
	cancel context.CancelFunc
	done   chan struct{}
}

func newSealer(db *DB) *sealer {
	return &sealer{db: db, due: map[outputKey]struct{}{}, failed: map[outputKey]sealFailure{}, wake: make(chan struct{}, 1)}
}

// seed finds the outputs that have something to seal: a ring written before
// segments existed, or an output whose seal never happened.
func (s *sealer) seed(ctx context.Context) error {
	rows, err := s.db.reader.QueryContext(ctx,
		`SELECT run_id, lease_epoch FROM run_output
		 WHERE end_offset - sealed_offset >= ? OR (closed_at IS NOT NULL AND end_offset > sealed_offset)
		    OR EXISTS (SELECT 1 FROM run_output_gaps g WHERE g.run_id = run_output.run_id AND g.lease_epoch = run_output.lease_epoch AND g.to_offset > run_output.sealed_offset)`, SegmentBytes)
	if err != nil {
		return fmt.Errorf("store: finding output to seal: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var keys []outputKey
	for rows.Next() {
		var k outputKey
		if err := rows.Scan(&k.run, &k.epoch); err != nil {
			return fmt.Errorf("store: scanning output to seal: %w", err)
		}
		keys = append(keys, k)
	}
	if err := rows.Err(); err != nil {
		return err
	}
	s.add(keys...)
	return nil
}

func (s *sealer) add(keys ...outputKey) {
	s.mu.Lock()
	for _, k := range keys {
		s.due[k] = struct{}{}
	}
	s.mu.Unlock()
	select {
	case s.wake <- struct{}{}:
	default:
	}
}

func (s *sealer) take() []outputKey {
	s.mu.Lock()
	defer s.mu.Unlock()
	keys := make([]outputKey, 0, len(s.due))
	for k := range s.due {
		keys = append(keys, k)
	}
	clear(s.due)
	return keys
}

// Between two attempts after a failed seal of an output the sealer waits
// sealRetryMin, then twice as long each time, up to sealRetryMax, on the DB's
// clock. The wait belongs to the output: activity (a wake) does not shorten it,
// and does not hold back the other outputs.
const (
	sealRetryMin = time.Second
	sealRetryMax = 30 * time.Second
)

// sealFailure is when a failing output may be tried again, and the wait that
// follows if that attempt fails too.
type sealFailure struct {
	next    time.Time
	backoff time.Duration
}

// fail records a failed attempt on k.
func (s *sealer) fail(k outputKey) {
	s.mu.Lock()
	defer s.mu.Unlock()
	f, ok := s.failed[k]
	if !ok {
		f.backoff = sealRetryMin
	}
	s.failed[k] = sealFailure{next: s.db.clock.Now().Add(f.backoff), backoff: min(2*f.backoff, sealRetryMax)}
	s.due[k] = struct{}{}
}

// forgive clears the failure state of k.
func (s *sealer) forgive(k outputKey) {
	s.mu.Lock()
	delete(s.failed, k)
	s.mu.Unlock()
}

// waiting reports whether k is in backoff; if so it goes back into due, without
// waking the sealer.
func (s *sealer) waiting(k outputKey) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	f, ok := s.failed[k]
	if !ok || !s.db.clock.Now().Before(f.next) {
		return false
	}
	s.due[k] = struct{}{}
	return true
}

// untilRetry is how long until the earliest output in backoff may be tried.
func (s *sealer) untilRetry() (time.Duration, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var (
		earliest time.Time
		found    bool
	)
	for _, f := range s.failed {
		if !found || f.next.Before(earliest) {
			earliest, found = f.next, true
		}
	}
	return max(earliest.Sub(s.db.clock.Now()), 0), found
}

func (s *sealer) start() {
	ctx, cancel := context.WithCancel(context.Background())
	s.cancel, s.done = cancel, make(chan struct{})
	go func() {
		defer close(s.done)
		var (
			retry   clock.Timer
			retryAt <-chan time.Time
		)
		disarm := func() {
			if retry != nil {
				retry.Stop()
				retry, retryAt = nil, nil
			}
		}
		defer disarm()
		for {
			select {
			case <-ctx.Done():
				return
			case <-s.wake:
			case <-retryAt:
			}
			disarm()
			_, err := s.pass1(ctx)
			if ctx.Err() != nil {
				continue
			}
			if in, ok := s.untilRetry(); ok {
				if err != nil {
					s.db.log.Warn("store: sealing output failed; it will be tried again", "err", err, "in", in)
				}
				retry = s.db.clock.NewTimer(in)
				retryAt = retry.C()
			}
		}
	}()
}

func (s *sealer) stop() {
	if s.cancel != nil {
		s.cancel()
		<-s.done
	}
}

// drain seals everything that is due, until nothing is.
func (s *sealer) drain(ctx context.Context) error {
	for {
		progressed, err := s.pass1(ctx)
		if err != nil || !progressed {
			return err
		}
	}
}

// pass1 is one round-robin pass: it seals at most ONE segment of each due
// output, so a fast output cannot hold the sealer while the others sit at
// MaxUnsealedBytes. An output that still has something to seal goes back into
// due and wakes the sealer; one whose seal fails stays due but is skipped by
// every pass, whoever runs it and whatever woke it, until its backoff is over;
// the background sealer arms its timer for the earliest. It reports whether any
// segment was sealed.
func (s *sealer) pass1(ctx context.Context) (bool, error) {
	s.pass.Lock()
	defer s.pass.Unlock()
	var (
		errs       []error
		progressed bool
	)
	for _, k := range s.take() {
		if s.waiting(k) {
			continue
		}
		more, sealed, err := s.sealOne(ctx, k)
		progressed = progressed || sealed
		switch {
		case err != nil:
			errs = append(errs, fmt.Errorf("the output of %s: %w", k.run, err))
			s.fail(k)
		case more:
			s.forgive(k)
			s.add(k)
		default:
			s.forgive(k)
		}
	}
	return progressed, errors.Join(errs...)
}

// sealOne seals the next segment of the output, if there is one. It reports
// whether the output has another to seal and whether this call sealed one.
func (s *sealer) sealOne(ctx context.Context, k outputKey) (more, sealed bool, err error) {
	p, err := s.prepare(ctx, k)
	if err != nil || p == nil {
		return false, false, err
	}
	sealed, err = s.install(ctx, p)
	return sealed && p.more, sealed, err
}

// sealing is a segment written to the CAS and not yet indexed.
type sealing struct {
	key         outputKey
	first, last uint64
	digest      string
	storedBytes int64
	rawBytes    int
	more        bool // set by install: the output has another segment to seal
}

// prepare reads the next range to seal from a snapshot and writes its gzip to
// the CAS. It holds no writer. It returns nil if there is nothing to seal.
func (s *sealer) prepare(ctx context.Context, k outputKey) (*sealing, error) {
	var (
		o      Output
		raw    []byte
		jumpTo uint64
	)
	err := s.db.Read(ctx, func(q Querier) (err error) {
		if o, err = OutputOf(ctx, q, domain.ID(k.run), k.epoch); err != nil {
			return err
		}
		upTo := sealTarget(o)
		gap, ahead, err := NextGap(ctx, q, o.RunID, o.Epoch, o.Sealed)
		switch {
		case err != nil:
			return err
		case ahead && gap.From <= o.Sealed:
			jumpTo = gap.To // nothing to seal in a gap: the sealed offset moves over it
			return nil
		case ahead && upTo > o.Sealed:
			upTo = min(upTo, gap.From)
		case ahead:
			upTo = gap.From // what precedes a gap is sealed whole, however little
		}
		if upTo > o.Sealed {
			raw, err = hotRange(ctx, q, make([]byte, 0, upTo-o.Sealed), o.RunID, o.Epoch, o.Sealed, upTo)
		}
		return err
	})
	if err != nil {
		return nil, err
	}
	if jumpTo > 0 {
		return &sealing{key: k, first: o.Sealed, last: jumpTo}, nil
	}
	if len(raw) == 0 {
		return nil, nil
	}
	digest, stored, err := s.db.blobs.PutGzip(ctx, raw)
	if err != nil {
		return nil, fmt.Errorf("store: writing a segment: %w", err)
	}
	return &sealing{key: k, first: o.Sealed, last: o.Sealed + uint64(len(raw)), digest: string(digest), storedBytes: stored, rawBytes: len(raw)}, nil
}

// install indexes the segment and retires its chunks, in a short transaction,
// if the output still holds the range it was cut from: the bytes below End never
// change, so that is Sealed unmoved (a gap moves it past the range) and End not
// below. If not, the segment is an orphan and false is returned.
func (s *sealer) install(ctx context.Context, p *sealing) (bool, error) {
	installed := false
	err := s.db.WithTx(ctx, func(tx *Tx) error {
		installed = false
		o, err := OutputOf(ctx, tx, domain.ID(p.key.run), p.key.epoch)
		switch {
		case errors.Is(err, ErrNotFound):
			return nil
		case err != nil:
			return err
		case o.Sealed != p.first || o.End < p.last || o.Start > p.first:
			return nil
		}
		if p.rawBytes > 0 { // none when the sealed offset only jumps over a gap
			if _, err := tx.Exec(ctx,
				`INSERT INTO logs_index (run_id, lease_epoch, seg_no, first_offset, last_offset, raw_bytes, digest)
				 VALUES (?, ?, (SELECT COALESCE(MAX(seg_no) + 1, 0) FROM logs_index WHERE run_id = ? AND lease_epoch = ?), ?, ?, ?, ?)`,
				p.key.run, p.key.epoch, p.key.run, p.key.epoch, p.first, p.last, p.rawBytes, p.digest); err != nil {
				return fmt.Errorf("store: indexing a segment: %w", err)
			}
		}
		if err := tx.trimOutput(ctx, o.RunID, o.Epoch, p.last); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `UPDATE run_output SET sealed_offset = ? WHERE run_id = ? AND lease_epoch = ?`, p.last, p.key.run, p.key.epoch); err != nil {
			return fmt.Errorf("store: moving the sealed offset: %w", err)
		}
		o.Sealed = p.last
		_, gapAhead, err := NextGap(ctx, tx, o.RunID, o.Epoch, p.last)
		if err != nil {
			return err
		}
		p.more = sealTarget(o) > p.last || gapAhead
		installed = true
		return nil
	})
	return installed, err
}

// SealPending seals, now and in the calling goroutine, the outputs that are
// due. The background sealer does the same on its own; this is for tests and
// for a shutdown that wants the hot tail short. Without a blob store it does nothing.
func (db *DB) SealPending(ctx context.Context) error {
	if db.sealer == nil {
		return nil
	}
	return db.sealer.drain(ctx)
}

// SealOutputs finds every output with bytes to seal (a ring written before
// segments existed, or a seal that never finished) and seals them now.
func (db *DB) SealOutputs(ctx context.Context) error {
	if db.sealer == nil {
		return nil
	}
	if err := db.sealer.seed(ctx); err != nil {
		return err
	}
	return db.sealer.drain(ctx)
}
