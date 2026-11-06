// Package tui is the terminal UI of CodeFabric (§19). Like the CLI it is a
// client of internal/client: it holds no business logic of its own.
package tui

import (
	"context"
	"fmt"

	tea "charm.land/bubbletea/v2"

	"github.com/Autarquico/codefabric/internal/cli/actions"
	"github.com/Autarquico/codefabric/internal/client"
)

// Run opens the TUI over the Controller of c and returns when the user quits,
// ctx ends, or the Controller revokes the credentials. The actions of its views
// run with the composition of the binary, in runner.
func Run(ctx context.Context, c *client.Client, runner actions.Runner) error {
	return run(ctx, SubscribeOverview(c), Options{Backend: NewBackend(ctx, c, runner), Feeds: SubscribeFeeds(c)})
}

// run is Run over any Source and program options (tests drive it headless).
func run(ctx context.Context, source Source, opts Options, programOpts ...tea.ProgramOption) error {
	m := New(opts)
	p := tea.NewProgram(m, append(programOpts, tea.WithContext(ctx))...)
	m.bind(ctx, p.Send)
	defer m.stop()

	subCtx, stop := context.WithCancel(ctx)
	defer stop()
	done := make(chan struct{})
	go func() {
		defer close(done)
		// A panic here would end the process with the terminal still in raw
		// mode and the alternate screen; turn it into the end of the program.
		defer func() {
			if r := recover(); r != nil {
				p.Send(fatalMsg{fmt.Errorf("overview subscription: %v", r)})
			}
		}()
		err := source(subCtx, func(u OverviewUpdate) { p.Send(updateMsg(u)) })
		if err != nil && subCtx.Err() == nil {
			p.Send(fatalMsg{err})
		}
	}()

	_, err := p.Run()
	stop()
	<-done
	if err != nil {
		return err
	}
	return m.Err()
}
