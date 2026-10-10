package tasks

import (
	"fmt"
	"path/filepath"
	"testing"
	"time"

	v1 "github.com/Autarquico/codefabric/internal/api/v1"
	"github.com/Autarquico/codefabric/internal/app/mutation"
	"github.com/Autarquico/codefabric/internal/controller/store"
	"github.com/Autarquico/codefabric/internal/domain"
	"github.com/Autarquico/codefabric/internal/support/clock"
)

// Rows 6, 7 and 8 of the §11.7 table, as stored in the Task: Rederive finds
// the state the table gives and says so with one TaskStateChanged, once.
func TestRederive_MaterialisesRows6To8OfTheTable(t *testing.T) {
	clk := clock.NewFake(time.Date(2026, 10, 10, 12, 0, 0, 0, time.UTC))
	db, err := store.Open(t.Context(), filepath.Join(t.TempDir(), "controller.db"), store.Options{Clock: clk})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	act := mutation.Act{Actor: domain.ActorRef{Kind: domain.ActorSystem, ID: "test"}, Now: clk.Now()}

	cases := []struct {
		name       string
		autostart  bool
		queuedRun  bool
		wantState  domain.TaskState
		wantReason domain.WaitingReason
	}{
		{"row 6: a queued Run", false, true, domain.TaskQueued, ""},
		{"row 7: no Runs and autostart", true, false, domain.TaskQueued, ""},
		{"row 8: no Runs and no autostart", false, false, domain.TaskWaiting, domain.WaitingStart},
	}
	ids := make([]domain.ID, len(cases))
	err = db.WithTx(t.Context(), func(tx *store.Tx) error {
		project := store.Project{ID: "project_P", Short: 1, Name: "p", Repo: "r", DefaultBranch: "main", CreatedAt: clk.Now()}
		if err := tx.InsertProject(t.Context(), project); err != nil {
			return err
		}
		for i, c := range cases {
			ids[i] = domain.ID("task_T" + string(rune('A'+i)))
			// Stored with a state the table does not give, as after a missed Rederive.
			task := store.Task{
				ID: ids[i], Short: uint64(i + 1), ProjectID: project.ID, Name: fmt.Sprintf("t%d", i),
				Prompt: "x", Autostart: c.autostart, Requirements: v1.Requirements{}, CreatedBy: domain.CreatorHuman,
				State: domain.TaskBlocked, CreatedAt: clk.Now(),
			}
			if err := tx.InsertTask(t.Context(), task); err != nil {
				return err
			}
			if c.queuedRun {
				run := store.Run{
					ID: "run_R", Short: 1, TaskID: ids[i], Attempt: 1, Lineage: domain.LineageInitial, Spec: []byte("{}"),
					SpecDigest: "sha256:x", SourceCommit: "c", State: domain.RunQueued, Publication: domain.PublicationNone,
					Evaluation: domain.EvaluationNone, Runtime: domain.RuntimeNone, CreatedAt: clk.Now(),
				}
				if err := tx.InsertRun(t.Context(), run); err != nil {
					return err
				}
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}

	for i, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			rederive := func() error {
				return db.WithTx(t.Context(), func(tx *store.Tx) error { return Rederive(t.Context(), tx, act, ids[i]) })
			}
			if err := rederive(); err != nil {
				t.Fatal(err)
			}
			if err := rederive(); err != nil { // a second pass has nothing to say
				t.Fatal(err)
			}
			var got store.Task
			err := db.Read(t.Context(), func(q store.Querier) (err error) {
				got, err = store.TaskByID(t.Context(), q, ids[i])
				return err
			})
			if err != nil {
				t.Fatal(err)
			}
			if got.State != c.wantState || got.WaitingReason != c.wantReason {
				t.Errorf("stored state %s(%s), want %s(%s)", got.State, got.WaitingReason, c.wantState, c.wantReason)
			}
			events, err := db.Events(t.Context(), 0, 100, store.EventFilter{TaskID: ids[i], Types: []domain.EventType{domain.EventTaskStateChanged}})
			if err != nil {
				t.Fatal(err)
			}
			if len(events) != 1 || events[0].Data["from"] != "blocked" || events[0].Data["to"] != string(c.wantState) ||
				events[0].Data["waiting_reason"] != string(c.wantReason) {
				t.Errorf("events = %+v", events)
			}
		})
	}
}
