package app_test

import (
	"context"
	"crypto/rand"
	"errors"
	"strings"
	"testing"

	v1 "github.com/Autarquico/codefabric/internal/api/v1"
	"github.com/Autarquico/codefabric/internal/app/mutation"
	"github.com/Autarquico/codefabric/internal/app/runs"
	"github.com/Autarquico/codefabric/internal/app/tasks"
	"github.com/Autarquico/codefabric/internal/controller/store"
	"github.com/Autarquico/codefabric/internal/domain"
)

// A use case is one transaction: when the last step fails, none of the
// earlier ones — marker, Run state, events — remains.
func TestCancel_FailureOfTheLastStepRollsEverythingBack(t *testing.T) {
	e := newEnv(t)
	e.project("backend")
	created := e.mustTask("backend", "x")
	before := len(e.events())

	boom := errors.New("rederive failed")
	failing := func(context.Context, *store.Tx, mutation.Act, domain.ID) error { return boom }
	ids := domain.NewIDGenerator(e.clock.Now, rand.Reader)
	broken := tasks.New(e.db, e.clock, ids, runs.New(e.db, ids, failing))

	_, err := broken.Cancel(t.Context(), e.owner, created.Task.ID, "", v1.ActionRequest{})
	if !errors.Is(err, boom) {
		t.Fatalf("err = %v", err)
	}

	task, err := e.app.Tasks.Get(t.Context(), created.Task.ID, "")
	must(t, err)
	run, err := e.app.Runs.Get(t.Context(), created.Run.ID)
	must(t, err)
	if task.State != domain.TaskQueued || task.CancelledAt != nil || run.State != domain.RunQueued || run.FinishedAt != nil {
		t.Errorf("task %+v, run %+v", task, run)
	}
	if got := len(e.events()); got != before {
		t.Errorf("%d events survived the rollback", got-before)
	}
}

// Saving the response of the request_id is the last step of the unit: if it
// fails, no Task, Run, event or key remains.
func TestCreateTask_FailureSavingTheResponseRollsEverythingBack(t *testing.T) {
	e := newEnv(t)
	e.project("backend")
	before := len(e.events())
	must(t, e.db.WithTx(t.Context(), func(tx *store.Tx) error {
		_, err := tx.Exec(t.Context(), `CREATE TRIGGER refuse_keys BEFORE INSERT ON idempotency_keys BEGIN SELECT RAISE(ABORT, 'refused'); END`)
		return err
	}))

	_, err := e.createTask("backend", "x", func(r *v1.CreateTaskRequest) { r.RequestID = "req-1" })
	if err == nil || !strings.Contains(err.Error(), "refused") {
		t.Fatalf("err = %v", err)
	}
	if e.count("tasks") != 0 || e.count("runs") != 0 || e.count("idempotency_keys") != 0 || len(e.events()) != before {
		t.Errorf("tasks %d, runs %d, keys %d, events %d", e.count("tasks"), e.count("runs"), e.count("idempotency_keys"), len(e.events())-before)
	}
}

// Creating a Run is part of the same unit as creating its Task.
func TestCreateTask_FailureWhileCreatingTheRunLeavesNoTask(t *testing.T) {
	e := newEnv(t)
	e.project("backend")
	boom := errors.New("rederive failed")
	ids := domain.NewIDGenerator(e.clock.Now, rand.Reader)
	broken := tasks.New(e.db, e.clock, ids, runs.New(e.db, ids,
		func(context.Context, *store.Tx, mutation.Act, domain.ID) error { return boom }))

	_, err := broken.Create(t.Context(), e.owner, v1.CreateTaskRequest{Project: "backend", Prompt: "x", ResolvedRef: "main", SourceCommit: commitA})
	if !errors.Is(err, boom) {
		t.Fatalf("err = %v", err)
	}
	if e.count("tasks") != 0 || e.count("runs") != 0 || len(e.events()) != 0 {
		t.Errorf("tasks %d, runs %d, events %d", e.count("tasks"), e.count("runs"), len(e.events()))
	}
	// Short IDs are given back with the rollback.
	if got := e.mustTask("backend", "x"); got.Task.Short != "task-1" || got.Run.Short != "run-1" {
		t.Errorf("short IDs %s %s", got.Task.Short, got.Run.Short)
	}
}
