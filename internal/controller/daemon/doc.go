// Package daemon runs the Controller process and owns the `cf controller`
// commands. They live here, not in internal/cli, because they need the
// Controller packages (ADR 0001); cmd/cf attaches them to the command tree.
package daemon
