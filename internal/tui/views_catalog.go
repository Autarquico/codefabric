package tui

import (
	"slices"
	"strconv"
	"strings"

	v1 "github.com/Autarquico/codefabric/internal/api/v1"
	"github.com/Autarquico/codefabric/internal/cli/actions"
)

// Workflows and Projects (§19.4): what is versioned and what it is applied to,
// and the repositories work is done in.

// adoptIDs are the actions that take a snapshot of the machine the person is
// on: they need the CLI's own review (diff and secret refs) before `yes`, which
// a run from here cannot show, so the TUI does not list them.
var adoptIDs = []string{"workflow.adopt", "workflow.commit"}

func workflowsDef() viewDef[WorkflowRow] {
	return viewDef[WorkflowRow]{
		route: RouteWorkflows, title: "Workflows", kind: actions.Workflow, history: true, forms: true,
		columns: []Column{{"Name", 18}, {"Head", 5}, {"On head", 8}, {"Drift", 9}, {"Created", 0}},
		empty:   "No workflows yet",
		skip:    func(d actions.Descriptor) bool { return slices.Contains(adoptIDs, d.ID) },
		// Rolling back acts on a Worker's harness, not on one Workflow: it has
		// no target, so the view names it for what it is. Apply is a capital:
		// "a" is Activity on the home.
		also:      []string{"workflow.rollback"},
		retitle:   map[string]string{"workflow.rollback": "Roll back a Worker's harness"},
		holds:     rollbackHolds,
		cliNote:   workflowCLINote,
		shortcuts: []shortcut{{"A", "Apply", "workflow.apply"}, {"s", "Sync", "workflow.sync"}, {"R", "Rollback", "workflow.rollback"}},
		needs:     []Route{RouteWorkers},
		list:      func(s *State) *Feed[WorkflowRow] { return &s.Workflows },
		source:    func(f Feeds) FeedSource[WorkflowRow] { return f.Workflows },
		id:        func(w WorkflowRow) string { return w.Summary.Name },
		suggest:   suggestWorkflowInputs,
		row: func(w WorkflowRow, c RenderContext) Row {
			onHead, drifted := w.coverage()
			return Row{
				ID: w.Summary.Name, Label: w.Summary.Name, Stamp: strconv.Itoa(w.Summary.Head) + "/" + w.Summary.HeadDigest,
				Cells: []Cell{
					{w.Summary.Name, ToneText},
					{headOf(w.Summary), ToneFaint},
					{onHead, ToneFaint},
					{drifted, driftTone(w)},
					{activityTime(w.Summary.CreatedAt, c.Now, c.Loc), ToneFaint},
				},
			}
		},
		detail: func(w WorkflowRow, c RenderContext) []Field {
			fields := []Field{
				{"Workflow", w.Summary.Name, ToneText},
				{"Head", joinParts(headOf(w.Summary), shortDigest(w.Summary.HeadDigest)), ToneText},
				{"Created", activityTime(w.Summary.CreatedAt, c.Now, c.Loc), ToneFaint},
			}
			for i, s := range w.Held {
				label := ""
				if i == 0 {
					label = "Applied"
				}
				// The pair is the identity of the row: it leads the value, whole.
				fields = append(fields, Field{label, s.Worker + "/" + s.Harness + " · " + heldOf(s, w.Summary.Head), heldTone(s, w.Summary.Head)})
			}
			return fields
		},
	}
}

// workflowCLINote is what the Workflows view leaves to the CLI, with the
// commands for the selected Workflow: adopt and commit take a snapshot of the
// machine and need the review the CLI shows; diff and explain print plans.
func workflowCLINote(row Row) string {
	name := row.ID
	target := " " + name + " --worker=<worker> --harness=<harness>"
	return "cf workflow adopt " + name + " · cf workflow commit " + name +
		" · cf workflow diff" + target + " · cf workflow explain" + target
}

// rollbackHolds says what the Worker and harness of a rollback hold now.
func rollbackHolds(s *State, d actions.Descriptor, values map[string]string) string {
	if d.ID != "workflow.rollback" {
		return ""
	}
	for _, w := range s.Workflows.Items {
		for _, h := range w.Held {
			if h.Worker == values["worker"] && h.Harness == values["harness"] {
				return w.Summary.Name + " v" + strconv.Itoa(h.AppliedVersion) + ", which goes back to the version it held before"
			}
		}
	}
	return "no Workflow is known on that Worker and harness"
}

// coverage says on how many of the Workers that hold the Workflow it is at its
// head, and how many of them have drifted from it.
func (w WorkflowRow) coverage() (onHead, drifted string) {
	if len(w.Held) == 0 {
		return noValue, noValue
	}
	head, drift := 0, 0
	for _, s := range w.Held {
		if s.AppliedVersion == w.Summary.Head {
			head++
		}
		if len(s.Drift) > 0 {
			drift++
		}
	}
	onHead = strconv.Itoa(head) + "/" + strconv.Itoa(len(w.Held))
	if drift == 0 {
		return onHead, "none"
	}
	return onHead, strconv.Itoa(drift) + " drifted"
}

func driftTone(w WorkflowRow) Tone {
	for _, s := range w.Held {
		if len(s.Drift) > 0 {
			return ToneWarning
		}
	}
	return ToneFaint
}

func headOf(w v1.WorkflowSummary) string {
	if w.Head == 0 {
		return noValue
	}
	return "v" + strconv.Itoa(w.Head)
}

// shortDigest is "sha256:" and the first 12 hex digits.
func shortDigest(d string) string {
	if kind, hex, ok := strings.Cut(d, ":"); ok && len(hex) > 12 {
		return kind + ":" + hex[:12]
	}
	return d
}

// heldOf says what a Worker holds of a harness's Workflow: its version, and
// what is off about it.
func heldOf(s v1.WorkflowState, head int) string {
	version := noValue
	if s.AppliedVersion > 0 {
		version = "v" + strconv.Itoa(s.AppliedVersion)
		if s.AppliedVersion != head {
			version += " (head v" + strconv.Itoa(head) + ")"
		}
	}
	var drifted []string
	for _, d := range s.Drift {
		drifted = append(drifted, d.Component)
	}
	parts := []string{version}
	if len(drifted) > 0 {
		parts = append(parts, "drift: "+strings.Join(drifted, ", "))
	}
	if s.Partial {
		parts = append(parts, "partial")
	}
	if s.ApplyPending != "" {
		parts = append(parts, "apply pending")
	}
	return joinParts(parts...)
}

func heldTone(s v1.WorkflowState, head int) Tone {
	if len(s.Drift) > 0 || s.Partial || s.AppliedVersion != head {
		return ToneWarning
	}
	return ToneText
}

// suggestWorkflowInputs proposes what a form can know: the Workers by name, and
// the harnesses that the Workflow (else any Workflow) is held under.
func suggestWorkflowInputs(s *State, workflow, input string) []string {
	var out []string
	switch input {
	case "worker":
		for _, w := range s.Workers.Items {
			out = append(out, w.Name)
		}
	case "harness":
		for _, only := range []bool{true, false} {
			for _, w := range s.Workflows.Items {
				if only && w.Summary.Name != workflow {
					continue
				}
				for _, h := range w.Held {
					if !slices.Contains(out, h.Harness) {
						out = append(out, h.Harness)
					}
				}
			}
			if len(out) > 0 {
				break
			}
		}
	}
	return out
}

func projectsDef() viewDef[v1.Project] {
	return viewDef[v1.Project]{
		route: RouteProjects, title: "Projects", kind: actions.Project, forms: true,
		columns: []Column{{"Name", 16}, {"ID", 9}, {"Branch", 8}, {"Origin", 8}, {"Repo", 0}},
		empty:   "No projects yet",
		// Editing takes optional values that a form of required inputs cannot ask for.
		skip:   func(d actions.Descriptor) bool { return d.ID == "project.set" },
		list:   func(s *State) *Feed[v1.Project] { return &s.Projects },
		source: func(f Feeds) FeedSource[v1.Project] { return f.Projects },
		id:     func(p v1.Project) string { return p.ID },
		row: func(p v1.Project, _ RenderContext) Row {
			return Row{
				ID: p.ID, Label: p.Name + " (" + p.Short + ")", Stamp: p.CreatedAt.String(),
				Cells: []Cell{
					{p.Name, ToneText},
					{p.Short, ToneFaint},
					{p.DefaultBranch, ToneFaint},
					{originAuth(p), ToneFaint},
					{p.Repo, ToneFaint},
				},
			}
		},
		detail: func(p v1.Project, c RenderContext) []Field {
			return []Field{
				{"Project", p.Name + " (" + p.Short + ")", ToneText},
				{"Repo", p.Repo, ToneText},
				{"Aliases", strings.Join(p.RepoAliases, ", "), ToneFaint},
				{"Branch", p.DefaultBranch, ToneText},
				{"Workflow", p.DefaultWorkflow, ToneText},
				{"Environment", p.DefaultEnvironment, ToneText},
				{"Mode", string(p.Defaults.Mode), ToneText},
				{"Completion", string(p.Defaults.Completion), ToneText},
				{"Repo config", string(p.RepoConfig), ToneText},
				{"Origin auth", originAuthLong(p), ToneText},
				{"Created", activityTime(p.CreatedAt, c.Now, c.Loc), ToneFaint},
			}
		},
	}
}

// originAuth says whether the fetch of the origin is authenticated; the ref of
// the secret is a name, never the secret.
func originAuth(p v1.Project) string {
	if p.OriginSecret == "" {
		return "none"
	}
	return "secret"
}

func originAuthLong(p v1.Project) string {
	if p.OriginSecret == "" {
		return "none (the repo is fetched without credentials)"
	}
	return "secret " + p.OriginSecret
}
