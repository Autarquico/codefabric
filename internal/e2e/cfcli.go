//go:build e2e && (linux || darwin)

package e2e

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"regexp"
	"strings"
	"time"

	v1 "github.com/Autarquico/codefabric/internal/api/v1"
	"github.com/Autarquico/codefabric/internal/domain"
)

// cfLimit bounds one cf command that is not a follow.
const cfLimit = 60 * time.Second

// Result is how a cf command ended: its exit code and what it wrote, apart.
type Result struct {
	Args           []string
	Code           int
	Stdout, Stderr string
}

func (r Result) String() string {
	return fmt.Sprintf("cf %s: exit %d\nstdout: %s\nstderr: %s", strings.Join(r.Args, " "), r.Code, r.Stdout, r.Stderr)
}

// CF runs the real cf in box b, the owner's commands in the owner's clone, and
// returns how it ended. A command that does not end in time fails the test.
func (c *Cluster) CF(b *Box, args ...string) Result {
	c.t.Helper()
	p := c.cfProc(b, args...)
	ctx, cancel := context.WithTimeout(c.H.Context(), Scaled(cfLimit))
	defer cancel()
	err := p.Wait(ctx)
	if ctx.Err() != nil {
		killCtx, cancelKill := context.WithTimeout(context.Background(), teardownTimeout)
		_ = p.KillGroup(killCtx)
		cancelKill()
		c.t.Fatalf("e2e: cf %s did not end: %v\n%s", strings.Join(args, " "), err, c.Logs())
	}
	res := Result{Args: args, Stdout: p.Log(), Stderr: p.Stderr()}
	var exit *exec.ExitError
	switch {
	case errors.As(err, &exit):
		res.Code = exit.ExitCode()
	case err != nil:
		c.t.Fatalf("e2e: cf %s: %v", strings.Join(args, " "), err)
	}
	return res
}

// mustCF is CF that requires success.
func (c *Cluster) mustCF(b *Box, args ...string) Result {
	c.t.Helper()
	res := c.CF(b, args...)
	if res.Code != 0 {
		c.t.Fatalf("e2e: %s", res)
	}
	return res
}

var unsafeName = regexp.MustCompile(`[^a-z0-9-]+`)

// cfProc starts cf in b as a client process with a log of its own.
func (c *Cluster) cfProc(b *Box, args ...string) *Proc {
	c.t.Helper()
	verb := "cf"
	if len(args) > 0 {
		verb = unsafeName.ReplaceAllString(args[0], "")
	}
	dir := ""
	if b == c.Ctl && c.Repo != "" {
		dir = c.Repo
	}
	return b.add(&Proc{
		box: b, role: RoleClient, name: fmt.Sprintf("cf%03d-%s", c.cfN.Add(1), verb),
		argv: append([]string{binary}, args...), dir: dir, splitStderr: true,
	})
}

// JSON runs cf with --json in b, requires success and decodes the one value it writes.
func JSON[T any](c *Cluster, b *Box, args ...string) T {
	c.t.Helper()
	res := c.CF(b, append(args, "--json")...)
	if res.Code != 0 {
		c.t.Fatalf("e2e: %s", res)
	}
	var v T
	if err := decodeOne(res.Stdout, &v); err != nil {
		c.t.Fatalf("e2e: %v\n%s", err, res)
	}
	return v
}

// Fails runs cf with --json in b and requires the error code want, with its
// exit code and its envelope on stdout.
func Fails(c *Cluster, b *Box, want domain.Code, args ...string) v1.ErrorBody {
	c.t.Helper()
	res := c.CF(b, append(args, "--json")...)
	var env v1.ErrorEnvelope
	if err := decodeOne(res.Stdout, &env); err != nil {
		c.t.Fatalf("e2e: not an error envelope: %v\n%s", err, res)
	}
	if res.Code != domain.ExitCode(want) || env.Error.Code != want {
		c.t.Fatalf("e2e: want exit %d and %s, got %+v\n%s", domain.ExitCode(want), want, env.Error, res)
	}
	return env.Error
}

// decodeOne decodes out, which must hold exactly one JSON value, into v.
func decodeOne(out string, v any) error {
	dec := json.NewDecoder(strings.NewReader(out))
	if err := dec.Decode(v); err != nil {
		return fmt.Errorf("decoding the output: %w", err)
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return errors.New("the output holds more than one JSON value")
	}
	return nil
}

// Do creates a Task as the owner, with the harness command running prompt as
// a script; flags go to `cf do` as they are.
func (c *Cluster) Do(prompt string, flags ...string) v1.CreateTaskResponse {
	c.t.Helper()
	created := JSON[v1.CreateTaskResponse](c, c.Ctl, append([]string{"do", prompt, "--harness", "command"}, flags...)...)
	if created.Run == nil {
		c.t.Fatalf("e2e: cf do created no Run: %+v", created.Task)
	}
	return created
}

// TaskSet changes a Task as the owner (`cf task set`).
func (c *Cluster) TaskSet(task string, flags ...string) v1.Task {
	c.t.Helper()
	return JSON[v1.Task](c, c.Ctl, append([]string{"task", "set", task}, flags...)...)
}

// Run is `cf run show <ref> --json` in b.
func (c *Cluster) Run(b *Box, ref string) v1.Run {
	c.t.Helper()
	return JSON[v1.Run](c, b, "run", "show", ref)
}

// RunShow is what `cf run show <ref>` shows a person in b.
func (c *Cluster) RunShow(b *Box, ref string) string {
	c.t.Helper()
	return c.mustCF(b, "run", "show", ref).Stdout
}

// Ps is `cf ps` in b.
func (c *Cluster) Ps(b *Box) []v1.PsEntry {
	c.t.Helper()
	return JSON[v1.PsResponse](c, b, "ps").Runs
}

// ListWorkers is `cf worker list` in b.
func (c *Cluster) ListWorkers(b *Box) []v1.Worker {
	c.t.Helper()
	return JSON[v1.ListWorkersResponse](c, b, "worker", "list").Workers
}

// Events are all the events `cf events` gives in b, page after page from the
// first; filters (--run, --type, ...) go to every page.
func (c *Cluster) Events(b *Box, filters ...string) []v1.Event {
	c.t.Helper()
	var all []v1.Event
	cursor := "0"
	for {
		args := append([]string{"events", "--after", cursor, "--limit", fmt.Sprint(v1.EventsMaxLimit)}, filters...)
		page := JSON[v1.EventPage](c, b, args...)
		all = append(all, page.Events...)
		if !page.HasMore {
			return all
		}
		if page.Cursor == cursor {
			c.t.Fatalf("e2e: events: a page with more after it left the cursor at %s", cursor)
		}
		cursor = page.Cursor
	}
}

// FollowLogs is `cf logs --run <run> --follow` in b, as a process that goes on
// until it is killed or the output ends; what it shows is its Log.
func (c *Cluster) FollowLogs(b *Box, run string) *Proc {
	c.t.Helper()
	return c.cfProc(b, "logs", "--run", run, "--follow")
}

// FollowEvents is `cf events --follow --json` in b, as a process that goes on
// until it is killed; its Log is JSON Lines, read with FollowedEvents.
func (c *Cluster) FollowEvents(b *Box, filters ...string) *Proc {
	c.t.Helper()
	return c.cfProc(b, append([]string{"events", "--follow", "--json"}, filters...)...)
}

// FollowedEvents are the events a FollowEvents process has written so far; a
// line it is still writing is left out.
func FollowedEvents(p *Proc) []v1.Event {
	var out []v1.Event
	for _, line := range bytes.Split([]byte(p.Log()), []byte("\n")) {
		var e v1.Event
		if json.Unmarshal(line, &e) == nil && e.Sequence > 0 {
			out = append(out, e)
		}
	}
	return out
}
