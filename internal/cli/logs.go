package cli

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/spf13/cobra"

	v1 "github.com/Autarquico/codefabric/internal/api/v1"
	"github.com/Autarquico/codefabric/internal/client"
	"github.com/Autarquico/codefabric/internal/domain"
)

const defaultLogLines = 200

// maxTailBytes bounds what the tail of a Run's output reads and keeps: a few
// lines of enormous length must not load the output of a Run (up to 100 MB).
const maxTailBytes = 4 << 20

func newLogsCmd() *cobra.Command {
	var (
		run        string
		follow     bool
		raw        bool
		structured bool
		tail       int
		q          client.LogQuery
		since      string
	)
	cmd := &cobra.Command{
		Use:   "logs [--run <run>]",
		Short: "Show the logs of the Controller and the Workers, or the output of a Run, and follow them",
		Long: "There are two things to read, and they are never mixed:\n" +
			"  - the structured logs of the Controller and of the Workers: lines with a time, a level, a\n" +
			"    source and attributes, in the order the Controller received them. This is what you get\n" +
			"    without --run, or with any of --worker, --project, --task, --level, --since, --after,\n" +
			"    --limit, or with --structured. The filters combine.\n" +
			"  - the output of one Run: what its harness printed, byte for byte. This is what you get with\n" +
			"    --run alone (and --tail, --follow, --raw). Add --structured to see the structured logs of\n" +
			"    that Run instead.\n" +
			"Without --follow, the last lines (200 by default; --limit sizes them) or, with --after, a page\n" +
			"from a cursor. With --follow, those and then what comes next, until interrupted (a Run's output:\n" +
			"until it ends). Escape sequences and control characters are removed so that a Run cannot act on\n" +
			"your terminal; --raw passes them through.\n" +
			"With --json: the page as one JSON value; with --follow, JSON Lines (one log per line; for a\n" +
			"Run's output, chunks with data in base64). The cursor of the last log resumes with --after.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			f := cmd.Flags()
			tailSet := f.Changed("tail")
			if tailSet && tail < 1 {
				return domain.NewError(domain.CodeInvalidRequest, "--tail is a number of lines of at least 1", nil)
			}
			if f.Changed("limit") && q.Limit < 0 {
				return domain.NewError(domain.CodeInvalidRequest, "--limit is a number of logs of at least 0", nil)
			}
			filtered := false
			for _, name := range []string{"worker", flagProject, "task", "level", "since", "after", "limit"} {
				filtered = filtered || f.Changed(name)
			}
			if run != "" && !structured && !filtered {
				c, err := openClient(cmd)
				if err != nil {
					return err
				}
				return showLogs(cmd, c, run, tail, follow, raw)
			}
			if since != "" {
				at, err := parseSince(since, time.Now())
				if err != nil {
					return err
				}
				q.Since = at
			}
			q.Run = run
			switch {
			case q.After != "":
				if tailSet {
					return domain.NewError(domain.CodeInvalidRequest, "--after reads a page from a cursor: use --limit, not --tail", nil)
				}
			case q.Limit > 0 && tailSet:
				return domain.NewError(domain.CodeInvalidRequest, "--tail and --limit both size the last lines: use one", nil)
			case q.Limit > 0:
				q.Tail, q.Limit = q.Limit, 0
			default:
				q.Tail = tail
			}
			c, err := openClient(cmd)
			if err != nil {
				return err
			}
			if follow {
				return followStructuredLogs(cmd, c, q, raw)
			}
			page, err := c.Logs(cmd.Context(), q)
			if err != nil {
				return err
			}
			return emit(cmd, page, func(out io.Writer) error {
				for _, l := range page.Logs {
					if err := printLog(out, l, raw); err != nil {
						return err
					}
				}
				return nil
			})
		},
	}
	f := cmd.Flags()
	f.StringVar(&run, "run", "", "the Run whose output to show (with --structured or a filter: whose structured logs)")
	f.BoolVar(&structured, "structured", false, "with --run, show the structured logs of the Run instead of its output")
	f.BoolVarP(&follow, "follow", "f", false, "keep showing what arrives")
	f.IntVar(&tail, "tail", defaultLogLines, "how many lines from the end to start with")
	f.BoolVar(&raw, "raw", false, "do not remove escape sequences and control characters")
	f.StringVar(&q.Worker, "worker", "", "only the logs of this Worker")
	f.StringVar(&q.Project, flagProject, "", "only the logs of this Project")
	f.StringVar(&q.Task, "task", "", "only the logs of this Task")
	f.StringVar(&q.Level, "level", "", "only logs of this level or above: debug, info, warn, error")
	f.StringVar(&since, "since", "", "only logs since this long ago (2h) or this time (RFC 3339)")
	f.StringVar(&q.After, "after", "", "start after this cursor")
	f.IntVar(&q.Limit, "limit", 0, "how many logs (default: 200 of the tail)")
	return cmd
}

// terminalSafe is s without what could act on the terminal, and on one line, so
// that a field cannot pass for another log line.
func terminalSafe(s string, raw bool) string {
	if raw {
		return s
	}
	return strings.Map(func(r rune) rune {
		if r == '\n' || r == '\t' {
			return ' '
		}
		return r
	}, string((&sanitizer{}).Filter([]byte(s))))
}

func printLog(out io.Writer, l v1.Log, raw bool) error {
	safe := func(s string) string { return terminalSafe(s, raw) }
	source := safe(l.Source)
	if l.RunID != "" {
		source += "/" + safe(l.RunID)
	}
	line := fmt.Sprintf("%d  %s  %-5s  %s  %s", l.Sequence, l.ReceivedAt.Local().Format(time.TimeOnly), safe(l.Level), source, safe(l.Message))
	if len(l.Attrs) > 0 && string(l.Attrs) != "{}" {
		line += "  " + safe(string(l.Attrs))
	}
	_, err := io.WriteString(out, line+"\n")
	return err
}

// followStructuredLogs shows the tail and then the stream from the cursor of
// the tail, so nothing between them is lost or repeated. A cursor the
// Controller no longer holds is a loss it says on stderr before going on from now.
func followStructuredLogs(cmd *cobra.Command, c *client.Client, q client.LogQuery, raw bool) error {
	asJSON, _ := cmd.Flags().GetBool(flagJSON)
	out, errOut := cmd.OutOrStdout(), cmd.ErrOrStderr()
	show := func(l v1.Log) error {
		if asJSON {
			return encodeJSON(out, l)
		}
		return printLog(out, l, raw)
	}
	ctx := cmd.Context()
	if q.After == "" {
		page, err := c.Logs(ctx, q)
		if err != nil {
			return err
		}
		for _, l := range page.Logs {
			if err := show(l); err != nil {
				return err
			}
		}
		q.After = page.Cursor
	}
	q.Tail, q.Limit = 0, 0
	for {
		err := c.FollowLogs(ctx, q, func(l v1.Log) error {
			if err := show(l); err != nil {
				return err
			}
			q.After = strconv.FormatUint(l.Sequence, 10)
			return nil
		})
		var coded *domain.Error
		switch {
		case err == nil, errors.Is(err, context.Canceled):
			return nil
		case errors.As(err, &coded) && coded.Code == domain.CodeCursorExpired:
			fmt.Fprintf(errOut, "warning: logs after %s were purged; continuing from now\n", q.After)
			now, nerr := c.Logs(ctx, client.LogQuery{Tail: 1})
			if nerr != nil {
				return nerr
			}
			q.After = now.Cursor
		default:
			return err
		}
	}
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

// outputReader is what readTail needs of the Controller.
type outputReader interface {
	RunOutput(ctx context.Context, ref string, q client.OutputQuery) (v1.RunOutput, error)
}

// readTail gathers the last lines of the output: the tail in bytes, and pages
// further back while there are fewer lines than asked for, the Controller still
// has older bytes and less than maxTailBytes has been read. The tail is one run
// of bytes: it does not cross a gap the Worker left when it discarded output,
// and gap says there is output before one. clipped says that the lines asked
// for were not all reached, or were cut, because of the bound on the bytes.
func readTail(ctx context.Context, c outputReader, run string, lines int) (page v1.RunOutput, tail []byte, clipped, gap bool, err error) {
	first, err := c.RunOutput(ctx, run, client.OutputQuery{})
	if err != nil {
		return first, nil, false, false, err
	}
	data, from := first.Data, first.Offset
	gap = first.Truncated && from > first.Start // the Controller began the tail after a gap
	for !gap && countLines(data) <= lines && from > first.Start {
		if len(data) >= maxTailBytes {
			clipped = true
			break
		}
		to := from
		from -= min(from-first.Start, uint64(v1.OutputPageBytes)) // not below Start, and never below zero
		offset := from
		older, err := c.RunOutput(ctx, run, client.OutputQuery{Epoch: first.LeaseEpoch, Offset: &offset, Limit: to - from})
		if err != nil {
			return first, nil, false, false, err
		}
		if older.Offset >= to || older.Truncated { // the page began in a gap: nothing before it is contiguous
			gap = true
			break
		}
		data = append(append(make([]byte, 0, len(older.Data)+len(data)), older.Data...), data...)
		from = older.Offset
	}
	first.Offset = from
	tail = lastLines(data, lines)
	if len(tail) > maxTailBytes { // a few very long lines: the end of them
		tail = tail[len(tail)-maxTailBytes:]
		clipped = true
	}
	return first, tail, clipped, gap, nil
}

// startAtRune is data without the continuation bytes of a character that the
// bound on the tail cut, for the sanitised rendering only: raw output and JSON
// keep every byte. keep says the data was not cut or is not sanitised.
func startAtRune(data []byte, keep bool) []byte {
	for !keep && len(data) > 0 && !utf8.RuneStart(data[0]) {
		data = data[1:]
	}
	return data
}

func showLogs(cmd *cobra.Command, c *client.Client, run string, lines int, follow, raw bool) error {
	ctx := cmd.Context()
	asJSON, _ := cmd.Flags().GetBool(flagJSON)
	out, errOut := cmd.OutOrStdout(), cmd.ErrOrStderr()

	page, data, clipped, gap, err := readTail(ctx, c, run, lines)
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
	} else if _, err := out.Write(san.Filter(startAtRune(data, raw || !clipped))); err != nil {
		return err
	}
	switch {
	case gap && !asJSON:
		fmt.Fprintln(errOut, "note: there is older output before a gap: the Worker discarded output when its journal degraded")
	case page.Truncated && !asJSON:
		fmt.Fprintln(errOut, "note: older output is no longer held by the Controller")
	}
	if clipped && !asJSON {
		fmt.Fprintf(errOut, "note: only the last %d MiB of the output were read; --tail reaches no further\n", maxTailBytes>>20)
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
			Chunk: func(ch v1.OutputChunk) error { return writeChunk(out, errOut, san, ch, asJSON) },
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

// writeChunk delivers a chunk of a follow, saying first that output is missing
// before it when the Controller does not hold it (the Worker discarded it).
func writeChunk(out, errOut io.Writer, san *sanitizer, ch v1.OutputChunk, asJSON bool) error {
	if ch.Gap {
		san.Reset()
		if !asJSON {
			fmt.Fprintf(errOut, "note: some output was skipped (it resumes at byte %d)\n", ch.Offset)
		}
	}
	if asJSON {
		return encodeJSON(out, ch)
	}
	_, err := out.Write(san.Filter(ch.Data))
	return err
}
