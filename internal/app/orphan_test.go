package app_test

import (
	"testing"
	"time"

	v1 "github.com/Autarquico/codefabric/internal/api/v1"
	"github.com/Autarquico/codefabric/internal/controller/pki"
	"github.com/Autarquico/codefabric/internal/domain"
)

// orphanUnderOldEpoch makes the Run of alpha an orphan under epoch 1: the lease
// expired, epoch 2 was accepted, and a report came under epoch 1.
func (e *env) orphanUnderOldEpoch(alpha pki.Session) (old, live *reporter) {
	e.t.Helper()
	run := e.queuedRun("x")
	first := e.offer(run.ID, alpha)
	e.clock.Advance(domain.Defaults().LeaseAcceptBy + time.Second)
	_, err := e.app.Leases.SweepExpired(e.t.Context())
	must(e.t, err)
	second := e.offer(run.ID, alpha)
	_, err = e.accept(alpha, second)
	must(e.t, err)
	old = &reporter{e: e, w: alpha, run: run, lease: first, epoch: reportEpoch, seq: 500}
	live = &reporter{e: e, w: alpha, run: run, lease: second, epoch: reportEpoch}
	old.mustSend(old.entry(v1.ReportStarted))
	return old, live
}

func TestOrphanToken_OnlyForAnOrphanOfTheWorkerAndOnlyToCreateItsRef(t *testing.T) {
	e := newEnv(t)
	alpha, beta := e.joinWorker("alpha"), e.joinWorker("beta")
	old, live := e.orphanUnderOldEpoch(alpha)
	project := old.projectID()
	mint := func(w pki.Session, epoch uint64) (v1.RelayTokenResponse, error) {
		return e.app.Runs.MintOrphanToken(t.Context(), w, old.run.ID, v1.OrphanTokenRequest{LeaseEpoch: epoch, ResultCommit: sha1})
	}
	if _, err := mint(alpha, live.lease.Epoch); err == nil {
		t.Error("a token for the live epoch, which is no orphan")
	}
	if _, err := mint(beta, old.lease.Epoch); err == nil {
		t.Error("a token for another Worker's orphan")
	}
	tok, err := mint(alpha, old.lease.Epoch)
	must(t, err)
	if tok.Ref != domain.OrphanRef(domain.ID(old.run.ID), old.lease.Epoch) || !tok.Write {
		t.Fatalf("token = %+v", tok)
	}
	g, err := e.authorize(alpha, project, true, tok.Token)
	if err != nil || !g.CreateOnly || g.Ref != tok.Ref || g.ExpectedOID != sha1 || g.Branch != "" {
		t.Fatalf("grant = %+v, %v", g, err)
	}
	if _, err := e.authorize(alpha, project, false, tok.Token); err == nil {
		t.Error("an orphan's token reads")
	}
	if _, err := e.authorize(beta, project, true, tok.Token); err == nil {
		t.Error("another Worker pushes with the token")
	}

	// Preserved: the report under the stale epoch is taken once, and the token is no more.
	e.relay.tips = map[string]string{project + "/" + tok.Ref: sha1}
	preserved := old.entry(v1.ReportOrphanPreserved, sealedTo(sha1))
	if got := results(old.mustSend(preserved)); got != "applied" {
		t.Fatalf("orphan_preserved = %s", got)
	}
	if got := results(old.mustSend(preserved)); got != "duplicate" {
		t.Errorf("the same report again = %s", got)
	}
	if got := results(old.mustSend(old.entry(v1.ReportOrphanPreserved, sealedTo(sha1)))); got != "rejected:CF_INVALID_TRANSITION" {
		t.Errorf("another report of it = %s", got)
	}
	if n := len(e.events(domain.EventRunOrphanPreserved)); n != 1 {
		t.Errorf("RunOrphanPreserved = %d", n)
	}
	if _, err := e.authorize(alpha, project, true, tok.Token); err == nil {
		t.Error("the token still writes once the orphan is preserved")
	}
	if _, err := mint(alpha, old.lease.Epoch); err == nil {
		t.Error("a token for an orphan preserved")
	}
}

func TestOrphanPreserved_TheRelayMustHoldTheCommit(t *testing.T) {
	e := newEnv(t)
	alpha := e.joinWorker("alpha")
	old, _ := e.orphanUnderOldEpoch(alpha)
	if got := results(old.mustSend(old.entry(v1.ReportOrphanPreserved, sealedTo(sha1)))); got != "rejected:CF_INVALID_REQUEST" {
		t.Errorf("orphan_preserved without the ref = %s", got)
	}
	if n := len(e.events(domain.EventRunOrphanPreserved)); n != 0 {
		t.Errorf("RunOrphanPreserved = %d", n)
	}
}
