package runs

import (
	"context"
	"errors"
	"time"
	"unicode/utf8"

	v1 "github.com/Autarquico/codefabric/internal/api/v1"
	"github.com/Autarquico/codefabric/internal/app/input"
	"github.com/Autarquico/codefabric/internal/app/refs"
	"github.com/Autarquico/codefabric/internal/app/stream"
	"github.com/Autarquico/codefabric/internal/controller/pki"
	"github.com/Autarquico/codefabric/internal/controller/store"
	"github.com/Autarquico/codefabric/internal/domain"
)

// noMoreOutputAfter is how long a Run that is over, with no session left, may
// go without output arriving before a follow of it gives up: the Worker that
// would have closed the output is gone, or its shipper abandoned it.
const noMoreOutputAfter = 30 * time.Second

const maxOutputNoteLen = 256

// A batch is applied in transactions of at most these many entries or bytes of
// data, one after the other: the writer is the Controller's only one, and a
// transaction that holds it for long delays every event and every other request
// behind it. The limits of the wire (v1.OutputBatchBytes and v1.OutputBatchMaxItems)
// are the Worker's to fill.
const (
	outputTxEntries = 32
	outputTxBytes   = 256 << 10
)

// AppendOutput is POST /v1/worker/output (#14): the output of several Runs of
// the Worker the caller stands for. Each entry is authorised on its own, against
// the lease it names (§16.8: finishing is allowed to a Run that is over), and
// answered on its own: an entry that is refused stores nothing and says why, and
// does not stop the others. The caller is authorised once per transaction, and a
// caller that is no longer valid fails the request (what earlier transactions
// stored stays: an entry sent again is a duplicate, and says so).
func (s *Service) AppendOutput(ctx context.Context, by pki.Session, batch v1.OutputBatch) (v1.OutputBatchResponse, error) {
	if len(batch.Entries) > v1.OutputBatchMaxItems {
		return v1.OutputBatchResponse{}, input.Invalid("at most %d entries in a batch", v1.OutputBatchMaxItems)
	}
	out := v1.OutputBatchResponse{Acks: make([]v1.OutputAck, 0, len(batch.Entries))}
	entries := batch.Entries
	if len(entries) == 0 {
		// Nothing to apply, but the caller is still authorised: a transaction that does only that.
		_, err := s.appendSome(ctx, by, nil)
		return out, err
	}
	for len(entries) > 0 {
		n, size := 0, 0
		for n < len(entries) && n < outputTxEntries && (n == 0 || size+len(entries[n].Data) <= outputTxBytes) {
			size += len(entries[n].Data)
			n++
		}
		acks, err := s.appendSome(ctx, by, entries[:n])
		if err != nil {
			return v1.OutputBatchResponse{}, err
		}
		out.Acks = append(out.Acks, acks...)
		entries = entries[n:]
	}
	return out, nil
}

func (s *Service) appendSome(ctx context.Context, by pki.Session, entries []v1.OutputEntry) ([]v1.OutputAck, error) {
	acks := make([]v1.OutputAck, 0, len(entries))
	err := s.db.WithTx(ctx, func(tx *store.Tx) error {
		acks = acks[:0]
		now := s.clock.Now()
		w, err := workerOfCaller(ctx, tx, by, now)
		if err != nil {
			return err
		}
		for _, e := range entries {
			ack, err := s.appendEntry(ctx, tx, w, now, e)
			if err != nil {
				return err
			}
			acks = append(acks, ack)
		}
		return nil
	})
	return acks, err
}

func (s *Service) appendEntry(ctx context.Context, tx *store.Tx, w store.Worker, now time.Time, e v1.OutputEntry) (v1.OutputAck, error) {
	ack := v1.OutputAck{RunID: e.RunID, LeaseEpoch: e.LeaseEpoch}
	refuse := func(err error) (v1.OutputAck, error) {
		var coded *domain.Error
		if !errors.As(err, &coded) || coded.Code == domain.CodeUnauthenticated {
			return v1.OutputAck{}, err
		}
		ack.Code, ack.Message = coded.Code, coded.Message
		return ack, nil
	}
	if err := validateOutputEntry(e); err != nil {
		return refuse(err)
	}
	if _, err := resolveLease(ctx, tx, w, domain.ID(e.RunID), e.LeaseEpoch); err != nil {
		return refuse(err)
	}
	var (
		o   store.Output
		err error
	)
	if e.EOF {
		o, err = tx.CloseOutput(ctx, domain.ID(e.RunID), e.LeaseEpoch, *e.FinalOffset, e.Note, now)
	} else {
		o, err = tx.AppendOutput(ctx, domain.ID(e.RunID), e.LeaseEpoch, e.Offset, e.Data, now)
	}
	if err != nil {
		return refuse(outputError(err))
	}
	ack.Start, ack.End, ack.Closed = o.Start, o.End, o.Closed
	return ack, nil
}

// outputError says, in the terms of the API, what the store refused.
func outputError(err error) error {
	switch {
	case errors.Is(err, store.ErrOutputClosed):
		return domain.NewError(domain.CodeInvalidTransition, "the output of this lease is closed", nil)
	case errors.Is(err, store.ErrOutputConflict), errors.Is(err, store.ErrOutputIncomplete), errors.Is(err, store.ErrOutputInvalid):
		return domain.NewError(domain.CodeInvalidRequest, err.Error(), nil)
	}
	return err
}

func validateOutputEntry(e v1.OutputEntry) error {
	switch {
	case !validRunID(e.RunID):
		return domain.NewError(domain.CodeNotFound, "lease not found", nil)
	case e.LeaseEpoch < 1:
		return input.Invalid("lease_epoch must be at least 1")
	case e.Offset > v1.OutputMaxOffset:
		return input.Invalid("offset is out of range")
	case e.EOF && (len(e.Data) > 0 || e.FinalOffset == nil):
		return input.Invalid("an eof entry has a final_offset and no data")
	case e.EOF && *e.FinalOffset > v1.OutputMaxOffset:
		return input.Invalid("final_offset is out of range")
	case !e.EOF && (e.FinalOffset != nil || e.Note != ""):
		return input.Invalid("final_offset and note belong to an eof entry")
	case !e.EOF && len(e.Data) == 0:
		return input.Invalid("an entry has data or is an eof")
	case len(e.Data) > v1.OutputChunkBytes:
		return input.Invalid("an entry carries at most %d bytes", v1.OutputChunkBytes)
	case uint64(len(e.Data)) > v1.OutputMaxOffset-e.Offset:
		return input.Invalid("offset and data are out of range")
	case len(e.Note) > maxOutputNoteLen || !utf8.ValidString(e.Note) || !domain.Printable(e.Note):
		return input.Invalid("note must print and be at most %d characters", maxOutputNoteLen)
	}
	return nil
}

func validRunID(id string) bool {
	kind, _, err := domain.ParseID(id)
	return err == nil && kind == domain.KindRun
}

// OutputRead is GET /v1/runs/{ref}/output without follow. Epoch 0 is the
// output of the Run as it is now. Tail asks for the last bytes, Offset for
// those from an absolute position; they exclude each other, and with neither
// the tail is v1.OutputDefaultTail. Limit bounds the bytes (a page at most).
type OutputRead struct {
	Ref       string
	Epoch     uint64
	Tail      uint64
	HasTail   bool
	Offset    uint64
	HasOffset bool
	Limit     uint64
}

func (q OutputRead) validate() error {
	switch {
	case q.HasTail && q.HasOffset:
		return input.Invalid("tail and offset exclude each other")
	case q.Tail > v1.OutputTailBytes:
		return input.Invalid("tail is at most %d bytes", v1.OutputTailBytes)
	case q.Offset > v1.OutputMaxOffset:
		return input.Invalid("offset is out of range")
	case q.Limit > v1.OutputPageBytes:
		return input.Invalid("limit is at most %d bytes", v1.OutputPageBytes)
	}
	return nil
}

// outputEpoch is the lease epoch whose output a reader means when it names
// none: the highest the Run has output for, or the one it is under now.
func outputEpoch(ctx context.Context, q store.Querier, run store.Run) (uint64, error) {
	latest, _, err := store.LatestOutputEpoch(ctx, q, run.ID)
	return max(latest, run.LeaseEpoch), err
}

// held is what the store has of an output, zero if nothing was received.
func held(ctx context.Context, q store.Querier, run domain.ID, epoch uint64) (store.Output, error) {
	if epoch == 0 {
		return store.Output{RunID: run}, nil
	}
	o, err := store.OutputOf(ctx, q, run, epoch)
	if errors.Is(err, store.ErrNotFound) {
		return store.Output{RunID: run, Epoch: epoch}, nil
	}
	return o, err
}

// runOf resolves a Run reference and authorises the caller, in one snapshot.
func (s *Service) runOf(ctx context.Context, q store.Querier, by pki.Session, ref string) (store.Run, error) {
	if _, err := pki.RequireCaller(ctx, q, by, s.clock.Now()); err != nil {
		return store.Run{}, err
	}
	id, err := refs.Run(ctx, q, ref)
	if err != nil {
		return store.Run{}, err
	}
	return store.RunByID(ctx, q, id)
}

// ReadOutput answers GET /v1/runs/{ref}/output: a page of the tail of the
// output the Controller holds. Truncated says that older output was asked for
// and the Controller no longer has it.
func (s *Service) ReadOutput(ctx context.Context, by pki.Session, q OutputRead) (v1.RunOutput, error) {
	if err := q.validate(); err != nil {
		return v1.RunOutput{}, err
	}
	limit := q.Limit
	if limit == 0 {
		limit = v1.OutputPageBytes
	}
	var out v1.RunOutput
	err := s.db.Read(ctx, func(r store.Querier) error {
		run, err := s.runOf(ctx, r, by, q.Ref)
		if err != nil {
			return err
		}
		epoch := q.Epoch
		if epoch == 0 {
			if epoch, err = outputEpoch(ctx, r, run); err != nil {
				return err
			}
		}
		o, err := held(ctx, r, run.ID, epoch)
		if err != nil {
			return err
		}
		out = v1.RunOutput{
			RunID: string(run.ID), LeaseEpoch: epoch, Start: o.Start, End: o.End, Data: []byte{}, Closed: o.Closed,
		}
		from, asked := q.Offset, q.HasOffset
		if !asked {
			tail := q.Tail
			if !q.HasTail {
				tail = v1.OutputDefaultTail
			}
			from = o.End - min(tail, o.End)
			out.Truncated = o.Start > 0 && tail > o.End-o.Start
		} else {
			out.Truncated = from < o.Start
		}
		from = min(max(from, o.Start), o.End)
		to := min(from+limit, o.End)
		out.Offset = from
		if to > from {
			if out.Data, err = store.OutputRange(ctx, r, run.ID, epoch, from, to); err != nil {
				return err
			}
		}
		return nil
	})
	return out, err
}

// OutputFollow is GET /v1/runs/{ref}/output?follow=true: the same selection as a
// read (Limit does not apply). The epoch is fixed when the follow opens, the one
// asked for or else the Run's current one; it starts at Offset, else at the last
// Tail bytes (v1.OutputDefaultTail by default).
type OutputFollow = OutputRead

// OutputFollower keeps delivering the output of one (Run, lease epoch).
type OutputFollower struct {
	s     *Service
	by    pki.Session
	run   domain.ID
	epoch uint64
	pos   uint64
	gap   bool
}

// OutputSink receives what a follow delivers. EOF and End are the last thing:
// the output is closed and delivered, or the follow gives up on it (see
// v1.EndNoMoreOutput, v1.EndNewerEpoch). Idle is called when it was quiet for
// stream.KeepaliveEvery.
type OutputSink struct {
	Chunk func(v1.OutputChunk) error
	EOF   func(v1.OutputEOF) error
	End   func(v1.OutputEnd) error
	Idle  func() error
}

// OpenOutputFollow validates a follow and fixes its epoch and start, before
// anything is sent. A position past what the Controller holds is a cursor that
// does not belong to this output.
func (s *Service) OpenOutputFollow(ctx context.Context, by pki.Session, q OutputFollow) (*OutputFollower, v1.OutputBegin, error) {
	if err := q.validate(); err != nil {
		return nil, v1.OutputBegin{}, err
	}
	f := &OutputFollower{s: s, by: by}
	var begin v1.OutputBegin
	err := s.db.Read(ctx, func(r store.Querier) error {
		run, err := s.runOf(ctx, r, by, q.Ref)
		if err != nil {
			return err
		}
		f.run, f.epoch = run.ID, q.Epoch
		if f.epoch == 0 {
			if f.epoch, err = outputEpoch(ctx, r, run); err != nil {
				return err
			}
		}
		o, err := held(ctx, r, run.ID, f.epoch)
		if err != nil {
			return err
		}
		begin = v1.OutputBegin{RunID: string(run.ID), LeaseEpoch: f.epoch, Start: o.Start, End: o.End}
		switch {
		case q.HasOffset && q.Offset > o.End:
			return domain.NewError(domain.CodeCursorExpired, "the offset is past the end of the output the Controller holds",
				map[string]any{"end": o.End})
		case q.HasOffset:
			f.pos = q.Offset
		default:
			tail := q.Tail
			if !q.HasTail {
				tail = v1.OutputDefaultTail
			}
			f.pos = o.End - min(tail, o.End)
		}
		begin.Offset = f.pos
		return nil
	})
	return f, begin, err
}

// Run delivers the output from where the follow stands until the output is
// closed and delivered (EOF), or it is clear that no more will come (End), or
// ctx ends, or the caller stops being valid, or a function of the sink fails.
//
// It looks, and only if there is nothing more waits, from the mark it read
// before looking, as the follow of events does. What was dropped from the
// Controller's tail before the follow got to it is delivered as a chunk with
// Gap set, at the offset where the bytes resume.
func (f *OutputFollower) Run(ctx context.Context, sink OutputSink) error {
	pacer := stream.NewPacer(f.s.clock, stream.KeepaliveEvery)
	notifier := f.s.db.OutputNotifier()
	quiet := false // the keepalive is due, to be sent once a look has found the caller valid
	for {
		seen := notifier.Latest()
		var l outputLook
		if err := f.s.db.Read(ctx, func(r store.Querier) (err error) {
			l, err = f.look(ctx, r)
			return err
		}); err != nil {
			return err
		}
		if l.chunk != nil {
			if err := sink.Chunk(*l.chunk); err != nil {
				return err
			}
			pacer.Wrote()
		} else if quiet {
			if err := sink.Idle(); err != nil {
				return err
			}
			pacer.Wrote()
		}
		switch {
		case l.more:
			continue
		case l.eof != nil:
			return sink.EOF(*l.eof)
		case l.end != nil:
			return sink.End(*l.end)
		}
		var err error
		// The state of the Run is not announced by this notifier: after a
		// keepalive, and when the Run is over, it is looked at again.
		if quiet, err = pacer.Wait(ctx, notifier.Changed(seen), l.waitFor); err != nil {
			return err
		}
	}
}

// outputLook is what one look at the output found.
type outputLook struct {
	chunk   *v1.OutputChunk
	more    bool // the chunk is not the end of what is held: look again at once
	eof     *v1.OutputEOF
	end     *v1.OutputEnd
	waitFor time.Duration // when the Run is over, how long until the follow may give up
}

func (f *OutputFollower) look(ctx context.Context, r store.Querier) (outputLook, error) {
	var l outputLook
	if _, err := pki.RequireCaller(ctx, r, f.by, f.s.clock.Now()); err != nil {
		return l, err
	}
	run, err := store.RunByID(ctx, r, f.run)
	if err != nil {
		return l, err
	}
	o, err := held(ctx, r, f.run, f.epoch)
	if err != nil {
		return l, err
	}
	if f.pos < o.Start {
		f.pos, f.gap = o.Start, true
	}
	if f.pos < o.End {
		to := min(f.pos+v1.OutputPageBytes, o.End)
		data, err := store.OutputRange(ctx, r, f.run, f.epoch, f.pos, to)
		if err != nil {
			return l, err
		}
		l.chunk = &v1.OutputChunk{RunID: string(f.run), LeaseEpoch: f.epoch, Offset: f.pos, Data: data, Gap: f.gap}
		f.pos, f.gap = to, false
		l.more = to < o.End
		if l.more {
			return l, nil
		}
	}
	switch newest, err := outputEpoch(ctx, r, run); {
	case err != nil:
		return l, err
	case o.Closed && f.pos == o.FinalOffset:
		l.eof = &v1.OutputEOF{RunID: string(f.run), LeaseEpoch: f.epoch, FinalOffset: o.FinalOffset}
	case newest > f.epoch:
		l.end = &v1.OutputEnd{RunID: string(f.run), Reason: v1.EndNewerEpoch, LeaseEpoch: newest}
	case run.State.IsTerminal() && run.Runtime != domain.RuntimeActive:
		since := o.UpdatedAt
		if run.FinishedAt != nil && run.FinishedAt.After(since) {
			since = *run.FinishedAt
		}
		if idle := f.s.clock.Now().Sub(since); idle >= noMoreOutputAfter {
			l.end = &v1.OutputEnd{RunID: string(f.run), Reason: v1.EndNoMoreOutput, LeaseEpoch: f.epoch}
		} else {
			l.waitFor = noMoreOutputAfter - idle
		}
	}
	return l, nil
}
