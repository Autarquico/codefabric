//go:build linux || darwin

package executor

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"syscall"
	"testing"
	"time"

	v1 "github.com/Autarquico/codefabric/internal/api/v1"
	"github.com/Autarquico/codefabric/internal/domain"
	"github.com/Autarquico/codefabric/internal/worker/execwrap"
	"github.com/Autarquico/codefabric/internal/worker/gitops"
	"github.com/Autarquico/codefabric/internal/worker/journal"
	"github.com/Autarquico/codefabric/internal/worker/state"
)

const (
	headOID   = "1111111111111111111111111111111111111111"
	resultOID = "2222222222222222222222222222222222222222"
)

var exitedAt = t0.Add(5 * time.Second)

// gitFixture is a fixture whose executor has a fake git.
func newGitFixture(t *testing.T) (*fixture, *fakeGit) {
	t.Helper()
	g := &fakeGit{head: headOID}
	f := newFixture(t, func(o *Options) {
		o.Git = g
		o.RelayTLS = RelayTLS{Cert: "/c/cert.pem", Key: "/c/key.pem", CA: "/c/ca.pem"}
	})
	return f, g
}

// exitedRun takes a Run to where finalize has written the exit and the intent to seal.
func (f *fixture) exitedRun() state.Key {
	f.t.Helper()
	k := f.started("echo hi")
	w := newWrapperProcess(f.t, "exit 0")
	<-w.ended
	f.writeWrapper(k, w.w)
	f.writeStarted(k, w.w, t0.Add(time.Second))
	f.writeExit(k, execwrap.Exit{ExitCode: intp(0), ExitedAt: exitedAt, Cause: execwrap.CauseExit})
	if !f.e.step(f.t.Context(), k) {
		f.t.Fatal("the Run was not settled")
	}
	return k
}

func (f *fixture) publication(k state.Key) state.Publication {
	f.t.Helper()
	p, err := state.PublicationOf(f.t.Context(), f.db.Reader(), k)
	if err != nil {
		f.t.Fatal(err)
	}
	return p
}

// turn is one turn of the publisher.
func (f *fixture) turn() bool { return f.e.publishDue(f.t.Context()) }

// passBackoff moves the clock past any wait of the publisher.
func (f *fixture) passBackoff() { f.clock.Advance(publishBackoffMax + time.Second) }

// failJournal makes every write to the journal fail, as a full disk would for SQLite.
func (f *fixture) failJournal(on bool) {
	f.t.Helper()
	sql := `DROP TRIGGER IF EXISTS journal_refused`
	if on {
		sql = `CREATE TRIGGER journal_refused BEFORE INSERT ON journal BEGIN SELECT RAISE(ABORT, 'database or disk is full'); END`
	}
	err := f.db.WithTx(f.t.Context(), func(tx *state.Tx) error {
		rows, err := tx.QueryContext(f.t.Context(), sql)
		if err != nil {
			return err
		}
		return rows.Close()
	})
	if err != nil {
		f.t.Fatal(err)
	}
}

func publicationKinds(f *fixture, k state.Key) []v1.RunReportKind { return kinds(f.reports(k)) }

func TestScenario_GitRelayPublication_Sellado_Executor(t *testing.T) {
	f, g := newGitFixture(t)
	k := f.exitedRun()

	// The exit stored the intent with it, and reported only the exit and the stop.
	p := f.publication(k)
	if p.State != state.PubSealing || p.ParentOID != headOID || !p.SealedAt.Equal(exitedAt) {
		t.Fatalf("publication = %+v", p)
	}
	if got := publicationKinds(f, k); !slices.Equal(got, []v1.RunReportKind{v1.ReportStarted, v1.ReportExited, v1.ReportStopped}) {
		t.Fatalf("reports before sealing = %v", got)
	}

	// result_commit is in state.db before the first report of the publication leaves the Worker.
	var seenCommit string
	reporter := &sealWatcher{f: f, k: k, seen: &seenCommit}
	sender := &journal.Sender{DB: f.db, Reporter: reporter, Journal: f.j, Clock: f.clock, Log: slog.New(slog.DiscardHandler)}

	if !f.turn() {
		t.Fatal("the publisher did nothing")
	}
	if g.sealCount() != 1 || g.seals[0].ParentOID != headOID || !g.seals[0].At.Equal(exitedAt) {
		t.Fatalf("seals = %+v", g.seals)
	}
	p = f.publication(k)
	if p.State != state.PubSealed || p.ResultCommit != resultOID {
		t.Fatalf("publication after sealing = %+v", p)
	}
	if _, err := sender.Flush(t.Context()); err != nil {
		t.Fatal(err)
	}
	if seenCommit != resultOID {
		t.Errorf("when `sealed` reached the Controller, state.db had result_commit %q, want %s", seenCommit, resultOID)
	}
}

// sealWatcher records what state.db holds when the `sealed` report arrives.
type sealWatcher struct {
	f    *fixture
	k    state.Key
	seen *string
}

func (w *sealWatcher) ReportRun(ctx context.Context, _ string, req v1.RunReportRequest) (v1.RunReportResponse, error) {
	out := v1.RunReportResponse{Run: v1.Run{}}
	for _, e := range req.Entries {
		if e.Kind == v1.ReportSealed {
			p, err := state.PublicationOf(ctx, w.f.db.Reader(), w.k)
			if err != nil {
				return out, err
			}
			*w.seen = p.ResultCommit
			if e.ResultCommit != p.ResultCommit {
				return out, fmt.Errorf("the report says %s and state.db %s", e.ResultCommit, p.ResultCommit)
			}
		}
		out.Outcomes = append(out.Outcomes, v1.RunReportOutcome{WorkerSeq: e.WorkerSeq, Result: v1.ReportApplied})
	}
	return out, nil
}

func TestScenario_GitRelayPublication_SelladoFallido_Executor(t *testing.T) {
	f, g := newGitFixture(t)
	g.seal = func(gitops.Intent) (gitops.Sealed, error) {
		return gitops.Sealed{}, fmt.Errorf("writing the commit: %w", syscall.ENOSPC)
	}
	k := f.exitedRun()
	if !f.turn() {
		t.Fatal("the publisher did nothing")
	}
	p := f.publication(k)
	if p.State != state.PubFailed || !strings.Contains(p.LastError, "no space left") {
		t.Fatalf("publication = %+v", p)
	}
	reports := f.reports(k)
	last := reports[len(reports)-1]
	if last.Kind != v1.ReportPublicationFailed || last.Phase != v1.PhaseSeal || !strings.Contains(last.Message, "no space left") {
		t.Errorf("last report = %+v", last)
	}
	// The failure is final: nothing more happens on its own.
	f.passBackoff()
	if f.turn() || g.sealCount() != 1 {
		t.Error("a failed sealing was tried again by itself")
	}

	// cf run publish: a new try, which seals again (there is no commit).
	g.mu.Lock()
	g.seal = nil
	g.mu.Unlock()
	f.publishOrder(k)
	if p := f.publication(k); p.State != state.PubSealing || p.Generation != 2 {
		t.Fatalf("after the order: %+v", p)
	}
	f.turn()
	if p := f.publication(k); p.State != state.PubSealed || p.ResultCommit != resultOID {
		t.Errorf("after sealing again: %+v", p)
	}
}

func TestSeal_WhenTheStateCannotBeWrittenEitherNothingIsReportedAndItIsTriedAgain(t *testing.T) {
	f, g := newGitFixture(t)
	k := f.exitedRun()
	before := f.reports(k)

	// git works, SQLite does not: no sealed report, the intent stays.
	f.failJournal(true)
	f.turn()
	if p := f.publication(k); p.State != state.PubSealing {
		t.Fatalf("publication = %+v", p)
	}
	if len(f.reports(k)) != len(before) {
		t.Error("a report was written")
	}
	// Neither works: the failure cannot be recorded either.
	g.mu.Lock()
	g.seal = func(gitops.Intent) (gitops.Sealed, error) { return gitops.Sealed{}, syscall.ENOSPC }
	g.mu.Unlock()
	f.passBackoff()
	f.turn()
	if p := f.publication(k); p.State != state.PubSealing {
		t.Fatalf("a failure that could not be recorded changed the state: %+v", p)
	}
	// The disk comes back: sealing is tried again, from the same intent.
	f.failJournal(false)
	g.mu.Lock()
	g.seal = nil
	g.mu.Unlock()
	f.passBackoff()
	if !f.turn() {
		t.Fatal("the publisher did nothing")
	}
	if p := f.publication(k); p.State != state.PubSealed {
		t.Errorf("publication = %+v", p)
	}
	for _, in := range g.seals {
		if in.ParentOID != headOID || !in.At.Equal(exitedAt) {
			t.Errorf("an attempt sealed from another intent: %+v", in)
		}
	}
}

func TestSeal_ARunWithoutARepositoryFailsToSeal(t *testing.T) {
	f, g := newGitFixture(t)
	g.headErr = gitops.ErrNotRepository
	g.seal = func(in gitops.Intent) (gitops.Sealed, error) {
		if in.ParentOID == "" {
			return gitops.Sealed{}, errors.New("no repository to seal")
		}
		return gitops.Sealed{Commit: resultOID}, nil
	}
	k := f.exitedRun()
	if p := f.publication(k); p.ParentOID != "" {
		t.Fatalf("parent = %q", p.ParentOID)
	}
	f.turn()
	reports := f.reports(k)
	if last := reports[len(reports)-1]; last.Kind != v1.ReportPublicationFailed || last.Phase != v1.PhaseSeal || !strings.Contains(last.Message, "no repository") {
		t.Errorf("last report = %+v", last)
	}
}

func TestARunThatNeverRanIsNotSealed(t *testing.T) {
	f, g := newGitFixture(t)
	g.prepareErr = errors.New("clone failed")
	k := f.accepted("echo hi")
	if !f.e.step(t.Context(), k) {
		t.Fatal("the Run did not end")
	}
	if _, err := state.PublicationOf(t.Context(), f.db.Reader(), k); !errors.Is(err, state.ErrNotFound) {
		t.Errorf("a Run that never ran has a publication: %v", err)
	}
	if f.turn() || g.sealCount() != 0 {
		t.Error("something was sealed")
	}
}

// sealed leaves a Run sealed and ready to push.
func (f *fixture) sealedRun() state.Key {
	f.t.Helper()
	k := f.exitedRun()
	f.turn()
	if p := f.publication(k); p.State != state.PubSealed {
		f.t.Fatalf("publication = %+v", p)
	}
	return k
}

// publishOrder gives the executor a `cf run publish` order for the Run.
func (f *fixture) publishOrder(k state.Key) {
	f.t.Helper()
	f.nextID++
	blocked, err := f.e.persist(f.t.Context(), []v1.WorkerCommand{{
		Seq: f.nextID + 100, Type: domain.CommandPublish, Publish: &v1.PublishCommand{RunID: k.RunID, LeaseID: "lease_x", Epoch: k.LeaseEpoch},
	}})
	if err != nil || blocked {
		f.t.Fatalf("persist: %v %v", blocked, err)
	}
}

func viewOf(k state.Key, p domain.Publication) v1.Run {
	return v1.Run{ID: k.RunID, Publication: p, Lease: &v1.RunLease{Epoch: k.LeaseEpoch, State: domain.LeaseAccepted}}
}

func (f *fixture) confirm(k state.Key, run v1.Run) {
	f.t.Helper()
	// The view answers the report of the push that is waiting, as the Controller would.
	f.confirmSeqs(k, []uint64{f.publication(k).PublishedSeq}, run)
}

func (f *fixture) confirmSeqs(k state.Key, seqs []uint64, run v1.Run) {
	f.t.Helper()
	if err := f.db.WithTx(f.t.Context(), func(tx *state.Tx) error { return f.e.ConfirmView(f.t.Context(), tx, k, seqs, run) }); err != nil {
		f.t.Fatal(err)
	}
}

func TestPush_SendsTheSealedCommitWithTheTokenAndTheCredentialsOfTheWorker(t *testing.T) {
	f, g := newGitFixture(t)
	k := f.sealedRun()
	if !f.turn() {
		t.Fatal("the publisher did nothing")
	}
	if g.pushCount() != 1 {
		t.Fatalf("%d pushes", g.pushCount())
	}
	c := g.pushes[0]
	if c.Project != "project_1" || c.Commit != resultOID || !strings.HasPrefix(c.Branch, "cf/") ||
		c.Auth.URL != "https://controller.test/v1/relay/project_1.git" || c.Auth.Token != "token-"+k.RunID ||
		c.Auth.Cert != "/c/cert.pem" || c.Auth.Key != "/c/key.pem" || c.Auth.CA != "/c/ca.pem" {
		t.Errorf("push = %+v", c)
	}
	if p := f.publication(k); p.State != state.PubPushed {
		t.Fatalf("publication = %+v", p)
	}
	reports := f.reports(k)
	if last := reports[len(reports)-1]; last.Kind != v1.ReportPublished || last.ResultCommit != resultOID {
		t.Errorf("last report = %+v", last)
	}
	// The token is nowhere in the state of the Worker.
	if dump := f.dumpState(); strings.Contains(dump, "token-"+k.RunID) {
		t.Error("the token is in state.db")
	}
	// Not confirmed until the Controller's view says so; and not pushed again meanwhile.
	f.passBackoff()
	if f.turn() || g.pushCount() != 1 {
		t.Error("a pushed publication was pushed again")
	}
	f.confirm(k, viewOf(k, domain.PublicationPending))
	if p := f.publication(k); p.State != state.PubPushed {
		t.Errorf("a pending view changed it: %+v", p)
	}
	f.confirm(k, v1.Run{ID: k.RunID, Publication: domain.PublicationPublished}) // a view with no lease: not the live one
	if p := f.publication(k); p.State != state.PubPushed {
		t.Errorf("a view of no lease changed it: %+v", p)
	}
	f.confirm(k, viewOf(k, domain.PublicationPublished))
	if p := f.publication(k); p.State != state.PubPublished {
		t.Errorf("publication = %+v", p)
	}
}

func (f *fixture) dumpState() string {
	f.t.Helper()
	var b strings.Builder
	for _, table := range []string{"local_runs", "publications", "journal", "meta", "stop_orders"} {
		rows, err := f.db.Reader().QueryContext(f.t.Context(), `SELECT * FROM `+table) //nolint:gosec // test constants
		if err != nil {
			f.t.Fatal(err)
		}
		cols, _ := rows.Columns()
		for rows.Next() {
			cells := make([]any, len(cols))
			ptrs := make([]any, len(cols))
			for i := range cells {
				ptrs[i] = &cells[i]
			}
			if err := rows.Scan(ptrs...); err != nil {
				f.t.Fatal(err)
			}
			fmt.Fprint(&b, cells...)
		}
		_ = rows.Close()
	}
	return b.String()
}

func TestScenario_GitRelayPublication_ControllerCaido_Executor(t *testing.T) {
	f, g := newGitFixture(t)
	k := f.sealedRun()
	down := true
	f.ctrl.mu.Lock()
	f.ctrl.relayToken = func(string, uint64) (v1.RelayTokenResponse, error) {
		if down {
			return v1.RelayTokenResponse{}, domain.NewError(domain.CodeControllerUnreachable, "connection refused", nil)
		}
		return v1.RelayTokenResponse{Token: "t", Path: "/v1/relay/project_1.git", Write: true}, nil
	}
	f.ctrl.mu.Unlock()

	// Far more tries than attempts are allowed: none counts.
	for range 3 * domain.Defaults().PublicationMaxAttempts {
		f.turn()
		f.passBackoff()
	}
	if p := f.publication(k); p.State != state.PubSealed || p.Attempts != 0 {
		t.Fatalf("publication = %+v", p)
	}
	if g.pushCount() != 0 {
		t.Fatal("it pushed with no token")
	}

	// The relay is reachable but git cannot connect: the same.
	f.ctrl.mu.Lock()
	down = false
	f.ctrl.mu.Unlock()
	g.mu.Lock()
	g.push = func(pushCall) error {
		return &gitops.PushError{Class: gitops.PushUnreachable, Message: "connection refused"}
	}
	g.mu.Unlock()
	for range 3 * domain.Defaults().PublicationMaxAttempts {
		f.turn()
		f.passBackoff()
	}
	if p := f.publication(k); p.State != state.PubSealed || p.Attempts != 0 {
		t.Fatalf("publication after unreachable pushes = %+v", p)
	}

	// The Controller is back: one push, the same commit, once.
	g.mu.Lock()
	g.push = nil
	g.mu.Unlock()
	f.turn()
	if p := f.publication(k); p.State != state.PubPushed {
		t.Fatalf("publication = %+v", p)
	}
	for _, c := range g.pushes {
		if c.Commit != resultOID {
			t.Errorf("a push of %s", c.Commit)
		}
	}
	published := 0
	for _, e := range f.reports(k) {
		if e.Kind == v1.ReportPublished {
			published++
		}
	}
	if published != 1 {
		t.Errorf("%d published reports", published)
	}
}

func TestPush_RefusalsCountAndTheTenthEndsThePublication(t *testing.T) {
	for name, class := range map[string]gitops.PushClass{"refused": gitops.PushRefused, "forbidden": gitops.PushForbidden, "permanent": gitops.PushPermanent, "local": gitops.PushLocal} {
		f, g := newGitFixture(t)
		k := f.sealedRun()
		g.push = func(pushCall) error {
			return &gitops.PushError{Class: class, Message: "rejected by the CodeFabric relay: no"}
		}
		limit := domain.Defaults().PublicationMaxAttempts
		for i := 1; i < limit; i++ {
			f.turn()
			if p := f.publication(k); p.State != state.PubSealed || p.Attempts != i {
				t.Fatalf("%s: after %d tries: %+v", name, i, p)
			}
			f.passBackoff()
		}
		f.turn()
		p := f.publication(k)
		if p.State != state.PubFailed {
			t.Fatalf("%s: publication = %+v", name, p)
		}
		reports := f.reports(k)
		if last := reports[len(reports)-1]; last.Kind != v1.ReportPublicationFailed || last.Phase != v1.PhasePush {
			t.Errorf("%s: last report = %+v", name, last)
		}
		if g.pushCount() != limit {
			t.Errorf("%s: %d pushes, want %d", name, g.pushCount(), limit)
		}
	}
}

func TestPush_ATokenTheControllerWillNotGiveEndsIt(t *testing.T) {
	f, g := newGitFixture(t)
	k := f.sealedRun()
	f.ctrl.mu.Lock()
	f.ctrl.relayToken = func(string, uint64) (v1.RelayTokenResponse, error) {
		return v1.RelayTokenResponse{}, domain.NewError(domain.CodeStaleLease, "this is not the live lease of the Run", nil)
	}
	f.ctrl.mu.Unlock()
	f.turn()
	if p := f.publication(k); p.State != state.PubFailed || g.pushCount() != 0 {
		t.Errorf("publication = %+v, %d pushes", p, g.pushCount())
	}
}

func TestPush_NoWriteAccessYetIsNotAnAttempt(t *testing.T) {
	f, g := newGitFixture(t)
	k := f.sealedRun()
	f.ctrl.mu.Lock()
	f.ctrl.relayToken = func(string, uint64) (v1.RelayTokenResponse, error) {
		return v1.RelayTokenResponse{Token: "t", Path: "/v1/relay/project_1.git", Write: false}, nil
	}
	f.ctrl.mu.Unlock()
	for range 15 {
		f.turn()
		f.passBackoff()
	}
	if p := f.publication(k); p.State != state.PubSealed || p.Attempts != 0 || g.pushCount() != 0 {
		t.Errorf("publication = %+v, %d pushes", p, g.pushCount())
	}
}

func TestPublish_ARetryPushesTheSameCommitWithoutSealingAgain(t *testing.T) {
	f, g := newGitFixture(t)
	k := f.sealedRun()
	f.turn()
	f.confirm(k, viewOf(k, domain.PublicationFailed)) // the Controller did not find the push
	if p := f.publication(k); p.State != state.PubFailed {
		t.Fatalf("publication = %+v", p)
	}
	f.publishOrder(k)
	if p := f.publication(k); p.State != state.PubSealed || p.Generation != 2 || p.Attempts != 0 || p.ResultCommit != resultOID {
		t.Fatalf("after the order: %+v", p)
	}
	f.turn()
	if g.sealCount() != 1 {
		t.Errorf("sealed %d times: a result that exists is not sealed again", g.sealCount())
	}
	if g.pushCount() != 2 || g.pushes[0].Commit != g.pushes[1].Commit {
		t.Errorf("pushes = %+v", g.pushes)
	}
	if p := f.publication(k); p.State != state.PubPushed {
		t.Errorf("publication = %+v", p)
	}
}

func TestPublish_ATryThatWasSlowCannotOverwriteTheNewOne(t *testing.T) {
	f, _ := newGitFixture(t)
	k := f.sealedRun()
	old := f.publication(k) // the old try read this, and is about to push
	f.turn()
	f.confirm(k, viewOf(k, domain.PublicationFailed))
	f.publishOrder(k) // generation 2, sealed
	reportsBefore := len(f.reports(k))

	// The old try finishes now: its push may have reached the relay, but it records nothing.
	f.e.push(t.Context(), old)
	if p := f.publication(k); p.State != state.PubSealed || p.Generation != 2 {
		t.Errorf("the old try overwrote the new one: %+v", p)
	}
	if len(f.reports(k)) != reportsBefore {
		t.Error("the old try reported")
	}
}

func TestPublish_AnOrderForARunThisWorkerNeverHeardOfIsAnswered(t *testing.T) {
	f, _ := newGitFixture(t)
	k := state.Key{RunID: runID(99), LeaseEpoch: 1}
	f.publishOrder(k)
	reports := f.reports(k)
	if len(reports) != 1 || reports[0].Kind != v1.ReportPublicationFailed || !strings.Contains(reports[0].Message, "no record") {
		t.Errorf("reports = %+v", reports)
	}
	cursor, _ := state.Cursor(t.Context(), f.db.Reader())
	if cursor == 0 {
		t.Error("the order was not passed over")
	}
}

func TestPublish_AFencedRunStillPushes(t *testing.T) {
	f, g := newGitFixture(t)
	k := f.sealedRun()
	if err := f.db.WithTx(t.Context(), func(tx *state.Tx) error { return state.Fence(t.Context(), tx, k, "CF_STALE_LEASE") }); err != nil {
		t.Fatal(err)
	}
	f.turn()
	if g.pushCount() != 1 {
		t.Errorf("%d pushes: the result is safe in the relay even if the Controller refuses the reports", g.pushCount())
	}
}

func TestPublish_ARestartKeepsWhereThePublicationWas(t *testing.T) {
	f, g := newGitFixture(t)
	k := f.exitedRun()
	f.restart("01J0000000000000000000000E2")
	f.turn()
	if p := f.publication(k); p.State != state.PubSealed {
		t.Fatalf("publication = %+v", p)
	}
	f.restart("01J0000000000000000000000E3")
	f.turn()
	if p := f.publication(k); p.State != state.PubPushed {
		t.Fatalf("publication = %+v", p)
	}
	f.restart("01J0000000000000000000000E4")
	f.passBackoff()
	if f.turn() || g.pushCount() != 1 {
		t.Error("a pushed publication was pushed after a restart")
	}
}

func TestPush_AcceptedButNotRecordedIsPushedAgainWithTheSameCommit(t *testing.T) {
	f, g := newGitFixture(t)
	k := f.sealedRun()

	// The relay takes the push and the state of the Worker cannot be written.
	f.failJournal(true)
	f.turn()
	if g.pushCount() != 1 {
		t.Fatalf("%d pushes", g.pushCount())
	}
	if p := f.publication(k); p.State != state.PubSealed {
		t.Fatalf("publication = %+v: a push that was not recorded did not happen for the Worker", p)
	}
	f.failJournal(false)
	f.passBackoff()
	f.turn()
	if g.pushCount() != 2 || g.pushes[0].Commit != g.pushes[1].Commit || g.pushes[1].Commit != resultOID {
		t.Errorf("pushes = %+v: the same commit, as a plain push", g.pushes)
	}
	if p := f.publication(k); p.State != state.PubPushed {
		t.Errorf("publication = %+v", p)
	}
}

func TestFinalize_AnUnreadableHeadClosesTheRunAndTheSealingReadsItAgainOrFails(t *testing.T) {
	for _, tc := range []struct {
		name      string
		recovers  bool
		wantState state.PubState
	}{{"HEAD is readable when sealing", true, state.PubSealed}, {"HEAD stays unreadable", false, state.PubFailed}} {
		f, g := newGitFixture(t)
		g.seal = func(in gitops.Intent) (gitops.Sealed, error) {
			if in.ParentOID == "" {
				return gitops.Sealed{}, errors.New("no repository to seal")
			}
			return gitops.Sealed{Commit: resultOID}, nil
		}
		g.headErr = errors.New("git is on fire")
		k := f.exitedRun() // the Run is settled whatever git says
		if r := f.run(k); r.Phase != state.PhaseStopped {
			t.Fatalf("%s: phase = %s", tc.name, r.Phase)
		}
		if got := publicationKinds(f, k); !slices.Equal(got, []v1.RunReportKind{v1.ReportStarted, v1.ReportExited, v1.ReportStopped}) {
			t.Fatalf("%s: reports = %v", tc.name, got)
		}
		if p := f.publication(k); p.ParentOID != "" || p.State != state.PubSealing {
			t.Fatalf("%s: publication = %+v", tc.name, p)
		}
		if tc.recovers {
			g.mu.Lock()
			g.headErr = nil
			g.mu.Unlock()
		}
		f.turn()
		p := f.publication(k)
		if p.State != tc.wantState || (tc.recovers && p.ParentOID != headOID) {
			t.Errorf("%s: publication = %+v", tc.name, p)
		}
		if !tc.recovers {
			reports := f.reports(k)
			if last := reports[len(reports)-1]; last.Kind != v1.ReportPublicationFailed || last.Phase != v1.PhaseSeal {
				t.Errorf("%s: last report = %+v", tc.name, last)
			}
		}
	}
}

func TestPublish_ANewTryRereadsTheHeadOfAWorktreeThatWasNotARepository(t *testing.T) {
	f, g := newGitFixture(t)
	g.headErr = gitops.ErrNotRepository
	g.seal = func(in gitops.Intent) (gitops.Sealed, error) {
		if in.ParentOID == "" {
			return gitops.Sealed{}, errors.New("no repository to seal")
		}
		return gitops.Sealed{Commit: resultOID}, nil
	}
	k := f.exitedRun()
	f.turn()
	if p := f.publication(k); p.State != state.PubFailed || p.ParentOID != "" {
		t.Fatalf("publication = %+v", p)
	}
	g.mu.Lock()
	g.headErr = nil
	g.mu.Unlock()
	f.publishOrder(k)
	f.turn()
	p := f.publication(k)
	if p.State != state.PubSealed || p.ParentOID != headOID {
		t.Errorf("publication = %+v", p)
	}
}

func TestPublish_TheAnswerToAnOlderTryNeverMovesTheNewOne(t *testing.T) {
	f, g := newGitFixture(t)
	k := f.sealedRun()
	f.turn() // generation 1 is pushed, with its `published` report
	first := f.publication(k)
	if first.State != state.PubPushed || first.PublishedSeq == 0 {
		t.Fatalf("publication = %+v", first)
	}
	// The answer to that report is held back (a barrier) while a new try goes by.
	f.publishOrder(k)
	f.turn()
	second := f.publication(k)
	if second.State != state.PubPushed || second.Generation != 2 || second.PublishedSeq == first.PublishedSeq {
		t.Fatalf("publication = %+v", second)
	}
	if g.pushCount() != 2 {
		t.Fatalf("%d pushes", g.pushCount())
	}
	// The held answer arrives: it is about the first report, and says failed.
	f.confirmSeqs(k, []uint64{first.PublishedSeq}, viewOf(k, domain.PublicationFailed))
	if p := f.publication(k); p.State != state.PubPushed {
		t.Fatalf("the answer to generation 1 moved generation 2: %+v", p)
	}
	// The answer to the new report does.
	f.confirmSeqs(k, []uint64{second.PublishedSeq}, viewOf(k, domain.PublicationPublished))
	if p := f.publication(k); p.State != state.PubPublished {
		t.Errorf("publication = %+v", p)
	}
}

func TestPublish_PublicationsThatWaitDoNotTakeThePlaceOfThoseThatAreDue(t *testing.T) {
	f, _ := newGitFixture(t)
	f.ctrl.mu.Lock()
	f.ctrl.relayToken = func(string, uint64) (v1.RelayTokenResponse, error) {
		return v1.RelayTokenResponse{Token: "t", Path: "/v1/relay/project_1.git", Write: false}, nil
	}
	f.ctrl.mu.Unlock()
	for range publishBatch {
		f.exitedRun()
		f.turn()
	}
	last := f.exitedRun()
	f.turn() // the batch is full of waiting publications; the new one is still reached
	if p := f.publication(last); p.State != state.PubSealed {
		t.Fatalf("the 17th publication is %s: the ones that wait took its place", p.State)
	}
}
