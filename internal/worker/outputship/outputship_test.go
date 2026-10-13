package outputship_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	v1 "github.com/Autarquico/codefabric/internal/api/v1"
	"github.com/Autarquico/codefabric/internal/config"
	"github.com/Autarquico/codefabric/internal/domain"
	"github.com/Autarquico/codefabric/internal/support/clock"
	"github.com/Autarquico/codefabric/internal/worker/execwrap"
	"github.com/Autarquico/codefabric/internal/worker/outputship"
	"github.com/Autarquico/codefabric/internal/worker/state"
)

var t0 = time.Date(2026, 10, 10, 12, 0, 0, 0, time.UTC)

// controller is a stand-in for the Controller's output store: it keeps what it
// was sent, as the ring does (contiguous from where the first entry began).
type controller struct {
	batches [][]v1.OutputEntry
	outputs map[state.Key]*held
	refuse  map[string]domain.Code // by run: a code every entry of it gets
	fail    error
}

type held struct {
	start, end uint64
	data       []byte
	closed     bool
}

func (c *controller) UploadOutput(_ context.Context, b v1.OutputBatch) (v1.OutputBatchResponse, error) {
	if c.fail != nil {
		return v1.OutputBatchResponse{}, c.fail
	}
	c.batches = append(c.batches, b.Entries)
	var out v1.OutputBatchResponse
	for _, e := range b.Entries {
		ack := v1.OutputAck{RunID: e.RunID, LeaseEpoch: e.LeaseEpoch}
		if code, ok := c.refuse[e.RunID]; ok {
			ack.Code = code
			out.Acks = append(out.Acks, ack)
			continue
		}
		k := state.Key{RunID: e.RunID, LeaseEpoch: e.LeaseEpoch}
		h := c.outputs[k]
		if h == nil {
			h = &held{start: e.Offset, end: e.Offset}
			c.outputs[k] = h
		}
		switch {
		case e.EOF:
			h.closed = true
		case e.Offset > h.end:
			h.start, h.end, h.data = e.Offset, e.Offset+uint64(len(e.Data)), append([]byte(nil), e.Data...)
		case e.Offset+uint64(len(e.Data)) > h.end:
			h.data = append(h.data, e.Data[h.end-e.Offset:]...)
			h.end = e.Offset + uint64(len(e.Data))
		}
		ack.Start, ack.End, ack.Closed = h.start, h.end, h.closed
		out.Acks = append(out.Acks, ack)
	}
	return out, nil
}

type fixture struct {
	t     *testing.T
	db    *state.DB
	paths config.Paths
	clk   *clock.Fake
	ctl   *controller
	ship  *outputship.Shipper
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	root := t.TempDir()
	f := &fixture{
		t: t, paths: config.Paths{ConfigDir: filepath.Join(root, "config"), DataDir: filepath.Join(root, "data")},
		clk: clock.NewFake(t0), ctl: &controller{outputs: map[state.Key]*held{}, refuse: map[string]domain.Code{}},
	}
	db, err := state.Open(t.Context(), f.paths.WorkerStateFile())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	f.db = db
	f.ship = outputship.New(outputship.Options{DB: db, Uploader: f.ctl, Paths: f.paths, Clock: f.clk, Log: slog.New(slog.DiscardHandler)})
	return f
}

// run is a Run of this Worker in a phase, with a run directory.
func (f *fixture) run(id string, phase state.Phase) state.Key {
	f.t.Helper()
	k := state.Key{RunID: id, LeaseEpoch: 1}
	r := state.Run{
		Key: k, LeaseID: "lease_" + id, RunShort: "run-1", TaskID: "task_1", ProjectID: "project_1", CommandSeq: 1, AcceptBy: t0.Add(time.Minute),
		SourceCommit: "abc", CPU: 1, MemoryMB: 1, Spec: []byte(`{}`), Harness: "command", Mode: "headless", CreatedAt: t0,
	}
	if err := f.db.WithTx(f.t.Context(), func(tx *state.Tx) error {
		if _, err := state.InsertAssigned(f.t.Context(), tx, r); err != nil {
			return err
		}
		return f.setPhase(tx, k, phase)
	}); err != nil {
		f.t.Fatal(err)
	}
	if err := os.MkdirAll(f.paths.WorkerRunDir(id, 1), 0o700); err != nil {
		f.t.Fatal(err)
	}
	return k
}

// setPhase walks the assignment from assigned to the phase, by the steps the executor takes.
func (f *fixture) setPhase(tx *state.Tx, k state.Key, phase state.Phase) error {
	ctx := f.t.Context()
	steps := map[state.Phase][]func() error{
		state.PhaseAssigned: nil,
		state.PhaseAccepted: {func() error { return state.Advance(ctx, tx, k, state.PhaseAssigned, state.PhaseAccepted, t0) }},
	}
	toRunning := []func() error{
		func() error { return state.Advance(ctx, tx, k, state.PhaseAssigned, state.PhaseAccepted, t0) },
		func() error { return state.Advance(ctx, tx, k, state.PhaseAccepted, state.PhaseStarting, t0) },
		func() error { return state.SetRunning(ctx, tx, k, `{"runtime":"tmux","ref":"x/y"}`, t0) },
	}
	steps[state.PhaseRunning] = toRunning
	steps[state.PhaseStopping] = append(toRunning[:3:3], func() error { return state.Advance(ctx, tx, k, state.PhaseRunning, state.PhaseStopping, t0) })
	for _, step := range steps[phase] {
		if err := step(); err != nil {
			return err
		}
	}
	if phase == state.PhaseStopped {
		return state.Close(ctx, tx, k, state.LivePhases, state.PhaseStopped, false, t0)
	}
	return nil
}

func (f *fixture) phase(k state.Key, phase state.Phase) {
	f.t.Helper()
	if err := f.db.WithTx(f.t.Context(), func(tx *state.Tx) error {
		if phase == state.PhaseStopped {
			return state.Close(f.t.Context(), tx, k, state.LivePhases, state.PhaseStopped, false, t0)
		}
		return f.setPhase(tx, k, phase)
	}); err != nil {
		f.t.Fatal(err)
	}
}

func (f *fixture) logPath(k state.Key) string {
	return filepath.Join(f.paths.WorkerRunDir(k.RunID, k.LeaseEpoch), execwrap.OutputLog)
}

func (f *fixture) write(k state.Key, text string) {
	f.t.Helper()
	file, err := os.OpenFile(f.logPath(k), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		f.t.Fatal(err)
	}
	defer func() { _ = file.Close() }()
	if _, err := file.WriteString(text); err != nil {
		f.t.Fatal(err)
	}
}

func (f *fixture) once() {
	f.t.Helper()
	if err := f.ship.Once(f.t.Context()); err != nil {
		f.t.Fatal(err)
	}
}

func (f *fixture) cursor(k state.Key) state.OutputCursor {
	f.t.Helper()
	c, err := state.OutputCursorOf(f.t.Context(), f.db.Reader(), k)
	if err != nil {
		f.t.Fatal(err)
	}
	return c
}

func (f *fixture) sent(k state.Key) string {
	h := f.ctl.outputs[k]
	if h == nil {
		return ""
	}
	return string(h.data)
}

func TestShip_SendsNewBytesFromWhereItLeftOffAndOnlyWhenThereAreNew(t *testing.T) {
	f := newFixture(t)
	k := f.run("run_a", state.PhaseRunning)
	f.once()
	if len(f.ctl.batches) != 0 {
		t.Fatal("sent something with no log")
	}
	f.write(k, "hello ")
	f.once()
	f.write(k, "world")
	f.once()
	f.once() // nothing new: no request
	if f.sent(k) != "hello world" || len(f.ctl.batches) != 2 {
		t.Fatalf("sent %q in %d requests", f.sent(k), len(f.ctl.batches))
	}
	if b := f.ctl.batches[1][0]; b.Offset != 6 || string(b.Data) != "world" {
		t.Fatalf("second request = %+v", b)
	}
	if c := f.cursor(k); c.Sent != 11 || !c.HasFile || c.Done {
		t.Fatalf("cursor = %+v", c)
	}
}

func TestShip_SendsTheOutputOfSeveralRunsInOneRequest(t *testing.T) {
	f := newFixture(t)
	a, b := f.run("run_a", state.PhaseRunning), f.run("run_b", state.PhaseStopping)
	f.write(a, "from a")
	f.write(b, "from b")
	f.once()
	if len(f.ctl.batches) != 1 || len(f.ctl.batches[0]) != 2 || f.sent(a) != "from a" || f.sent(b) != "from b" {
		t.Fatalf("batches = %+v", f.ctl.batches)
	}
	// A Run that has not started, or whose assignment is over without a process, has no output to send.
	f.run("run_c", state.PhaseAccepted)
	f.write(state.Key{RunID: "run_c", LeaseEpoch: 1}, "not yet")
	f.once()
	if len(f.ctl.batches) != 1 {
		t.Fatal("sent the log of a Run that has not started")
	}
}

func TestShip_DoesNotSendWhatTheRingWouldDropAndBoundsABatch(t *testing.T) {
	f := newFixture(t)
	k := f.run("run_a", state.PhaseRunning)
	big := bytes.Repeat([]byte("x"), 3*v1.OutputTailBytes)
	big[len(big)-1] = 'Z'
	f.write(k, string(big))
	f.once()
	first := f.ctl.batches[0][0]
	if first.Offset != uint64(len(big)-v1.OutputTailBytes) { //nolint:gosec // a small length
		t.Fatalf("started at %d, want the last MiB (%d)", first.Offset, len(big)-v1.OutputTailBytes)
	}
	total := 0
	for _, e := range f.ctl.batches[0] {
		total += len(e.Data)
		if len(e.Data) > v1.OutputChunkBytes {
			t.Fatalf("a chunk of %d bytes", len(e.Data))
		}
	}
	if total > 1<<20 || total < v1.OutputChunkBytes {
		t.Fatalf("a batch of %d bytes", total)
	}
	// It catches up in passes, one request each.
	for range 20 {
		f.clk.Advance(time.Second)
		f.once()
	}
	if c := f.cursor(k); c.Sent != uint64(len(big)) {
		t.Fatalf("sent up to %d of %d", c.Sent, len(big))
	}
}

func TestShip_AReplacedOrTruncatedLogIsNotReadAgainAndTheOutputIsClosedWithANote(t *testing.T) {
	for name, tamper := range map[string]func(path string){
		"replaced": func(path string) {
			_ = os.Remove(path)
			_ = os.WriteFile(path, []byte("a completely different and longer log\n"), 0o600)
		},
		"truncated": func(path string) { _ = os.Truncate(path, 2) },
		// The inode of a file removed and made again is reused at once on Linux; here
		// the same thing is made certain: other bytes, longer than what was sent, in
		// the very same file (same device, same inode).
		"rewritten in place": func(path string) {
			_ = os.WriteFile(path, []byte("a completely different and longer log\n"), 0o600)
		},
	} {
		f := newFixture(t)
		k := f.run("run_a", state.PhaseRunning)
		f.write(k, "original output")
		f.once()
		tamper(f.logPath(k))
		f.once()
		last := f.ctl.batches[len(f.ctl.batches)-1]
		e := last[len(last)-1]
		if !e.EOF || e.FinalOffset == nil || *e.FinalOffset != 15 || e.Note == "" || len(last) != 1 {
			t.Errorf("%s: last request = %+v", name, last)
		}
		if f.sent(k) != "original output" || !f.cursor(k).Done {
			t.Errorf("%s: sent %q, cursor %+v", name, f.sent(k), f.cursor(k))
		}
		before := len(f.ctl.batches)
		f.write(k, "more")
		f.once()
		if len(f.ctl.batches) != before {
			t.Errorf("%s: sent after closing", name)
		}
	}
}

func TestShip_ASymlinkOrAPipeInPlaceOfTheLogIsNeverRead(t *testing.T) {
	secret := filepath.Join(t.TempDir(), "secret")
	if err := os.WriteFile(secret, []byte("the daemon's secrets"), 0o600); err != nil {
		t.Fatal(err)
	}
	f := newFixture(t)
	k := f.run("run_a", state.PhaseRunning)
	if err := os.Symlink(secret, f.logPath(k)); err != nil {
		t.Fatal(err)
	}
	f.once()
	for _, b := range f.ctl.batches {
		for _, e := range b {
			if len(e.Data) > 0 {
				t.Fatalf("the target of a symlink was sent: %q", e.Data)
			}
		}
	}
	if !f.cursor(k).Done {
		t.Error("a log that is a symlink was not closed")
	}

	k2 := f.run("run_b", state.PhaseRunning)
	if err := syscall.Mkfifo(f.logPath(k2), 0o600); err != nil {
		t.Skipf("no mkfifo: %v", err)
	}
	done := make(chan struct{})
	go func() { f.once(); close(done) }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("a FIFO blocked the shipper")
	}
	if !f.cursor(k2).Done {
		t.Error("a log that is a pipe was not closed")
	}
}

func TestShip_ClosesTheOutputOfAFinishedRunOnceTheLogHasStoppedGrowing(t *testing.T) {
	f := newFixture(t)
	k := f.run("run_a", state.PhaseRunning)
	f.write(k, "work")
	f.once()
	f.phase(k, state.PhaseStopped)

	f.once() // first look at the finished Run: the log may still be growing
	if len(f.ctl.batches) != 1 {
		t.Fatalf("closed at once: %+v", f.ctl.batches)
	}
	f.write(k, " and the tail the capture flushed late")
	f.once()
	f.clk.Advance(1500 * time.Millisecond)
	f.once() // drained and stable for a settle: the end
	last := f.ctl.batches[len(f.ctl.batches)-1]
	e := last[len(last)-1]
	want := uint64(len("work and the tail the capture flushed late"))
	if !e.EOF || *e.FinalOffset != want || e.Note != "" {
		t.Fatalf("last = %+v", last)
	}
	if f.sent(k) != "work and the tail the capture flushed late" || !f.ctl.outputs[k].closed {
		t.Fatalf("sent %q", f.sent(k))
	}
	if c := f.cursor(k); !c.Done || !c.EOFSent {
		t.Fatalf("cursor = %+v", c)
	}
	before := len(f.ctl.batches)
	f.clk.Advance(time.Minute)
	f.once()
	if len(f.ctl.batches) != before {
		t.Error("sent after the end")
	}
}

func TestShip_ARunThatNeverWroteIsClosedEmpty(t *testing.T) {
	f := newFixture(t)
	k := f.run("run_a", state.PhaseStopped)
	f.once()
	e := f.ctl.batches[0][0]
	if !e.EOF || *e.FinalOffset != 0 || !f.cursor(k).Done {
		t.Fatalf("entry = %+v", e)
	}
}

func TestShip_ARefusedLeaseStopsTheOutputForGood(t *testing.T) {
	for _, code := range []domain.Code{domain.CodeStaleLease, domain.CodeNotFound, domain.CodePolicyDenied, domain.CodeInvalidRequest} {
		f := newFixture(t)
		k := f.run("run_a", state.PhaseRunning)
		f.write(k, "x")
		f.ctl.refuse["run_a"] = code
		f.once()
		if !f.cursor(k).Done {
			t.Errorf("%s: not done", code)
		}
		before := len(f.ctl.batches)
		f.write(k, "y")
		f.once()
		if len(f.ctl.batches) != before {
			t.Errorf("%s: tried again", code)
		}
	}
	// An error that may pass is retried.
	f := newFixture(t)
	k := f.run("run_a", state.PhaseRunning)
	f.write(k, "x")
	f.ctl.refuse["run_a"] = domain.CodeInternal
	f.once()
	if f.cursor(k).Done || f.cursor(k).Sent != 0 {
		t.Fatalf("cursor = %+v", f.cursor(k))
	}
	delete(f.ctl.refuse, "run_a")
	f.once()
	if f.sent(k) != "x" {
		t.Fatalf("sent %q", f.sent(k))
	}
}

func TestShip_BacksOffWhenTheControllerCannotBeReached(t *testing.T) {
	f := newFixture(t)
	k := f.run("run_a", state.PhaseRunning)
	f.write(k, "x")
	f.ctl.fail = errors.New("down")
	if err := f.ship.Once(t.Context()); err == nil {
		t.Fatal("no error")
	}
	f.ctl.fail = nil
	f.once() // inside the backoff: does nothing
	if len(f.ctl.batches) != 0 {
		t.Fatal("tried again at once")
	}
	f.clk.Advance(3 * time.Second)
	f.once()
	if f.sent(k) != "x" {
		t.Fatalf("sent %q", f.sent(k))
	}
}

func TestShip_ALostStateResumesFromWhatTheControllerHolds(t *testing.T) {
	f := newFixture(t)
	k := f.run("run_a", state.PhaseRunning)
	f.write(k, "0123456789")
	// The Controller already holds the first 6 bytes; this Worker lost its cursor.
	f.ctl.outputs[k] = &held{start: 0, end: 6, data: []byte("012345")}
	f.once()
	if c := f.cursor(k); c.Sent != 10 {
		t.Fatalf("cursor = %+v", c)
	}
	if f.sent(k) != "0123456789" {
		t.Fatalf("sent %q", f.sent(k))
	}
	// Had the Controller held more than the log, the cursor never passes the log.
	f2 := newFixture(t)
	k2 := f2.run("run_a", state.PhaseRunning)
	f2.write(k2, "abc")
	f2.ctl.outputs[k2] = &held{start: 0, end: 50, data: bytes.Repeat([]byte("z"), 50)}
	f2.once()
	if c := f2.cursor(k2); c.Sent != 3 {
		t.Fatalf("cursor = %+v", c)
	}
}

func TestShip_RunSendsOnTheClockAndStopsWithItsContext(t *testing.T) {
	f := newFixture(t)
	k := f.run("run_a", state.PhaseRunning)
	f.write(k, "tick")
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- f.ship.Run(ctx) }()
	for deadline := time.Now().Add(5 * time.Second); f.cursor(k).Sent != 4; time.Sleep(5 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatal("never sent")
		}
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

func TestShip_ABatchNeverHasMoreEntriesThanTheControllerTakes(t *testing.T) {
	f := newFixture(t)
	var keys []state.Key
	for i := range v1.OutputBatchMaxItems - 2 { // 62 Runs with one chunk each
		k := f.run(fmt.Sprintf("run_%03d", i), state.PhaseRunning)
		f.write(k, "x")
		keys = append(keys, k)
	}
	// The last has two chunks pending and is over: two data entries and the end make three.
	last := f.run("run_zzz", state.PhaseStopped)
	f.write(last, string(bytes.Repeat([]byte("y"), 2*v1.OutputChunkBytes)))
	keys = append(keys, last)

	for range 6 {
		f.clk.Advance(2 * time.Second)
		f.once()
	}
	for _, b := range f.ctl.batches {
		if len(b) > v1.OutputBatchMaxItems {
			t.Fatalf("a batch of %d entries", len(b))
		}
	}
	for _, k := range keys[:len(keys)-1] {
		if f.sent(k) != "x" {
			t.Fatalf("%s sent %q", k.RunID, f.sent(k))
		}
	}
	if h := f.ctl.outputs[last]; h == nil || len(h.data) != 2*v1.OutputChunkBytes || !h.closed || !f.cursor(last).Done {
		t.Fatalf("the last Run was not sent whole and closed: %+v", h)
	}
}

// A log that was removed and made again, whatever its inode, is not the log that
// was being sent: the start of what was sent says so.
func TestShip_TheFingerprintOfWhatWasSentTellsAnotherLogWithTheSameInode(t *testing.T) {
	f := newFixture(t)
	k := f.run("run_a", state.PhaseRunning)
	first := strings.Repeat("A", 6000) // more than the fingerprint covers
	f.write(k, first)
	f.once()
	c := f.cursor(k)
	if c.Sent != 6000 || c.PrefixLen != 4096 || len(c.PrefixSum) != 32 {
		t.Fatalf("cursor = %+v", c)
	}
	// Same size, other bytes, same inode: only the start of the file can tell.
	if err := os.WriteFile(f.logPath(k), []byte(strings.Repeat("B", 7000)), 0o600); err != nil {
		t.Fatal(err)
	}
	f.once()
	last := f.ctl.batches[len(f.ctl.batches)-1]
	e := last[len(last)-1]
	if !e.EOF || e.Note == "" || *e.FinalOffset != 6000 || f.sent(k) != first || !f.cursor(k).Done {
		t.Fatalf("last = %+v, sent %d bytes, cursor %+v", last, len(f.sent(k)), f.cursor(k))
	}
}

func TestShip_AGrowingLogKeepsItsFingerprintAndIsNotMistaken(t *testing.T) {
	f := newFixture(t)
	k := f.run("run_a", state.PhaseRunning)
	for _, text := range []string{"ab", strings.Repeat("c", 3000), strings.Repeat("d", 3000), "e"} {
		f.write(k, text)
		f.once()
		if c := f.cursor(k); c.Done {
			t.Fatalf("an appended log was taken for another: %+v", c)
		}
	}
	if c := f.cursor(k); c.PrefixLen != 4096 || c.Sent != 6003 {
		t.Fatalf("cursor = %+v", c)
	}
	if want := "ab" + strings.Repeat("c", 3000) + strings.Repeat("d", 3000) + "e"; f.sent(k) != want {
		t.Fatal("what was sent is not the log")
	}
}
