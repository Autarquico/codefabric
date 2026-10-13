package app_test

import (
	"bytes"
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	v1 "github.com/Autarquico/codefabric/internal/api/v1"
	"github.com/Autarquico/codefabric/internal/app/runs"
	"github.com/Autarquico/codefabric/internal/app/stream"
	"github.com/Autarquico/codefabric/internal/controller/pki"
	"github.com/Autarquico/codefabric/internal/controller/store"
	"github.com/Autarquico/codefabric/internal/domain"
)

// outputRun is a Run with an accepted lease on a Worker, the state in which
// the Worker sends its output.
type outputRun struct {
	run    v1.Run
	worker pki.Session
	lease  store.Lease
}

func (e *env) outputRun(name string) outputRun {
	e.t.Helper()
	w := e.joinWorker(name)
	run := e.queuedRun("task of " + name)
	l := e.offer(run.ID, w)
	if _, err := e.accept(w, l); err != nil {
		e.t.Fatal(err)
	}
	return outputRun{run: run, worker: w, lease: l}
}

func entry(o outputRun, offset uint64, text string) v1.OutputEntry {
	return v1.OutputEntry{RunID: o.run.ID, LeaseEpoch: o.lease.Epoch, Offset: offset, Data: []byte(text)}
}

func eofEntry(o outputRun, final uint64) v1.OutputEntry {
	return v1.OutputEntry{RunID: o.run.ID, LeaseEpoch: o.lease.Epoch, EOF: true, FinalOffset: &final}
}

func (e *env) send(w pki.Session, entries ...v1.OutputEntry) []v1.OutputAck {
	e.t.Helper()
	out, err := e.app.Runs.AppendOutput(e.t.Context(), w, v1.OutputBatch{Entries: entries})
	must(e.t, err)
	if len(out.Acks) != len(entries) {
		e.t.Fatalf("%d acks for %d entries", len(out.Acks), len(entries))
	}
	return out.Acks
}

func (e *env) readOutput(q runs.OutputRead) v1.RunOutput {
	e.t.Helper()
	out, err := e.app.Runs.ReadOutput(e.t.Context(), e.owner, q)
	must(e.t, err)
	return out
}

func TestOutput_AWorkerAppendsToItsLeaseAndTheClientReadsItBack(t *testing.T) {
	e := newEnv(t)
	o := e.outputRun("alpha")
	acks := e.send(o.worker, entry(o, 0, "hello "), entry(o, 6, "world"))
	if acks[1].Code != "" || acks[1].Start != 0 || acks[1].End != 11 || acks[1].Closed {
		t.Fatalf("ack = %+v", acks[1])
	}

	got := e.readOutput(runs.OutputRead{Ref: o.run.Short})
	if string(got.Data) != "hello world" || got.Start != 0 || got.End != 11 || got.Offset != 0 || got.Truncated || got.Closed || got.LeaseEpoch != 1 {
		t.Fatalf("read = %+v", got)
	}
	if got = e.readOutput(runs.OutputRead{Ref: o.run.ID, HasTail: true, Tail: 5}); string(got.Data) != "world" || got.Offset != 6 {
		t.Fatalf("tail 5 = %+v", got)
	}
	if got = e.readOutput(runs.OutputRead{Ref: o.run.ID, HasOffset: true, Offset: 3, Limit: 4}); string(got.Data) != "lo w" || got.Offset != 3 {
		t.Fatalf("offset 3 limit 4 = %+v", got)
	}
	if got = e.readOutput(runs.OutputRead{Ref: o.run.ID, HasOffset: true, Offset: 99}); len(got.Data) != 0 || got.Offset != 11 {
		t.Fatalf("offset past the end = %+v", got)
	}
	// Sent again, nothing changes.
	if acks = e.send(o.worker, entry(o, 0, "hello world")); acks[0].End != 11 || acks[0].Code != "" {
		t.Fatalf("repeat = %+v", acks[0])
	}
}

func TestOutput_ARunWithoutOutputReadsEmpty(t *testing.T) {
	e := newEnv(t)
	run := e.queuedRun("x")
	got := e.readOutput(runs.OutputRead{Ref: run.Short})
	if got.LeaseEpoch != 0 || got.End != 0 || len(got.Data) != 0 || got.Closed || got.Data == nil {
		t.Fatalf("read = %+v", got)
	}
	_, err := e.app.Runs.ReadOutput(t.Context(), e.owner, runs.OutputRead{Ref: "run-99"})
	wantCode(t, err, domain.CodeNotFound)
}

func TestOutput_ReadsAreValidated(t *testing.T) {
	e := newEnv(t)
	o := e.outputRun("alpha")
	for name, q := range map[string]runs.OutputRead{
		"tail and offset": {Ref: o.run.ID, HasTail: true, HasOffset: true},
		"tail too big":    {Ref: o.run.ID, HasTail: true, Tail: v1.OutputTailBytes + 1},
		"offset too big":  {Ref: o.run.ID, HasOffset: true, Offset: v1.OutputMaxOffset + 1},
		"limit too big":   {Ref: o.run.ID, Limit: v1.OutputPageBytes + 1},
	} {
		if _, err := e.app.Runs.ReadOutput(t.Context(), e.owner, q); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

// The authorisation matrix of POST /v1/worker/output, entry by entry.
func TestOutput_EachEntryIsAuthorisedAgainstTheLeaseItNames(t *testing.T) {
	e := newEnv(t)
	o := e.outputRun("alpha")
	other := e.outputRun("beta")

	foreign := entry(o, 0, "stolen") // beta sends to alpha's lease
	stale := entry(o, 0, "stale")    // an epoch alpha never had
	stale.LeaseEpoch = 7
	unknownRun := entry(o, 0, "x") // not a Run at all
	unknownRun.RunID = "task_01J8ZQ4M5N6P7R8S9T0V1W2X3Y"
	acks := e.send(other.worker, foreign, stale, unknownRun, entry(other, 0, "mine"))
	for i, want := range []domain.Code{domain.CodeNotFound, domain.CodeNotFound, domain.CodeNotFound, ""} {
		if acks[i].Code != want {
			t.Errorf("entry %d: code %q, want %q (%+v)", i, acks[i].Code, want, acks[i])
		}
	}
	for _, refused := range acks[:3] {
		if refused.End != 0 || refused.Closed || refused.Start != 0 {
			t.Errorf("a refused entry carries data: %+v", refused)
		}
	}
	if got := e.readOutput(runs.OutputRead{Ref: o.run.ID}); len(got.Data) != 0 {
		t.Fatalf("a foreign lease stored %q", got.Data)
	}
	if got := e.readOutput(runs.OutputRead{Ref: other.run.ID}); string(got.Data) != "mine" {
		t.Fatalf("the entry after the refused ones was not stored: %q", got.Data)
	}

	// A client is not a Worker: the whole request fails.
	_, err := e.app.Runs.AppendOutput(t.Context(), e.owner, v1.OutputBatch{Entries: []v1.OutputEntry{entry(o, 0, "x")}})
	wantCode(t, err, domain.CodeUnauthenticated)
}

func TestOutput_ATerminalRunStillTakesItsOutputButAnArchivedTaskDoesNot(t *testing.T) {
	e := newEnv(t)
	o := e.outputRun("alpha")
	e.send(o.worker, entry(o, 0, "before"))
	_, err := e.app.Tasks.Cancel(t.Context(), e.owner, o.run.TaskID, "", v1.ActionRequest{})
	must(t, err)
	if r := e.storedRun(o.run.ID); !r.State.IsTerminal() {
		t.Fatalf("run is %s", r.State)
	}
	if acks := e.send(o.worker, entry(o, 6, " after")); acks[0].Code != "" || acks[0].End != 12 {
		t.Fatalf("a terminal Run refused its output: %+v", acks[0])
	}
	_, err = e.app.Tasks.Archive(t.Context(), e.owner, o.run.TaskID, "", v1.ActionRequest{})
	must(t, err)
	if acks := e.send(o.worker, entry(o, 12, "!")); acks[0].Code != domain.CodeNotFound {
		t.Fatalf("an archived Task took output: %+v", acks[0])
	}
}

func TestOutput_ContradictionsAreRefusedPerEntryAndTheRestStands(t *testing.T) {
	e := newEnv(t)
	o := e.outputRun("alpha")
	e.send(o.worker, entry(o, 0, "abcdef"))
	acks := e.send(o.worker, entry(o, 2, "XX"), entry(o, 6, "ghi"))
	if acks[0].Code != domain.CodeInvalidRequest || acks[1].Code != "" || acks[1].End != 9 {
		t.Fatalf("acks = %+v", acks)
	}
	if got := e.readOutput(runs.OutputRead{Ref: o.run.ID}); string(got.Data) != "abcdefghi" {
		t.Fatalf("read %q", got.Data)
	}
}

func TestOutput_EntriesAreValidated(t *testing.T) {
	e := newEnv(t)
	o := e.outputRun("alpha")
	final := uint64(5)
	overflow := v1.OutputMaxOffset + 1
	for name, bad := range map[string]v1.OutputEntry{
		"epoch zero":       {RunID: o.run.ID, LeaseEpoch: 0, Data: []byte("x")},
		"no data":          {RunID: o.run.ID, LeaseEpoch: 1},
		"data and eof":     {RunID: o.run.ID, LeaseEpoch: 1, Data: []byte("x"), EOF: true, FinalOffset: &final},
		"eof without end":  {RunID: o.run.ID, LeaseEpoch: 1, EOF: true},
		"final on data":    {RunID: o.run.ID, LeaseEpoch: 1, Data: []byte("x"), FinalOffset: &final},
		"note on data":     {RunID: o.run.ID, LeaseEpoch: 1, Data: []byte("x"), Note: "n"},
		"chunk too big":    {RunID: o.run.ID, LeaseEpoch: 1, Data: make([]byte, v1.OutputChunkBytes+1)},
		"offset overflow":  {RunID: o.run.ID, LeaseEpoch: 1, Offset: overflow, Data: []byte("x")},
		"sum overflow":     {RunID: o.run.ID, LeaseEpoch: 1, Offset: v1.OutputMaxOffset, Data: []byte("x")},
		"final overflow":   {RunID: o.run.ID, LeaseEpoch: 1, EOF: true, FinalOffset: &overflow},
		"note too long":    {RunID: o.run.ID, LeaseEpoch: 1, EOF: true, FinalOffset: &final, Note: strings.Repeat("n", 257)},
		"note unprintable": {RunID: o.run.ID, LeaseEpoch: 1, EOF: true, FinalOffset: &final, Note: "a\x1bb"},
	} {
		if acks := e.send(o.worker, bad); acks[0].Code != domain.CodeInvalidRequest {
			t.Errorf("%s: %+v", name, acks[0])
		}
	}
	_, err := e.app.Runs.AppendOutput(t.Context(), o.worker, v1.OutputBatch{Entries: make([]v1.OutputEntry, v1.OutputBatchMaxItems+1)})
	wantCode(t, err, domain.CodeInvalidRequest)
	if got := e.readOutput(runs.OutputRead{Ref: o.run.ID}); got.End != 0 {
		t.Fatalf("an invalid entry stored something: %+v", got)
	}
}

func TestOutput_TheEOFClosesItOnceTheLastBytesAreIn(t *testing.T) {
	e := newEnv(t)
	o := e.outputRun("alpha")
	e.send(o.worker, entry(o, 0, "done"))
	if acks := e.send(o.worker, eofEntry(o, 9)); acks[0].Code != domain.CodeInvalidRequest {
		t.Fatalf("closing past what was received: %+v", acks[0])
	}
	if acks := e.send(o.worker, eofEntry(o, 4)); acks[0].Code != "" || !acks[0].Closed {
		t.Fatalf("eof = %+v", acks[0])
	}
	if acks := e.send(o.worker, eofEntry(o, 4)); acks[0].Code != "" || !acks[0].Closed {
		t.Fatalf("eof again = %+v", acks[0])
	}
	if acks := e.send(o.worker, entry(o, 4, "more")); acks[0].Code != domain.CodeInvalidTransition {
		t.Fatalf("an append after the eof: %+v", acks[0])
	}
	if acks := e.send(o.worker, eofEntry(o, 5)); acks[0].Code != domain.CodeInvalidRequest {
		t.Fatalf("an eof elsewhere: %+v", acks[0])
	}
	if got := e.readOutput(runs.OutputRead{Ref: o.run.ID}); !got.Closed || string(got.Data) != "done" {
		t.Fatalf("read = %+v", got)
	}
}

func TestOutput_TwoEpochsOfARunKeepTheirOwnBytesAtTheSameOffsets(t *testing.T) {
	e := newEnv(t)
	o := e.outputRun("alpha")
	e.send(o.worker, entry(o, 0, "first"))
	// A later epoch of the same Run, written through the store as a new lease would have.
	must(t, e.db.WithTx(t.Context(), func(tx *store.Tx) error {
		_, err := tx.AppendOutput(t.Context(), domain.ID(o.run.ID), 2, 0, []byte("second"), e.clock.Now())
		return err
	}))
	if got := e.readOutput(runs.OutputRead{Ref: o.run.ID}); got.LeaseEpoch != 2 || string(got.Data) != "second" {
		t.Fatalf("the latest epoch: %+v", got)
	}
	if got := e.readOutput(runs.OutputRead{Ref: o.run.ID, Epoch: 1}); got.LeaseEpoch != 1 || string(got.Data) != "first" {
		t.Fatalf("epoch 1: %+v", got)
	}
}

// outputSeen is what a follow of output delivered.
type outputSeen struct {
	mu      sync.Mutex
	data    bytes.Buffer
	gaps    []uint64
	eof     *v1.OutputEOF
	end     *v1.OutputEnd
	idles   int
	err     error
	stopped chan struct{}
	cancel  context.CancelFunc
}

func (s *outputSeen) text() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.data.String()
}

func (s *outputSeen) done() (*v1.OutputEOF, *v1.OutputEnd) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.eof, s.end
}

func (e *env) followOutput(q runs.OutputFollow) (*outputSeen, v1.OutputBegin) {
	e.t.Helper()
	f, begin, err := e.app.Runs.OpenOutputFollow(e.t.Context(), e.owner, q)
	must(e.t, err)
	ctx, cancel := context.WithCancel(e.t.Context())
	e.t.Cleanup(cancel)
	s := &outputSeen{stopped: make(chan struct{}), cancel: cancel}
	go func() {
		defer close(s.stopped)
		err := f.Run(ctx, runs.OutputSink{
			Chunk: func(c v1.OutputChunk) error {
				s.mu.Lock()
				defer s.mu.Unlock()
				if c.Gap {
					s.gaps = append(s.gaps, c.Offset)
				}
				s.data.Write(c.Data)
				return nil
			},
			EOF: func(x v1.OutputEOF) error {
				s.mu.Lock()
				defer s.mu.Unlock()
				s.eof = &x
				return nil
			},
			End: func(x v1.OutputEnd) error {
				s.mu.Lock()
				defer s.mu.Unlock()
				s.end = &x
				return nil
			},
			Idle: func() error {
				s.mu.Lock()
				defer s.mu.Unlock()
				s.idles++
				return nil
			},
		})
		s.mu.Lock()
		s.err = err
		s.mu.Unlock()
	}()
	return s, begin
}

func TestOutputFollow_DeliversTheTailThenWhatArrivesThenTheEOF(t *testing.T) {
	e := newEnv(t)
	o := e.outputRun("alpha")
	e.send(o.worker, entry(o, 0, "line one\n"))

	s, begin := e.followOutput(runs.OutputFollow{Ref: o.run.Short})
	if begin.LeaseEpoch != 1 || begin.Start != 0 || begin.End != 9 || begin.RunID != o.run.ID {
		t.Fatalf("begin = %+v", begin)
	}
	waitUntil(t, "the tail", func() bool { return s.text() == "line one\n" })
	e.send(o.worker, entry(o, 9, "line two\n"))
	waitUntil(t, "live output", func() bool { return s.text() == "line one\nline two\n" })

	e.send(o.worker, eofEntry(o, 18))
	<-s.stopped
	if eof, end := s.done(); eof == nil || eof.FinalOffset != 18 || end != nil || s.err != nil {
		t.Fatalf("eof = %+v, end = %+v, err = %v", eof, end, s.err)
	}
}

func TestOutputFollow_StartsWhereAskedAndFlagsWhatTheRingDropped(t *testing.T) {
	e := newEnv(t)
	o := e.outputRun("alpha")
	e.send(o.worker, entry(o, 0, "0123456789"))

	s, _ := e.followOutput(runs.OutputFollow{Ref: o.run.ID, HasOffset: true, Offset: 4})
	waitUntil(t, "from offset 4", func() bool { return s.text() == "456789" })

	if _, _, err := e.app.Runs.OpenOutputFollow(t.Context(), e.owner, runs.OutputFollow{Ref: o.run.ID, HasOffset: true, Offset: 99}); err == nil {
		t.Fatal("a position past the end was accepted")
	} else {
		wantCode(t, err, domain.CodeCursorExpired)
	}
	tail, _ := e.followOutput(runs.OutputFollow{Ref: o.run.ID, HasTail: true, Tail: 3})
	waitUntil(t, "tail 3", func() bool { return tail.text() == "789" })

	// The Worker skips ahead, so what the follow had not reached is gone: it is told.
	late, _ := e.followOutput(runs.OutputFollow{Ref: o.run.ID, HasOffset: true, Offset: 0})
	waitUntil(t, "all of it", func() bool { return late.text() == "0123456789" })
	e.send(o.worker, entry(o, 5000, "after the jump"))
	waitUntil(t, "after the jump", func() bool { return strings.HasSuffix(late.text(), "after the jump") })
	late.mu.Lock()
	gaps := late.gaps
	late.mu.Unlock()
	if len(gaps) != 1 || gaps[0] != 5000 {
		t.Fatalf("gaps = %v, want one at 5000", gaps)
	}
}

func TestOutputFollow_AnotherEpochEndsItWithNewerEpoch(t *testing.T) {
	e := newEnv(t)
	o := e.outputRun("alpha")
	e.send(o.worker, entry(o, 0, "one"))
	s, _ := e.followOutput(runs.OutputFollow{Ref: o.run.ID})
	waitUntil(t, "the output", func() bool { return s.text() == "one" })

	must(t, e.db.WithTx(t.Context(), func(tx *store.Tx) error {
		_, err := tx.AppendOutput(t.Context(), domain.ID(o.run.ID), 2, 0, []byte("two"), e.clock.Now())
		return err
	}))
	<-s.stopped
	if eof, end := s.done(); eof != nil || end == nil || end.Reason != v1.EndNewerEpoch || end.LeaseEpoch != 2 {
		t.Fatalf("eof = %+v, end = %+v", eof, end)
	}
	// The follow stayed on its epoch: it did not deliver the other's bytes.
	if s.text() != "one" {
		t.Fatalf("delivered %q", s.text())
	}
}

func TestOutputFollow_ARunThatIsOverEndsWithNoMoreOutputAfterThirtySeconds(t *testing.T) {
	e := newEnv(t)
	o := e.outputRun("alpha")
	e.send(o.worker, entry(o, 0, "last words"))
	s, _ := e.followOutput(runs.OutputFollow{Ref: o.run.ID})
	waitUntil(t, "the output", func() bool { return s.text() == "last words" })

	// The Run ends and its session is gone, but the Worker never closes the output.
	_, err := e.app.Tasks.Cancel(t.Context(), e.owner, o.run.TaskID, "", v1.ActionRequest{})
	must(t, err)
	must(t, e.db.WithTx(t.Context(), func(tx *store.Tx) error {
		_, err := tx.Exec(t.Context(), `UPDATE runs SET runtime = 'stopped' WHERE id = ?`, o.run.ID)
		return err
	}))
	e.clock.Advance(stream.KeepaliveEvery) // the follow looks again after a keepalive
	waitUntil(t, "the keepalive", func() bool { s.mu.Lock(); defer s.mu.Unlock(); return s.idles >= 1 })
	if _, end := s.done(); end != nil {
		t.Fatalf("ended after %s: %+v", stream.KeepaliveEvery, end)
	}
	e.clock.Advance(20 * time.Second)
	<-s.stopped
	if _, end := s.done(); end == nil || end.Reason != v1.EndNoMoreOutput {
		t.Fatalf("end = %+v", end)
	}
}

func TestOutputFollow_ARunWhoseSessionIsStillActiveIsNotGivenUpOn(t *testing.T) {
	e := newEnv(t)
	o := e.outputRun("alpha")
	e.send(o.worker, entry(o, 0, "x"))
	s, _ := e.followOutput(runs.OutputFollow{Ref: o.run.ID})
	waitUntil(t, "the output", func() bool { return s.text() == "x" })
	must(t, e.db.WithTx(t.Context(), func(tx *store.Tx) error {
		_, err := tx.Exec(t.Context(), `UPDATE runs SET runtime = 'active' WHERE id = ?`, o.run.ID)
		return err
	}))
	_, err := e.app.Tasks.Cancel(t.Context(), e.owner, o.run.TaskID, "", v1.ActionRequest{})
	must(t, err)
	e.clock.Advance(time.Hour)
	time.Sleep(50 * time.Millisecond)
	if eof, end := s.done(); eof != nil || end != nil {
		t.Fatalf("gave up on a Run with an active session: %+v %+v", eof, end)
	}
}

func TestOutputFollow_KeepsAliveAndEndsWithItsCaller(t *testing.T) {
	e := newEnv(t)
	o := e.outputRun("alpha")
	laptop := e.newClient("laptop")
	f, _, err := e.app.Runs.OpenOutputFollow(t.Context(), laptop, runs.OutputFollow{Ref: o.run.ID})
	must(t, err)
	var mu sync.Mutex
	idles := 0
	done := make(chan error, 1)
	go func() {
		done <- f.Run(t.Context(), runs.OutputSink{
			Chunk: func(v1.OutputChunk) error { return nil },
			EOF:   func(v1.OutputEOF) error { return nil },
			End:   func(v1.OutputEnd) error { return nil },
			Idle:  func() error { mu.Lock(); idles++; mu.Unlock(); return nil },
		})
	}()
	must(t, e.blockUntil(1))
	e.clock.Advance(stream.KeepaliveEvery)
	waitUntil(t, "a keepalive", func() bool { mu.Lock(); defer mu.Unlock(); return idles == 1 })

	must(t, e.authority.RevokeClient(t.Context(), e.owner, laptop.Principal.ID))
	e.send(o.worker, entry(o, 0, "wakes it"))
	select {
	case err := <-done:
		wantCode(t, err, domain.CodeUnauthenticated)
	case <-time.After(5 * time.Second):
		t.Fatal("the follow outlived its caller")
	}
}

func TestOutputFollow_ASlowReaderGetsEverythingInPages(t *testing.T) {
	e := newEnv(t)
	o := e.outputRun("alpha")
	var want strings.Builder
	for i := range 30 {
		chunk := fmt.Sprintf("%02d", i) + strings.Repeat("z", v1.OutputChunkBytes-2)
		e.send(o.worker, entry(o, uint64(i)*v1.OutputChunkBytes, chunk)) //nolint:gosec // small
		want.WriteString(chunk)
	}
	// 30 chunks of 64 KiB are 1.9 MiB: the ring keeps the last MiB.
	s, begin := e.followOutput(runs.OutputFollow{Ref: o.run.ID, HasOffset: true, Offset: 0})
	if begin.Start == 0 || begin.End != 30*v1.OutputChunkBytes {
		t.Fatalf("begin = %+v", begin)
	}
	waitUntil(t, "the retained tail", func() bool { return len(s.text()) == int(begin.End-begin.Start) }) //nolint:gosec // bounded
	if s.text() != want.String()[begin.Start:] {
		t.Fatal("the bytes delivered are not the bytes sent")
	}
}

// A big batch goes in transactions of its own, in order; every entry is still answered, and the order is kept.
func TestOutput_ABigBatchIsAppliedInSeveralTransactionsInOrder(t *testing.T) {
	e := newEnv(t)
	o := e.outputRun("alpha")
	var entries []v1.OutputEntry
	var want strings.Builder
	offset := uint64(0)
	for i := range 100 { // more than 32 entries
		text := fmt.Sprintf("%03d|", i)
		entries = append(entries, entry(o, offset, text))
		want.WriteString(text)
		offset += uint64(len(text))
	}
	for i := range 6 { // and more than 256 KiB of data
		chunk := strings.Repeat(string(rune('a'+i)), v1.OutputChunkBytes)
		entries = append(entries, entry(o, offset, chunk))
		want.WriteString(chunk)
		offset += uint64(len(chunk))
	}
	acks := e.send(o.worker, entries...)
	for i, ack := range acks {
		if ack.Code != "" || ack.End != entries[i].Offset+uint64(len(entries[i].Data)) {
			t.Fatalf("ack %d = %+v", i, ack)
		}
	}
	if got := e.readOutput(runs.OutputRead{Ref: o.run.ID, HasOffset: true, Offset: 0, Limit: 400}); string(got.Data[:8]) != "000|001|" {
		t.Fatalf("order lost: %q", got.Data)
	}
	if got := e.readOutput(runs.OutputRead{Ref: o.run.ID}); got.End != uint64(want.Len()) { //nolint:gosec // a length
		t.Fatalf("end %d, want %d", got.End, want.Len())
	}
}
