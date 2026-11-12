package app_test

import (
	"testing"

	"github.com/Autarquico/codefabric/internal/controller/store"
	"github.com/Autarquico/codefabric/internal/domain"
)

func (r *reporter) recordCheckpoint(id domain.ID) {
	r.e.t.Helper()
	must(r.e.t, r.e.db.WithTx(r.e.t.Context(), func(tx *store.Tx) error {
		_, err := tx.InsertCheckpoint(r.e.t.Context(), store.Checkpoint{Checkpoint: domain.Checkpoint{
			ID: id, RunID: domain.ID(r.run.ID), Trigger: domain.CheckpointManual,
			SourceRef: domain.CheckpointRef(id), CreatedAt: r.e.clock.Now(),
		}})
		return err
	}))
}

func TestCheckpointToken_OnlyCreatesTheRefOfACheckpointOfItsOwnRun(t *testing.T) {
	e := newEnv(t)
	alpha, beta := e.joinWorker("alpha"), e.joinWorker("beta")
	mine, other := e.acceptedRun(alpha, "x"), e.acceptedRun(beta, "y")
	cp, foreign := domain.ID("cp_01J00000000000000000000001"), domain.ID("cp_01J00000000000000000000002")
	mine.recordCheckpoint(cp)
	other.recordCheckpoint(foreign)
	project := mine.projectID()
	mint := func(r *reporter, id domain.ID) error {
		_, err := e.app.Runs.MintCheckpointToken(t.Context(), r.w, r.run.ID, r.lease.Epoch, id, sha1)
		return err
	}

	wantCode(t, mint(mine, foreign), domain.CodeNotFound)
	wantCode(t, mint(mine, "cp_01J00000000000000000000003"), domain.CodeNotFound)
	wantCode(t, mint(mine, "run_01J00000000000000000000001"), domain.CodeInvalidRequest)
	wantCode(t, mint(other, cp), domain.CodeNotFound)
	if _, err := e.app.Runs.MintCheckpointToken(t.Context(), beta, mine.run.ID, mine.lease.Epoch, cp, sha1); err == nil {
		t.Error("a token for another Worker's Run")
	}

	tok, err := e.app.Runs.MintCheckpointToken(t.Context(), alpha, mine.run.ID, mine.lease.Epoch, cp, sha1)
	must(t, err)
	if tok.Ref != domain.CheckpointRef(cp) || !tok.Write {
		t.Fatalf("token = %+v", tok)
	}
	g, err := e.authorize(alpha, project, true, tok.Token)
	if err != nil || g.Ref != tok.Ref || g.ExpectedOID != sha1 || g.Branch != "" || !g.Write {
		t.Fatalf("grant = %+v, %v", g, err)
	}
	if _, err := e.authorize(alpha, project, false, tok.Token); err == nil {
		t.Error("a checkpoint's token reads")
	}
	if _, err := e.authorize(beta, project, true, tok.Token); err == nil {
		t.Error("another Worker pushes with the token")
	}
	if _, err := e.authorize(alpha, "project_01J00000000000000000000009", true, tok.Token); err == nil {
		t.Error("the token writes in another Project")
	}

	// A token of the lease still writes no ref but the branch.
	lease := mine.token()
	if g, err := e.authorize(alpha, project, false, lease.Token); err != nil || g.Write || g.Ref != "" {
		t.Errorf("lease token grant = %+v, %v", g, err)
	}
}
