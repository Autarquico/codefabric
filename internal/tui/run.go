// Package tui is the terminal UI of CodeFabric (§19). Like the CLI it is a
// client of internal/client: it holds no business logic of its own.
package tui

import (
	"context"
	"os"

	tea "charm.land/bubbletea/v2"

	"github.com/Autarquico/codefabric/internal/client"
)

// Run opens the TUI over the Controller of c and returns when the user quits,
// ctx ends, or the Controller revokes the credentials.
func Run(ctx context.Context, c *client.Client) error {
	_, noColor := os.LookupEnv("NO_COLOR")
	return run(ctx, SubscribeOverview(c), Options{Mono: noColor})
}

// run is Run over any Source and program options (tests drive it headless).
func run(ctx context.Context, source Source, opts Options, programOpts ...tea.ProgramOption) error {
	m := New(opts)
	p := tea.NewProgram(m, append(programOpts, tea.WithContext(ctx))...)

	subCtx, stop := context.WithCancel(ctx)
	defer stop()
	done := make(chan struct{})
	go func() {
		defer close(done)
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
