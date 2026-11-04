package main

import (
	"encoding/json"
	"testing"

	v1 "github.com/Autarquico/codefabric/internal/api/v1"
	"github.com/Autarquico/codefabric/internal/cli"
	"github.com/Autarquico/codefabric/internal/cli/actions"
	"github.com/Autarquico/codefabric/internal/domain"
)

// The actions run the very commands of the CLI against a real Controller: the
// same result, and the same error code and exit status.
func TestScenario_TuiViewsActions_EjecutaElComandoDeLaCLI(t *testing.T) {
	host, _ := bringUpController(t)
	use(t, host)
	newOrigin(t).clone(t)
	ok[v1.Project](t, "project", "create", "backend")
	ok[v1.CreateTaskResponse](t, "do", "echo hi", "--name", "auth-race")

	runner := actions.Runner{Options: cli.Options{Terminal: &spyTerminal{}}}
	ctx := t.Context()

	timeline := runner.Run(ctx, "task.timeline", actions.Input{Target: "auth-race"})
	if timeline.Failure != nil {
		t.Fatalf("timeline: %+v", timeline.Failure)
	}
	var page v1.TaskTimeline
	if err := json.Unmarshal(timeline.Output, &page); err != nil || len(page.Events) == 0 {
		t.Fatalf("timeline = %s, %v", timeline.Output, err)
	}

	cancelled := runner.Run(ctx, "task.cancel", actions.Input{Target: "auth-race"})
	var task v1.Task
	if cancelled.Failure != nil || json.Unmarshal(cancelled.Output, &task) != nil || task.Name != "auth-race" {
		t.Fatalf("cancel: %+v %s", cancelled.Failure, cancelled.Output)
	}

	missing := runner.Run(ctx, "task.cancel", actions.Input{Target: "no-such-task"})
	if f := missing.Failure; f == nil || f.Code != domain.CodeNotFound || f.ExitCode != domain.ExitCode(domain.CodeNotFound) || f.Hint == "" {
		t.Fatalf("cancel of a missing Task: %+v", f)
	}
}
