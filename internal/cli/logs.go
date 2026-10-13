package cli

import (
	"bytes"
	"context"
	"errors"
	"fmt"

	"github.com/spf13/cobra"

	v1 "github.com/Autarquico/codefabric/internal/api/v1"
	"github.com/Autarquico/codefabric/internal/client"
	"github.com/Autarquico/codefabric/internal/domain"
)

const defaultLogLines = 200

func newLogsCmd() *cobra.Command {
	var (
		run    string
		follow bool
		raw    bool
		tail   int
		other  struct{ worker, project, task, level, since string }
	)
	cmd := &cobra.Command{
		Use:   "logs --run <run>",
		Short: "Show the output of a Run, and follow it",
		Long: "Show the last lines of the output of a Run (200 by default) and, with --follow, what it prints\n" +
			"next, until the output ends or you interrupt. The Controller keeps the last MiB of each Run's\n" +
			"output until the log storage of Phase 4. Escape sequences and control characters are removed so\n" +
			"that the Run cannot act on your terminal; --raw passes them through.\n" +
			"With --json: the page as one JSON value, or with --follow JSON Lines of chunks (data in base64).",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if run == "" || other.worker != "" || other.project != "" || other.task != "" || other.level != "" || other.since != "" {
				return domain.NewError(domain.CodeNotImplemented,
					"only the output of one Run is available (cf logs --run <run>); the other logs and their filters arrive with structured logs in Phase 4", nil)
			}
			if tail < 1 {
				return domain.NewError(domain.CodeInvalidRequest, "--tail is a number of lines of at least 1", nil)
			}
			c, err := openClient(cmd)
			if err != nil {
				return err
			}
			return showLogs(cmd, c, run, tail, follow, raw)
		},
	}
	f := cmd.Flags()
	f.StringVar(&run, "run", "", "the Run whose output to show")
	f.BoolVarP(&follow, "follow", "f", false, "keep showing the output as it arrives")
	f.IntVar(&tail, "tail", defaultLogLines, "how many lines from the end to start with")
	f.BoolVar(&raw, "raw", false, "do not remove escape sequences and control characters")
	f.StringVar(&other.worker, "worker", "", "not available yet")
	f.StringVar(&other.project, flagProject, "", "not available yet")
	f.StringVar(&other.task, "task", "", "not available yet")
	f.StringVar(&other.level, "level", "", "not available yet")
	f.StringVar(&other.since, "since", "", "not available yet")
	for _, name := range []string{"worker", flagProject, "task", "level", "since"} {
		_ = f.MarkHidden(name)
	}
	return cmd
}

// lastLines is the last n lines of data, a final line without its newline included.
func lastLines(data []byte, n int) []byte {
	end := len(data)
	if end > 0 && data[end-1] == '\n' {
		end--
	}
	for i := end - 1; i >= 0; i-- {
		if data[i] == '\n' {
			if n--; n == 0 {
				return data[i+1:]
			}
		}
	}
	return data
}

func countLines(data []byte) int {
	n := bytes.Count(data, []byte{'\n'})
	if len(data) > 0 && data[len(data)-1] != '\n' {
		n++
	}
	return n
}

// readTail gathers the last lines of the output: the tail in bytes, and pages
// further back while there are fewer lines than asked for and the Controller
// still has older bytes.
func readTail(ctx context.Context, c *client.Client, run string, lines int) (v1.RunOutput, []byte, error) {
	first, err := c.RunOutput(ctx, run, client.OutputQuery{})
	if err != nil {
		return first, nil, err
	}
	data, from := first.Data, first.Offset
	for countLines(data) <= lines && from > first.Start {
		to := from
		from -= min(from-first.Start, uint64(v1.OutputPageBytes)) // not below Start, and never below zero
		offset := from
		page, err := c.RunOutput(ctx, run, client.OutputQuery{Epoch: first.LeaseEpoch, Offset: &offset, Limit: to - from})
		if err != nil {
			return first, nil, err
		}
		data = append(append([]byte{}, page.Data...), data...)
		from = page.Offset
	}
	first.Offset = from
	return first, lastLines(data, lines), nil
}

func showLogs(cmd *cobra.Command, c *client.Client, run string, lines int, follow, raw bool) error {
	ctx := cmd.Context()
	asJSON, _ := cmd.Flags().GetBool(flagJSON)
	out, errOut := cmd.OutOrStdout(), cmd.ErrOrStderr()

	page, data, err := readTail(ctx, c, run, lines)
	if err != nil {
		return err
	}
	if asJSON && !follow {
		page.Data = data
		page.Offset = page.End - uint64(len(data))
		return encodeJSON(out, page)
	}
	san := &sanitizer{raw: raw}
	if asJSON {
		// JSON Lines: the tail is the first chunk, so a consumer sees all of it.
		chunk := v1.OutputChunk{RunID: page.RunID, LeaseEpoch: page.LeaseEpoch, Offset: page.End - uint64(len(data)), Data: data}
		if err := encodeJSON(out, chunk); err != nil {
			return err
		}
	} else if _, err := out.Write(san.Filter(data)); err != nil {
		return err
	}
	if page.Truncated && !asJSON {
		fmt.Fprintln(errOut, "note: older output is no longer held by the Controller")
	}
	if !follow {
		return nil
	}

	query := client.OutputQuery{Epoch: page.LeaseEpoch}
	end := page.End
	query.Offset = &end
	for {
		ending, err := c.FollowOutput(ctx, run, query, client.OutputHandler{
			Begin: func(v1.OutputBegin) error { san.Reset(); return nil },
			Chunk: func(ch v1.OutputChunk) error {
				if ch.Gap {
					san.Reset()
					if !asJSON {
						fmt.Fprintln(errOut, "note: some output was skipped")
					}
				}
				if asJSON {
					return encodeJSON(out, ch)
				}
				_, err := out.Write(san.Filter(ch.Data))
				return err
			},
		})
		switch {
		case err == nil && ending.End != nil && ending.End.Reason == v1.EndNewerEpoch:
			fmt.Fprintf(errOut, "note: the Run went on under a new lease; following it (epoch %d)\n", ending.End.LeaseEpoch)
			zero := uint64(0)
			query = client.OutputQuery{Epoch: ending.End.LeaseEpoch, Offset: &zero}
			san.Reset()
		case err == nil && ending.End != nil:
			fmt.Fprintln(errOut, "note: the Run is over and no more output is coming")
			return nil
		case err == nil, errors.Is(err, context.Canceled):
			return nil
		default:
			return err
		}
	}
}
