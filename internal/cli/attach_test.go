package cli

import (
	"context"
	"errors"
	"testing"

	v1 "github.com/Autarquico/codefabric/internal/api/v1"
	"github.com/Autarquico/codefabric/internal/domain"
)

const (
	runA  = "run_01J8ZQ4M5N6P7R8S9T0V1W2X3Y"
	runB  = "run_01J8ZQ4M5N6P7R8S9T0V1W2X4Z"
	taskA = "task_01J8ZQ4M5N6P7R8S9T0V1W2X3Y"
	taskB = "task_01J8ZQ4M5N6P7R8S9T0V1W2X4Z"
	wkr   = "worker_01J8ZQ4M5N6P7R8S9T0V1W2X3Y"
)

// directory is the Controller's second opinion, as a test dictates it.
type directory struct {
	run  v1.Run
	task v1.Task
	id   string
}

func (d directory) Run(context.Context, string) (v1.Run, error)           { return d.run, nil }
func (d directory) Task(context.Context, string, string) (v1.Task, error) { return d.task, nil }
func (d directory) Resolve(context.Context, domain.Kind, string, string) (v1.Resolved, error) {
	return v1.Resolved{ID: d.id}, nil
}

func target() v1.AttachTarget {
	return v1.AttachTarget{RunID: runA, LeaseEpoch: 2, Worker: v1.WorkerRef{ID: wkr, Name: "alpha"}}
}

func honest() directory {
	return directory{
		run:  v1.Run{ID: runA, TaskID: taskA, WorkerID: wkr, Lease: &v1.RunLease{Epoch: 2}},
		task: v1.Task{ID: taskA}, id: runA,
	}
}

func TestVerifyTarget_OnlyTheRunAskedForUnderItsLiveLeaseOnItsWorkerIsObeyed(t *testing.T) {
	ctx := t.Context()
	for _, ref := range []string{runA, "run-1", "auth-race"} {
		if err := verifyTarget(ctx, honest(), ref, "", target()); err != nil {
			t.Errorf("%s: %v", ref, err)
		}
	}
	tamper := map[string]struct {
		ref    string
		mutate func(*directory, *v1.AttachTarget)
	}{
		"another Run than the ID asked for":      {runB, func(*directory, *v1.AttachTarget) {}},
		"another Run than the short ID":          {"run-1", func(d *directory, _ *v1.AttachTarget) { d.id = runB }},
		"a Run of another Task":                  {"auth-race", func(d *directory, _ *v1.AttachTarget) { d.task.ID = taskB }},
		"another lease epoch":                    {runA, func(_ *directory, a *v1.AttachTarget) { a.LeaseEpoch = 1 }},
		"a Run with no lease":                    {runA, func(d *directory, _ *v1.AttachTarget) { d.run.Lease = nil }},
		"another Worker":                         {runA, func(_ *directory, a *v1.AttachTarget) { a.Worker.ID = "worker_other" }},
		"a Run the second opinion does not know": {runA, func(d *directory, a *v1.AttachTarget) { a.RunID = runB; d.run.ID = runA }},
	}
	for name, tc := range tamper {
		d, a := honest(), target()
		tc.mutate(&d, &a)
		err := verifyTarget(ctx, d, tc.ref, "", a)
		var coded *domain.Error
		if !errors.As(err, &coded) || coded.Code != domain.CodeRunNotAttachable || coded.Details["reason"] != "target_mismatch" {
			t.Errorf("%s: %v", name, err)
		}
	}
}
