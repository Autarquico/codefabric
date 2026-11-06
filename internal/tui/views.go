package tui

import (
	"fmt"
	"strconv"
	"strings"
	"time"

	v1 "github.com/Autarquico/codefabric/internal/api/v1"
	"github.com/Autarquico/codefabric/internal/cli/actions"
	"github.com/Autarquico/codefabric/internal/domain"
)

// The views of §19.4, by route. Tasks, Runs, Workers, Workflows and Projects are
// lists over a feed with actions from the registry (they need a Backend); the
// others are screens of their own.

// specs are all the views of the shell.
func specs() map[Route]viewSpec {
	all := []viewSpec{
		tasksDef().spec(), runsDef().spec(), workersDef().spec(), workflowsDef().spec(), projectsDef().spec(),
		activityView(), modelsView(), logsView(),
	}
	byRoute := make(map[Route]viewSpec, len(all))
	for _, spec := range all {
		byRoute[spec.route] = spec
	}
	return byRoute
}

// needsForm are the actions of a kind that take their inputs from a form, which
// the menu has no room for: editing a Task, and reads that the detail shows.
var needsForm = map[string]bool{"task.set": true, "task.timeline": true}

func skipForms(d actions.Descriptor) bool { return needsForm[d.ID] }

func tasksDef() viewDef[v1.Task] {
	return viewDef[v1.Task]{
		route: RouteTasks, title: "Tasks", kind: actions.Task, timeline: true,
		columns:   []Column{{"ID", 9}, {"State", 10}, {"Waiting", 9}, {"Created", 8}, {"Name", 0}},
		empty:     "No tasks yet",
		skip:      skipForms,
		shortcuts: []shortcut{{"a", "Attach", "run.attach"}, {"c", "Cancel", "task.cancel"}, {"d", "Complete", "task.complete"}, {"x", "Archive", "task.archive"}},
		list:      func(s *State) *Feed[v1.Task] { return &s.Tasks },
		source:    func(f Feeds) FeedSource[v1.Task] { return f.Tasks },
		id:        func(t v1.Task) string { return t.ID },
		row: func(t v1.Task, c RenderContext) Row {
			return Row{
				ID: t.ID, Label: taskName(t), Stamp: string(t.State) + "/" + string(t.WaitingReason),
				Cells: []Cell{
					{t.Short, ToneFaint},
					{string(t.State), taskTone(t.State)},
					{string(t.WaitingReason), waitingTone(t.WaitingReason)},
					{activityTime(t.CreatedAt, c.Now, c.Loc), ToneFaint},
					{t.Name, ToneText},
				},
			}
		},
		detail: func(t v1.Task, _ RenderContext) []Field {
			return []Field{
				{"Task", taskName(t), ToneText},
				{"State", string(t.State), taskTone(t.State)},
				{"Waiting", string(t.WaitingReason), waitingTone(t.WaitingReason)},
				{"Mode", string(t.Mode), ToneText},
				{"Completion", string(t.Completion), ToneText},
				{"Workflow", t.Workflow, ToneText},
				{"Prompt", firstLine(t.Prompt), ToneFaint},
			}
		},
	}
}

func runsDef() viewDef[v1.Run] {
	return viewDef[v1.Run]{
		route: RouteRuns, title: "Runs", kind: actions.Run,
		columns: []Column{{"ID", 9}, {"Try", 3}, {"State", 14}, {"Runtime", 8}, {"Started", 7}, {"Task", 0}},
		empty:   "No runs yet",
		skip:    func(d actions.Descriptor) bool { return d.ID == "run.fork" || d.ID == "run.promote" },
		// Publish is a capital: "p" is Projects on the home, and a key must not
		// mean a navigation in one place and an outward action in the next.
		shortcuts: []shortcut{{"a", "Attach", "run.attach"}, {"P", "Publish", "run.publish"}},
		needs:     []Route{RouteTasks, RouteWorkers},
		list:      func(s *State) *Feed[v1.Run] { return &s.Runs },
		source:    func(f Feeds) FeedSource[v1.Run] { return f.Runs },
		id:        func(r v1.Run) string { return r.ID },
		row: func(r v1.Run, c RenderContext) Row {
			return Row{
				ID: r.ID, Label: runName(r), Stamp: string(r.State),
				Cells: []Cell{
					{r.Short, ToneFaint},
					{strconv.Itoa(r.Attempt), ToneFaint},
					{string(r.State), runTone(r.State)},
					{string(r.Runtime), ToneFaint},
					{startedAt(r, c), ToneFaint},
					{taskOf(c.State, r.TaskID), ToneText},
				},
			}
		},
		detail: func(r v1.Run, c RenderContext) []Field {
			exit := ""
			if r.ExitCode != nil {
				exit = strconv.Itoa(*r.ExitCode)
			}
			return []Field{
				{"Run", runName(r), ToneText},
				{"State", string(r.State), runTone(r.State)},
				{"Ended by", string(r.EndReason), ToneText},
				{"Exit code", exit, ToneText},
				{"Task", taskOf(c.State, r.TaskID), ToneText},
				{"Worker", workerOf(c.State, r), reachTone(r)},
				{"Branch", r.Branch, ToneText},
				{"Model", r.Model, ToneText},
				{"Publication", publicationOf(r), publicationTone(r.Publication)},
				{"Evaluation", evaluationOf(r), ToneText},
			}
		},
	}
}

func workersDef() viewDef[v1.Worker] {
	return viewDef[v1.Worker]{
		route: RouteWorkers, title: "Workers", kind: actions.Worker,
		columns:   []Column{{"Name", 16}, {"State", 10}, {"Health", 9}, {"Runs", 5}, {"Seen", 7}, {"Host", 0}},
		empty:     "No workers yet",
		shortcuts: []shortcut{{"d", "Drain", "worker.drain"}, {"u", "Undrain", "worker.undrain"}, {"x", "Remove", "worker.remove"}},
		list:      func(s *State) *Feed[v1.Worker] { return &s.Workers },
		source:    func(f Feeds) FeedSource[v1.Worker] { return f.Workers },
		id:        func(w v1.Worker) string { return w.ID },
		row: func(w v1.Worker, c RenderContext) Row {
			return Row{
				ID: w.ID, Label: workerName(w), Stamp: string(w.State),
				Cells: []Cell{
					{w.Name, ToneText},
					{string(w.State), workerTone(w.State)},
					{w.Health.Status, healthTone(w.Health.Status)},
					{fmt.Sprintf("%d/%d", w.ActiveRuns, w.MaxConcurrentRuns), ToneFaint},
					{seen(w.LastHeartbeatAt, c), ToneFaint},
					{w.Hostname, ToneFaint},
				},
			}
		},
		detail: func(w v1.Worker, _ RenderContext) []Field {
			fields := []Field{
				{"Worker", workerName(w), ToneText},
				{"State", string(w.State), workerTone(w.State)},
				{"Health", w.Health.Status, healthTone(w.Health.Status)},
				{"Message", w.Health.Message, ToneFaint},
				{"Runs", fmt.Sprintf("%d active of %d, %d reserved", w.ActiveRuns, w.MaxConcurrentRuns, w.Reservations), ToneText},
				{"Host", strings.TrimSpace(w.Hostname + " " + w.OS + "/" + w.Arch), ToneText},
				{"Daemon", daemonOf(w), versionTone(w)},
			}
			for _, comp := range w.Health.Components {
				if comp.Status != v1.ComponentOK {
					tone := ToneWarning
					if comp.Status == v1.ComponentFail {
						tone = ToneDanger
					}
					fields = append(fields, Field{comp.Name, joinParts(comp.Status, comp.Message, comp.Action), tone})
				}
			}
			return fields
		},
	}
}

// taskOf names a Task the way the Tasks view does, from the list that the Runs
// view follows for it; the shortened ID is only for a Task nobody has told us.
func taskOf(s *State, id string) string {
	for _, t := range s.Tasks.Items {
		if t.ID == id {
			return t.Name
		}
	}
	return shortRef(id)
}

// workerNameOf is the same for a Worker.
func workerNameOf(s *State, id string) string {
	for _, w := range s.Workers.Items {
		if w.ID == id {
			return w.Name
		}
	}
	return shortRef(id)
}

// joinParts joins what is not empty with " · ".
func joinParts(parts ...string) string {
	var kept []string
	for _, p := range parts {
		if p != "" {
			kept = append(kept, p)
		}
	}
	return strings.Join(kept, " · ")
}

func taskName(t v1.Task) string { return t.Name + " (" + t.Short + ")" }

func runName(r v1.Run) string { return r.Short + " (try " + strconv.Itoa(r.Attempt) + ")" }

func workerName(w v1.Worker) string { return w.Name + " (" + w.Short + ")" }

func firstLine(s string) string {
	line, _, _ := strings.Cut(strings.TrimSpace(s), "\n")
	return line
}

func startedAt(r v1.Run, c RenderContext) string {
	if r.StartedAt == nil {
		return noValue
	}
	return activityTime(*r.StartedAt, c.Now, c.Loc)
}

func seen(at *time.Time, c RenderContext) string {
	if at == nil {
		return noValue
	}
	return activityTime(*at, c.Now, c.Loc)
}

func workerOf(s *State, r v1.Run) string {
	switch {
	case r.WorkerID == "":
		return ""
	case r.WorkerReachable != nil && !*r.WorkerReachable:
		return workerNameOf(s, r.WorkerID) + " (unreachable)"
	}
	return workerNameOf(s, r.WorkerID)
}

func reachTone(r v1.Run) Tone {
	if r.WorkerReachable != nil && !*r.WorkerReachable {
		return ToneDanger
	}
	return ToneText
}

func publicationOf(r v1.Run) string {
	if r.Publication == domain.PublicationNone {
		return ""
	}
	return string(r.Publication)
}

func evaluationOf(r v1.Run) string {
	if r.Evaluation == domain.EvaluationNone {
		return ""
	}
	return string(r.Evaluation)
}

func daemonOf(w v1.Worker) string {
	if w.Compatible {
		return w.DaemonVersion
	}
	return w.DaemonVersion + " (incompatible)"
}

func versionTone(w v1.Worker) Tone {
	if w.Compatible {
		return ToneText
	}
	return ToneDanger
}

func taskTone(s domain.TaskState) Tone {
	switch s {
	case domain.TaskRunning:
		return ToneSuccess
	case domain.TaskWaiting:
		return ToneWarning
	case domain.TaskFailed:
		return ToneDanger
	case domain.TaskBlocked, domain.TaskQueued, domain.TaskCancelled, domain.TaskArchived:
		return ToneFaint
	}
	return ToneText
}

func waitingTone(r domain.WaitingReason) Tone {
	if r == domain.WaitingInput || r == domain.WaitingDecision {
		return ToneWarning
	}
	return ToneFaint
}

func runTone(s domain.RunState) Tone {
	switch s {
	case domain.RunRunning:
		return ToneSuccess
	case domain.RunWaitingInput:
		return ToneWarning
	case domain.RunFailed, domain.RunLost:
		return ToneDanger
	case domain.RunQueued, domain.RunPreparing, domain.RunSuspended, domain.RunCancelled:
		return ToneFaint
	}
	return ToneText
}

func publicationTone(p domain.Publication) Tone {
	if p == domain.PublicationFailed {
		return ToneDanger
	}
	return ToneText
}

func workerTone(s domain.WorkerState) Tone {
	switch s {
	case domain.WorkerOnline:
		return ToneSuccess
	case domain.WorkerDegraded, domain.WorkerDraining:
		return ToneWarning
	case domain.WorkerOffline:
		return ToneDanger
	case domain.WorkerJoining, domain.WorkerDrained, domain.WorkerRemoved:
		return ToneFaint
	}
	return ToneText
}

func healthTone(status string) Tone {
	switch status {
	case v1.HealthOK:
		return ToneSuccess
	case "":
		return ToneFaint
	}
	return ToneWarning
}
