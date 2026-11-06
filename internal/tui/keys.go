package tui

// Hint is one entry of the key help of §19.3: the key and what it opens.
type Hint struct {
	Key   string
	Label string
}

// Route is the name of a screen that a key opens.
type Route string

// The screens of §19.3. Only the ones registered in the router are reachable;
// the others stay in the help, faint, until their milestone lands.
const (
	RouteTasks     Route = "tasks"
	RouteWorkers   Route = "workers"
	RouteModels    Route = "models"
	RouteRuns      Route = "runs"
	RouteWorkflows Route = "workflows"
	RouteProjects  Route = "projects"
	RouteActivity  Route = "activity"
	RouteLogs      Route = "logs"
	RouteInbox     Route = "inbox"
	RouteSearch    Route = "search"
)

type binding struct {
	Hint
	Route Route
}

// bindings is the key help of §19.3, in the order and rows it shows them.
var bindings = [][]binding{
	{
		{Hint{"t", "Tasks"}, RouteTasks},
		{Hint{"w", "Workers"}, RouteWorkers},
		{Hint{"m", "Models"}, RouteModels},
		{Hint{"r", "Runs"}, RouteRuns},
		{Hint{"f", "Workflows"}, RouteWorkflows},
	},
	{
		{Hint{"p", "Projects"}, RouteProjects},
		{Hint{"a", "Activity"}, RouteActivity},
		{Hint{"l", "Logs"}, RouteLogs},
		{Hint{"i", "Inbox"}, RouteInbox},
		{Hint{"/", "Search"}, RouteSearch},
	},
}

// Action is what a key press asks of the shell.
type Action int

// Actions of the shell.
const (
	ActionNone Action = iota
	ActionQuit
	ActionBack
	ActionOpen
)

// resolve says what a key press (as bubbletea spells it) asks for. The route is
// set only for ActionOpen.
func resolve(key string) (Action, Route) {
	switch key {
	case "q", "ctrl+c":
		return ActionQuit, ""
	case "esc":
		return ActionBack, ""
	}
	for _, row := range bindings {
		for _, b := range row {
			if b.Key == key {
				return ActionOpen, b.Route
			}
		}
	}
	return ActionNone, ""
}

// labelOf is the name that the key help gives a route.
func labelOf(route Route) string {
	for _, row := range bindings {
		for _, b := range row {
			if b.Route == route {
				return b.Label
			}
		}
	}
	return string(route)
}
