package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"time"

	"github.com/spf13/cobra"

	v1 "github.com/Autarquico/codefabric/internal/api/v1"
	"github.com/Autarquico/codefabric/internal/client"
	"github.com/Autarquico/codefabric/internal/domain"
)

const defaultEventTail = 20

func newEventsCmd() *cobra.Command {
	var (
		q      client.EventQuery
		follow bool
		since  string
	)
	cmd := &cobra.Command{
		Use:   "events",
		Short: "Show the events of the Controller, and follow them as they happen",
		Long: "Without --follow, the last events (20 by default) or, with --after, a page of them from a cursor.\n" +
			"With --follow, the last events and then the ones that happen, until interrupted. With --json and\n" +
			"--follow the output is JSON Lines: one event per line, which is the one exception to \"one JSON\n" +
			"value\"; the cursor of the last event resumes it with --after.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if since != "" {
				at, err := parseSince(since, time.Now())
				if err != nil {
					return err
				}
				q.Since = at
			}
			c, err := openClient(cmd)
			if err != nil {
				return err
			}
			if follow {
				return followEvents(cmd, c, q)
			}
			if q.After == "" && q.Tail == 0 {
				q.Tail = defaultEventTail
			}
			if q.Tail > 0 && q.Limit > 0 { // --limit sizes the tail, too
				q.Tail, q.Limit = q.Limit, 0
			}
			page, err := c.Events(cmd.Context(), q)
			if err != nil {
				return err
			}
			return emit(cmd, page, func(out io.Writer) error {
				for _, e := range page.Events {
					printEvent(out, e)
				}
				return nil
			})
		},
	}
	f := cmd.Flags()
	f.BoolVarP(&follow, "follow", "f", false, "keep showing events as they happen")
	f.StringVar(&q.Project, flagProject, "", "only the events of this Project")
	f.StringVar(&q.Task, "task", "", "only the events of this Task")
	f.StringVar(&q.Run, "run", "", "only the events of this Run")
	f.StringVar(&q.Worker, "worker", "", "only the events of this Worker")
	f.StringArrayVar(&q.Types, "type", nil, "only events of this type (repeatable)")
	f.StringVar(&q.Level, "level", "", "only events of this level or above: debug, info, warn, error")
	f.StringVar(&since, "since", "", "only events since this long ago (2h) or this time (RFC 3339)")
	f.StringVar(&q.After, "after", "", "start after this cursor")
	f.IntVar(&q.Limit, "limit", 0, "how many events (default: 20 of the tail)")
	return cmd
}

// parseSince reads "2h" as that long before now, and anything else as an RFC 3339 time.
func parseSince(text string, now time.Time) (time.Time, error) {
	if d, err := time.ParseDuration(text); err == nil && d > 0 {
		return now.Add(-d), nil
	}
	at, err := time.Parse(time.RFC3339, text)
	if err != nil {
		return time.Time{}, domain.NewError(domain.CodeInvalidRequest, "--since is a duration such as 2h or an RFC 3339 time", nil)
	}
	return at, nil
}

func printEvent(out io.Writer, e v1.Event) {
	fmt.Fprintf(out, "%d  %s  %-5s  %s  %s\n", e.Sequence, e.ReceivedAt.Local().Format(time.TimeOnly),
		printable(e.Level), printable(string(e.Type)), printable(e.Message))
}

// followEvents shows the tail and then the stream from the cursor of the tail,
// so nothing between them is lost or repeated. A cursor the Controller no longer
// holds is a loss it says out loud, on stderr, before it goes on from the present.
func followEvents(cmd *cobra.Command, c *client.Client, q client.EventQuery) error {
	asJSON, _ := cmd.Flags().GetBool(flagJSON)
	out, errOut := cmd.OutOrStdout(), cmd.ErrOrStderr()
	show := func(e v1.Event) error {
		if asJSON {
			return encodeJSON(out, e)
		}
		printEvent(out, e)
		return nil
	}
	ctx := cmd.Context()
	if q.After == "" {
		tail := q
		tail.Tail, tail.Limit = defaultEventTail, 0
		if q.Limit > 0 {
			tail.Tail = q.Limit
		}
		page, err := c.Events(ctx, tail)
		if err != nil {
			return err
		}
		for _, e := range page.Events {
			if err := show(e); err != nil {
				return err
			}
		}
		q.After = page.Cursor
	}
	q.Tail, q.Limit = 0, 0
	for {
		err := c.FollowEvents(ctx, q, func(e v1.Event) error {
			q.After = fmt.Sprint(e.Sequence)
			return show(e)
		})
		var coded *domain.Error
		switch {
		case err == nil, errors.Is(err, context.Canceled):
			return nil
		case errors.As(err, &coded) && coded.Code == domain.CodeCursorExpired:
			if purged, found := coded.Details["purged_through"].(float64); found {
				fmt.Fprintf(errOut, "warning: events between %s and %.0f were purged; continuing from now\n", q.After, purged)
			} else {
				fmt.Fprintf(errOut, "warning: events after %s were purged; continuing from now\n", q.After)
			}
			now, nerr := c.Events(ctx, client.EventQuery{Tail: 1})
			if nerr != nil {
				return nerr
			}
			q.After = now.Cursor
		default:
			return err
		}
	}
}
