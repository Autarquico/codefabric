package executor

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"unicode/utf8"

	"github.com/Autarquico/codefabric/internal/adapters/harness"
	v1 "github.com/Autarquico/codefabric/internal/api/v1"
	"github.com/Autarquico/codefabric/internal/domain"
	"github.com/Autarquico/codefabric/internal/support/redact"
	"github.com/Autarquico/codefabric/internal/worker/execwrap"
	"github.com/Autarquico/codefabric/internal/worker/inputsock"
	"github.com/Autarquico/codefabric/internal/worker/secretjit"
	"github.com/Autarquico/codefabric/internal/worker/state"
)

// Needs-input on the Worker (structured-input:v1).
//
// A harness of a Run that asks for input reaches the Worker in two ways. A
// permission is a call to the MCP tool of the permission bridge (`cf worker
// mcp-bridge`), which asks the daemon over the socket of the Run; a question
// (AskUserQuestion) is a line of the structured stream the executor already
// reads. Either becomes a prompt: a row of state.db written in the transaction
// of the input_requested report, once, with its summary and options redacted of
// the secrets of the Run (the Controller has none and only bounds them).
//
// The answer is an `input` order. Written to state.db with the cursor of the
// queue, it is delivered by the supervisor of the Run:
//
//   - a permission goes to the bridge waiting on the socket, which returns it
//     to the harness as the result of the tool and acks. The daemon records the
//     delivery (input_delivered) on the ack, so an answer the bridge got but the
//     daemon did not hear of is acked again, and one the bridge did not get is
//     sent again, until it is: delivery of a permission is at least once, and
//     the bridge answers its harness once.
//   - a question goes to the FIFO of the harness as a message of its input
//     stream. That cannot be taken back, so it is at most once: the order is
//     marked delivering before the write, and one found so after a restart is
//     taken as delivered.
//
// An order for a Run that is over, for another lease, or for a prompt this Run
// never asked is dropped. A harness whose Worker does not announce the
// capability keeps the behaviour of stopForInput.

// InputFIFO is the name, in the run directory, of the FIFO that is the stdin of
// a harness with structured input.
const InputFIFO = "input.fifo"

const noDescription = "(the harness gave no description)"

// withheld stands for a text that cannot be redacted: the secrets of the Run
// cannot be read, and the text could carry one.
const withheld = "(withheld: the secrets of the Run cannot be read to redact this text)"

var promptIDPattern = regexp.MustCompile(`^[A-Za-z0-9._-]{1,` + strconv.Itoa(v1.MaxPromptIDLen) + `}$`)

// structuredInput says whether Runs of this spec are answered while they run:
// headless, a harness that can, and a Worker that announces it.
func (e *Executor) structuredInput(spec specView) bool {
	id := spec.Requirements.Harness
	a, ok := e.harnesses.Get(id)
	return ok && spec.Mode != execwrap.ModeInteractive && a.Capabilities().StructuredInput && e.announcesInput(id)
}

func (e *Executor) announcesInput(harnessID string) bool {
	for _, id := range e.o.StructuredInput {
		if id == harnessID {
			return true
		}
	}
	return false
}

// launchedWithInput says whether the Run was launched to be answered while it
// runs. That is decided once, by inputOfLaunch, and kept in the launch of the
// Run: what the daemon announces now may have changed since (another Claude
// Code), and the Run keeps the input it was launched with. A launch that cannot
// be read is an error, never an answer: a Run launched with input that is taken
// for one without would be stopped instead of asked.
func (e *Executor) launchedWithInput(k state.Key) (bool, error) {
	var l execwrap.Launch
	ok, err := execwrap.Read(e.runDir(k), execwrap.LaunchFile, &l)
	if err != nil {
		return false, fmt.Errorf("reading the launch of a Run: %w", err)
	}
	return ok && l.InputPath != "", nil
}

// inputOfLaunch tells the adapter how the Run is to be answered. A Run that
// asks permissions on a Worker that cannot answer them does not run: skipping
// the prompts it asked for would be running with more than it was given.
func (e *Executor) inputOfLaunch(r state.Run, spec specView, dir string, run *harness.RunSpec) error {
	ask := spec.Requirements.Permissions == v1.PermissionsAsk
	if !e.structuredInput(spec) {
		switch {
		case ask && spec.Mode == execwrap.ModeInteractive:
			return &codedFailure{domain.CodeInvalidRequest, "the Run asks for permissions, which only a headless Run can"}
		case ask:
			return &codedFailure{domain.CodeInvalidRequest, fmt.Sprintf("the Run asks for permissions and this Worker does not announce %s for %s",
				domain.StructuredInputCapability, spec.Requirements.Harness)}
		}
		return nil
	}
	run.StructuredInput, run.InputPath = true, e.inputPath(dir)
	if ask {
		path, err := inputsock.Path(e.socketDir(), r.RunID, r.LeaseEpoch)
		if err != nil {
			return err
		}
		run.AskPermissions = true
		run.PermissionServer = append(append([]string{}, e.o.PermissionServerArgv...), "--socket", path)
	}
	return nil
}

func (e *Executor) socketDir() string {
	if e.o.SocketDir != "" {
		return e.o.SocketDir
	}
	return inputsock.Dir(e.o.Paths.WorkerDataDir())
}

func (e *Executor) inputPath(dir string) string { return filepath.Join(dir, InputFIFO) }

// inRun says whether the harness of the Run is up, so that it can be answered.
func inRun(r state.Run) bool {
	return r.Phase == state.PhaseStarting || r.Phase == state.PhaseRunning
}

// inputRun is what the daemon holds for the input of one Run in this life of
// the daemon: the socket of its bridge and the connections waiting on answers.
type inputRun struct {
	e   *Executor
	ctx context.Context
	key state.Key
	srv *inputsock.Server

	mu      sync.Mutex
	waiters map[string]*waiter
}

// waiter is the bridge connection that waits for the answer of a prompt; sent
// says that the answer was written to this connection.
type waiter struct {
	conn *inputsock.Conn
	sent bool
}

func (ir *inputRun) setWaiter(promptID string, c *inputsock.Conn) {
	ir.mu.Lock()
	defer ir.mu.Unlock()
	ir.waiters[promptID] = &waiter{conn: c}
}

func (ir *inputRun) dropWaiter(promptID string) {
	ir.mu.Lock()
	defer ir.mu.Unlock()
	delete(ir.waiters, promptID)
}

// send writes the answer to the connection waiting on the prompt, once per connection.
func (ir *inputRun) send(promptID string, m inputsock.Message) {
	ir.mu.Lock()
	w := ir.waiters[promptID]
	if w == nil || w.sent {
		ir.mu.Unlock()
		return
	}
	w.sent = true
	ir.mu.Unlock()
	if err := w.conn.Send(m); err != nil {
		ir.mu.Lock()
		w.sent = false // the bridge connects again and asks again
		ir.mu.Unlock()
	}
}

// Request, Ack and Gone are the inputsock.Handler of the Run.
func (ir *inputRun) Request(c *inputsock.Conn, m inputsock.Message) {
	ir.e.permissionAsked(ir, c, m)
}

func (ir *inputRun) Ack(c *inputsock.Conn, m inputsock.Message) {
	ir.e.permissionAcked(ir, c, m)
}

func (ir *inputRun) Withdraw(c *inputsock.Conn, m inputsock.Message) {
	ir.e.permissionWithdrawn(ir, c, m)
}

func (ir *inputRun) Gone(c *inputsock.Conn) {
	ir.mu.Lock()
	defer ir.mu.Unlock()
	for id, w := range ir.waiters {
		if w.conn == c {
			delete(ir.waiters, id)
		}
	}
}

// serveInput is what the supervisor does about the input of a live Run at every
// turn: it has the socket of its bridge up (it is made again after a restart of
// the daemon, and the bridge, which outlives it, connects to it again), and
// delivers the orders that wait. The caller holds the lock of the Run. alive
// says that its harness has not exited.
func (e *Executor) serveInput(ctx context.Context, r state.Run, alive bool) {
	withInput, err := e.launchedWithInput(r.Key)
	if err != nil {
		e.log.WarnContext(ctx, "serving the input of a Run failed; it is tried again", "run", r.RunID, "err", err)
		return
	}
	if !withInput {
		return
	}
	if !alive || !inRun(r) {
		e.dropInput(ctx, r.Key)
		return
	}
	if parseSpec(r.Spec).Requirements.Permissions == v1.PermissionsAsk {
		e.ensureSocket(ctx, r)
	}
	e.deliverInputs(ctx, r)
	// The prompts may have closed while nobody looked at the end of the input: an
	// ack or a withdrawal heard by a daemon that died before acting on it.
	e.endInputOrWarn(ctx, r.Key)
}

func (e *Executor) ensureSocket(ctx context.Context, r state.Run) {
	e.inputMu.Lock()
	defer e.inputMu.Unlock()
	if e.inputs[r.Key] != nil {
		return
	}
	path, err := inputsock.Path(e.socketDir(), r.RunID, r.LeaseEpoch)
	if err != nil {
		e.log.ErrorContext(ctx, "the socket of the permission bridge of a Run cannot be made", "run", r.RunID, "err", err)
		return
	}
	ir := &inputRun{e: e, ctx: ctx, key: r.Key, waiters: map[string]*waiter{}}
	if ir.srv, err = inputsock.Listen(path, ir, inputsock.WithClock(e.clock)); err != nil {
		e.log.ErrorContext(ctx, "listening for the permission bridge of a Run failed; it is tried again", "run", r.RunID, "err", err)
		return
	}
	e.inputs[r.Key] = ir
}

func (e *Executor) inputOf(k state.Key) *inputRun {
	e.inputMu.Lock()
	defer e.inputMu.Unlock()
	return e.inputs[k]
}

// closeInput takes the socket of a Run down. It does not wait for the handlers
// of the socket: the caller may hold the lock of the Run, which they take, so
// closeInputs is the one that waits for them.
func (e *Executor) closeInput(k state.Key) {
	e.inputMu.Lock()
	ir := e.inputs[k]
	delete(e.inputs, k)
	e.inputMu.Unlock()
	if ir != nil {
		ir.srv.Shutdown()
		e.inputHandlers.Go(ir.srv.Wait)
	}
}

// closeInputs takes every socket down and waits for their handlers: the
// executor ends.
func (e *Executor) closeInputs() {
	e.inputMu.Lock()
	keys := make([]state.Key, 0, len(e.inputs))
	for k := range e.inputs {
		keys = append(keys, k)
	}
	e.inputMu.Unlock()
	for _, k := range keys {
		e.closeInput(k)
	}
	e.inputHandlers.Wait()
}

// dropInput gives up what waits for an answer of a Run that is not up any
// more: its socket, and its orders, which nobody can be handed to.
func (e *Executor) dropInput(ctx context.Context, k state.Key) {
	e.closeInput(k)
	if e.o.DB == nil {
		return
	}
	err := e.o.DB.WithTx(ctx, func(tx *state.Tx) error { return state.IgnoreInputOrders(ctx, tx, k) })
	if err != nil && ctx.Err() == nil {
		e.log.WarnContext(ctx, "giving up the input orders of a Run failed", "run", k.RunID, "err", err)
	}
}

// persistInput stores an `input` order in the transaction that moves the
// cursor. An order for a Run this Worker does not hold up, under another lease
// or epoch, is dropped: the cursor moves and nothing is stored.
func (e *Executor) persistInput(ctx context.Context, tx *state.Tx, c v1.WorkerCommand) error {
	in := c.Input
	if kind, _, err := domain.ParseID(in.RunID); err != nil || kind != domain.KindRun || in.Epoch == 0 || !promptIDPattern.MatchString(in.PromptID) {
		e.log.ErrorContext(ctx, "an input order does not name a prompt of a Run; it is ignored", "seq", c.Seq)
		return state.SetCursor(ctx, tx, c.Seq)
	}
	k := state.Key{RunID: in.RunID, LeaseEpoch: in.Epoch}
	r, err := state.RunBy(ctx, tx, k)
	switch {
	case errors.Is(err, state.ErrNotFound) || (err == nil && (!inRun(r) || r.LeaseID != in.LeaseID)):
		e.log.InfoContext(ctx, "an input order for a Run that is not up here is ignored", "seq", c.Seq, "run", in.RunID)
	case err != nil:
		return err
	default:
		if err := state.InsertInputOrder(ctx, tx, state.InputOrder{
			Seq: c.Seq, Key: k, LeaseID: in.LeaseID, PromptID: in.PromptID, Kind: in.Kind,
			Decision: in.Answer.Decision, Text: in.Answer.Text, CreatedAt: c.CreatedAt,
		}); err != nil {
			return err
		}
	}
	return state.SetCursor(ctx, tx, c.Seq)
}

// permissionAsked is a question from the bridge, or the same one asked again.
func (e *Executor) permissionAsked(ir *inputRun, c *inputsock.Conn, m inputsock.Message) {
	ctx, k := ir.ctx, ir.key
	if !promptIDPattern.MatchString(m.PromptID) {
		_ = c.Close()
		return
	}
	unlock := e.lock(k)
	defer unlock()
	r, err := state.RunBy(ctx, e.o.DB.Reader(), k)
	if err != nil || !inRun(r) {
		_ = c.Close()
		return
	}
	if _, err := state.InputPromptOf(ctx, e.o.DB.Reader(), k, m.PromptID); errors.Is(err, state.ErrNotFound) {
		summary := "permission to use " + m.Tool + ": " + compactJSON(m.Input)
		switch err := e.openPrompt(ctx, r, m.PromptID, domain.PromptPermission, summary, []string{domain.AnswerAllow, domain.AnswerDeny}, false); {
		case errors.Is(err, errTooManyPrompts):
			_ = c.Send(inputsock.Message{Op: inputsock.OpAnswer, PromptID: m.PromptID, Decision: domain.AnswerDeny, Message: "denied: too many questions are waiting for an answer"})
			return
		case err != nil:
			e.log.WarnContext(ctx, "recording a permission prompt failed; the bridge asks again", "run", k.RunID, "err", err)
			_ = c.Close()
			return
		}
	} else if err != nil {
		e.log.WarnContext(ctx, "reading a prompt failed; the bridge asks again", "run", k.RunID, "err", err)
		_ = c.Close()
		return
	}
	ir.setWaiter(m.PromptID, c)
	e.sendPermissionAnswer(ctx, ir, m.PromptID)
}

// permissionAcked records that an answer reached the harness.
func (e *Executor) permissionAcked(ir *inputRun, c *inputsock.Conn, m inputsock.Message) {
	ctx, k := ir.ctx, ir.key
	unlock := e.lock(k)
	defer unlock()
	if r, err := state.RunBy(ctx, e.o.DB.Reader(), k); err == nil && inRun(r) {
		if order, err := state.InputOrderFor(ctx, e.o.DB.Reader(), k, m.PromptID); err == nil {
			if err := e.finishDelivery(ctx, k, order); err != nil {
				e.log.WarnContext(ctx, "recording the delivery of a permission failed; the bridge acks again", "run", k.RunID, "err", err)
				_ = c.Close()
				return
			}
			e.endInputOrWarn(ctx, k)
		}
	}
	ir.dropWaiter(m.PromptID)
	_ = c.Send(inputsock.Message{Op: inputsock.OpAcked, PromptID: m.PromptID})
}

// permissionWithdrawn closes a prompt whose harness stopped waiting for the
// answer: it no longer holds the input of the harness open (it still counts
// against v1.MaxOpenPrompts: the Controller is not told), and an answer that comes for it is dropped.
func (e *Executor) permissionWithdrawn(ir *inputRun, c *inputsock.Conn, m inputsock.Message) {
	ctx, k := ir.ctx, ir.key
	unlock := e.lock(k)
	defer unlock()
	err := e.o.DB.WithTx(ctx, func(tx *state.Tx) error {
		_, err := state.WithdrawInputPrompt(ctx, tx, k, m.PromptID)
		return err
	})
	if err != nil {
		e.log.WarnContext(ctx, "withdrawing a permission prompt failed; the bridge withdraws it again", "run", k.RunID, "err", err)
		_ = c.Close()
		return
	}
	e.endInputOrWarn(ctx, k)
	ir.dropWaiter(m.PromptID)
	_ = c.Send(inputsock.Message{Op: inputsock.OpWithdrawn, PromptID: m.PromptID})
}

// sendPermissionAnswer writes the order that answers the prompt to the bridge
// that waits for it, if there is both.
func (e *Executor) sendPermissionAnswer(ctx context.Context, ir *inputRun, promptID string) {
	o, err := state.InputOrderFor(ctx, e.o.DB.Reader(), ir.key, promptID)
	if err != nil {
		return
	}
	m := inputsock.Message{Op: inputsock.OpAnswer, PromptID: promptID, Decision: domain.AnswerDeny, Message: "denied from the CodeFabric inbox"}
	if o.Decision == domain.AnswerAllow {
		m.Decision, m.Message = domain.AnswerAllow, ""
	}
	ir.send(promptID, m)
}

// deliverInputs hands the orders of the Run to its harness. The caller holds the lock.
func (e *Executor) deliverInputs(ctx context.Context, r state.Run) {
	orders, err := state.PendingInputOrders(ctx, e.o.DB.Reader(), r.Key)
	if err != nil {
		e.log.WarnContext(ctx, "reading the input orders of a Run failed", "run", r.RunID, "err", err)
		return
	}
	for _, o := range orders {
		if o.LeaseID != r.LeaseID {
			e.ignoreOrder(ctx, o)
			continue
		}
		p, err := state.InputPromptOf(ctx, e.o.DB.Reader(), r.Key, o.PromptID)
		if errors.Is(err, state.ErrNotFound) || (err == nil && p.State != state.PromptOpen) {
			e.ignoreOrder(ctx, o) // an answer to a question this Run never asked, or already answered
			continue
		}
		if err != nil {
			e.log.WarnContext(ctx, "reading a prompt failed", "run", r.RunID, "err", err)
			continue
		}
		if p.Kind == domain.PromptPermission {
			if ir := e.inputOf(r.Key); ir != nil {
				e.sendPermissionAnswer(ctx, ir, o.PromptID)
			}
			continue
		}
		e.writeAnswer(ctx, r, o)
	}
}

func (e *Executor) ignoreOrder(ctx context.Context, o state.InputOrder) {
	err := e.o.DB.WithTx(ctx, func(tx *state.Tx) error {
		_, err := state.SetInputOrderState(ctx, tx, o.Seq, o.State, state.OrderIgnored)
		return err
	})
	if err != nil && ctx.Err() == nil {
		e.log.WarnContext(ctx, "dropping an input order failed", "seq", o.Seq, "err", err)
	}
}

// writeAnswer puts the answer of a question in the input stream of the harness.
func (e *Executor) writeAnswer(ctx context.Context, r state.Run, o state.InputOrder) {
	if o.State == state.OrderDelivering {
		// The write was begun by a daemon that did not see it end. It is not written
		// again: a message the harness read twice is worse than one it did not.
		e.settleDelivery(ctx, r.Key, o)
		return
	}
	spec := parseSpec(r.Spec)
	adapter, ok := e.harnesses.Get(spec.Requirements.Harness)
	if !ok {
		return
	}
	h, err := e.handleOf(adapter, r, spec)
	if err != nil {
		e.log.WarnContext(ctx, "handing an answer to the harness of a Run failed; it is tried again", "run", r.RunID, "err", err)
		return
	}
	err = e.o.DB.WithTx(ctx, func(tx *state.Tx) error {
		changed, err := state.SetInputOrderState(ctx, tx, o.Seq, state.OrderQueued, state.OrderDelivering)
		if err == nil && !changed {
			err = state.ErrChanged
		}
		if err != nil {
			return err
		}
		// The answer begins a turn of the harness: its input stays open for it.
		return state.BeginTurn(ctx, tx, r.Key)
	})
	if err != nil {
		return
	}
	switch sendErr := adapter.SendInput(ctx, e.o.Host, h, harness.Input{Text: o.Text}); {
	case errors.Is(sendErr, harness.ErrInputNotSent) || errors.Is(sendErr, harness.ErrUnsupported):
		e.log.WarnContext(ctx, "handing an answer to the harness of a Run failed; it is tried again", "run", r.RunID, "err", sendErr)
		_ = e.o.DB.WithTx(ctx, func(tx *state.Tx) error {
			_, err := state.SetInputOrderState(ctx, tx, o.Seq, state.OrderDelivering, state.OrderQueued)
			return err
		})
		return
	case sendErr != nil:
		// Part of it may be in the stream: written again, the harness would read it
		// twice. It is taken as delivered, at most once.
		e.log.WarnContext(ctx, "handing an answer to the harness of a Run may have failed half-way; it is not sent again", "run", r.RunID, "err", sendErr)
	}
	e.settleDelivery(ctx, r.Key, o)
}

func (e *Executor) settleDelivery(ctx context.Context, k state.Key, o state.InputOrder) {
	if err := e.finishDelivery(ctx, k, o); err != nil && ctx.Err() == nil {
		e.log.WarnContext(ctx, "recording the delivery of an answer failed; it is tried again", "run", k.RunID, "err", err)
	}
}

// finishDelivery closes the prompt and its order, and tells the Controller, in
// one transaction.
func (e *Executor) finishDelivery(ctx context.Context, k state.Key, o state.InputOrder) error {
	err := e.o.DB.WithTx(ctx, func(tx *state.Tx) error {
		for _, from := range []string{state.OrderQueued, state.OrderDelivering} {
			if _, err := state.SetInputOrderState(ctx, tx, o.Seq, from, state.OrderDone); err != nil {
				return err
			}
		}
		changed, err := state.AcknowledgeInputPrompt(ctx, tx, k, o.PromptID)
		if err != nil || !changed {
			return err
		}
		return e.report(ctx, tx, k, v1.ReportInputDelivered, e.clock.Now(), func(en *v1.RunReportEntry) { en.PromptID = o.PromptID })
	})
	if err == nil {
		e.o.Journal.Notify()
	}
	return err
}

// errTooManyPrompts is what openPrompt answers a new prompt with when the Run
// already has v1.MaxOpenPrompts open: the Controller would reject the report
// and stall the journal behind it.
var errTooManyPrompts = errors.New("executor: a Run has too many prompts open")

// openPrompt writes a prompt and the report that asks the Controller for its
// answer, once: the same ask seen again finds its row. The summary, and the
// options unless they are fixed, are redacted of the secrets of the Run before
// they are bounded or reported. A new prompt past v1.MaxOpenPrompts is not
// written: errTooManyPrompts.
func (e *Executor) openPrompt(ctx context.Context, r state.Run, id string, kind domain.PromptKind, summary string, options []string, redactOptions bool) error {
	// What does not print goes first: removed after the redaction, it could
	// join the halves of a secret the redaction did not see whole.
	redacted := e.redactor(r)
	summary = reportable(redacted(reportableUnbounded(summary)))
	if summary == "" {
		summary = noDescription
	}
	var opts []string
	for _, o := range options {
		if redactOptions {
			o = redacted(reportableUnbounded(o))
		}
		if o = boundText(o, v1.MaxOptionLen); o != "" && len(opts) < v1.MaxPromptOption {
			opts = append(opts, o)
		}
	}
	err := e.o.DB.WithTx(ctx, func(tx *state.Tx) error {
		if _, err := state.InputPromptOf(ctx, tx, r.Key, id); !errors.Is(err, state.ErrNotFound) {
			return err // the same ask seen again, or a read that failed
		}
		held, err := state.ControllerHeldInputPrompts(ctx, tx, r.Key)
		if err != nil {
			return err
		}
		if held >= v1.MaxOpenPrompts {
			return errTooManyPrompts
		}
		inserted, err := state.InsertInputPrompt(ctx, tx, state.InputPrompt{Key: r.Key, PromptID: id, Kind: kind, CreatedAt: e.clock.Now()})
		if err != nil || !inserted {
			return err
		}
		return e.report(ctx, tx, r.Key, v1.ReportInputRequested, e.clock.Now(), func(en *v1.RunReportEntry) {
			en.PromptID, en.PromptKind, en.Summary, en.Options = id, kind, summary, opts
		})
	})
	if err == nil {
		e.o.Journal.Notify()
	}
	return err
}

// askedQuestions opens the prompts of the questions a harness asked in its
// stream. A question the harness gave no ID for is named by where it was read,
// which is the same on every read of the same bytes.
func (e *Executor) askedQuestions(ctx context.Context, r state.Run, offset uint64, evs []harness.Event) error {
	for i, ev := range evs {
		id := questionID(ev, offset, i)
		switch err := e.openPrompt(ctx, r, id, domain.PromptQuestion, ev.Message, ev.Options, true); {
		case errors.Is(err, errTooManyPrompts):
			// Past the limit the question is dropped, not reported: the Controller
			// would reject the batch and stall the journal behind it.
			e.log.WarnContext(ctx, "a question is dropped: the Run has too many prompts open", "run", r.RunID, "prompt", id)
		case err != nil:
			return err
		}
	}
	return nil
}

func questionID(ev harness.Event, offset uint64, index int) string {
	if ev.ID != "" {
		if id := "q-" + ev.ID; promptIDPattern.MatchString(id) {
			return id
		}
		sum := sha256.Sum256([]byte(ev.ID))
		return "q-" + hex.EncodeToString(sum[:12])
	}
	return fmt.Sprintf("q-%d-%d", offset, index)
}

// endInputIfIdle tells the wrapper that the input of the harness is over once
// it has finished a turn with no prompt left open: Claude Code, fed by a
// stream, ends only when the stream does. Either can come last, so it is asked
// again whenever one of them may have changed; asking again is harmless.
func (e *Executor) endInputIfIdle(ctx context.Context, k state.Key) error {
	cur, err := state.EventCursorOf(ctx, e.o.DB.Reader(), k)
	if err != nil || !cur.TurnOver {
		return err
	}
	open, err := state.OpenInputPrompts(ctx, e.o.DB.Reader(), k)
	if err != nil || open > 0 {
		return err
	}
	eof := filepath.Join(e.runDir(k), execwrap.InputEOFFile)
	if _, err := os.Stat(eof); err == nil {
		return nil
	}
	return os.WriteFile(eof, nil, 0o600)
}

func (e *Executor) endInputOrWarn(ctx context.Context, k state.Key) {
	if err := e.endInputIfIdle(ctx, k); err != nil && ctx.Err() == nil {
		e.log.WarnContext(ctx, "ending the input of a harness failed; it is tried again", "run", k.RunID, "err", err)
	}
}

// redactor is the redaction of the text of a Run of the secrets it holds. A Run
// without a delivery has nothing to redact; one whose secrets cannot be read
// gets text that says so, never the original.
func (e *Executor) redactor(r state.Run) func(string) string {
	needles, err := secretjit.Load(e.runDir(r.Key))
	switch {
	case errors.Is(err, secretjit.ErrNotDelivered) || (err == nil && len(needles) == 0):
		return func(s string) string { return s }
	case err != nil:
		return func(string) string { return withheld }
	}
	set, err := redact.NewEscapedSet(needles)
	if err != nil {
		return func(string) string { return withheld }
	}
	return func(s string) string { return string(redact.Bytes([]byte(s), set)) }
}

// compactJSON is a JSON value on one line as the harness wrote it: not decoded
// and encoded again, so that the redaction sees the escapes the harness used.
func compactJSON(raw json.RawMessage) string {
	var b bytes.Buffer
	if json.Compact(&b, raw) != nil {
		return strings.TrimSpace(string(raw))
	}
	return b.String()
}

// boundText makes s fit the check of the Controller (no controls, valid UTF-8)
// and at most limit bytes, cut at a character.
func boundText(s string, limit int) string {
	s = reportableUnbounded(s)
	if len(s) <= limit {
		return s
	}
	cut := limit
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut]
}
