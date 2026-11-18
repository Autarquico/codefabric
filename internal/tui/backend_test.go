package tui

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"

	v1 "github.com/Autarquico/codefabric/internal/api/v1"
	"github.com/Autarquico/codefabric/internal/cli"
	"github.com/Autarquico/codefabric/internal/cli/actions"
	"github.com/Autarquico/codefabric/internal/domain"
)

// history serves a Task's timeline of total events in pages, like the Controller.
type history struct {
	total int
	pages []int // the limit of each page asked for
	err   error
}

func (h *history) TaskTimeline(_ context.Context, _, _, after string, limit int) (v1.TaskTimeline, error) {
	if h.err != nil {
		return v1.TaskTimeline{}, h.err
	}
	h.pages = append(h.pages, limit)
	from, _ := strconv.Atoi(after) // the cursor is the number of events already read
	to := min(from+limit, h.total)
	var page v1.TaskTimeline
	for seq := uint64(from) + 1; seq <= uint64(to); seq++ { //nolint:gosec // both are small and not negative
		page.Events = append(page.Events, v1.Event{Sequence: seq})
	}
	page.HasMore, page.Cursor = to < h.total, strconv.Itoa(to)
	return page, nil
}

func sequences(events []v1.Event) (first, last uint64) {
	return events[0].Sequence, events[len(events)-1].Sequence
}

func TestSDKBackend_Timeline_KeepsTheNewestEventsInOrder(t *testing.T) {
	h := &history{total: 250} // 3 pages
	got, err := sdkBackend{ctx: context.Background(), client: h}.Timeline("tsk_1")
	if err != nil {
		t.Fatal(err)
	}
	first, last := sequences(got.Events)
	if len(got.Events) != timelineKeep || first != 250-timelineKeep+1 || last != 250 || got.Capped {
		t.Errorf("kept %d events %d..%d (capped %v), want the newest %d, 201..250", len(got.Events), first, last, got.Capped, timelineKeep)
	}
	if len(h.pages) != 3 {
		t.Errorf("read %d pages for 250 events", len(h.pages))
	}
	for i := 1; i < len(got.Events); i++ {
		if got.Events[i].Sequence != got.Events[i-1].Sequence+1 {
			t.Fatalf("events out of order at %d", i)
		}
	}
}

func TestSDKBackend_Timeline_ShortHistoryIsKeptWhole(t *testing.T) {
	got, err := sdkBackend{ctx: context.Background(), client: &history{total: 3}}.Timeline("tsk_1")
	if first, last := sequences(got.Events); err != nil || len(got.Events) != 3 || first != 1 || last != 3 || got.Capped {
		t.Errorf("got %d events, capped %v, err %v", len(got.Events), got.Capped, err)
	}
}

func TestSDKBackend_Timeline_SaysWhenTheHistoryWasTooLongToReadToItsEnd(t *testing.T) {
	h := &history{total: timelinePage*timelineMaxPages + 500}
	got, err := sdkBackend{ctx: context.Background(), client: h}.Timeline("tsk_1")
	if err != nil {
		t.Fatal(err)
	}
	if len(h.pages) != timelineMaxPages || !got.Capped {
		t.Errorf("read %d pages, capped %v: the cap of %d pages was not reported", len(h.pages), got.Capped, timelineMaxPages)
	}
	if _, last := sequences(got.Events); last != timelinePage*timelineMaxPages {
		t.Errorf("the window ends at %d, the last event read", last)
	}
}

func TestSDKBackend_Timeline_AFailedPageIsAnError(t *testing.T) {
	boom := errors.New("controller down")
	if _, err := (sdkBackend{ctx: context.Background(), client: &history{err: boom}}).Timeline("tsk_1"); !errors.Is(err, boom) {
		t.Errorf("err = %v", err)
	}
}

func TestSDKBackend_Attach_ARefusalIsAFailureAndNoSession(t *testing.T) {
	session, failure := sdkBackend{ctx: context.Background(), runner: actions.Runner{}}.Attach("task.cancel", actions.Input{Target: "tsk_1"})
	if session != nil || failure == nil || failure.Code != domain.CodeInvalidRequest {
		t.Errorf("session %v, failure %+v", session, failure)
	}
}

func TestSDKAttachment_ExposesTheSessionAndMapsItsExit(t *testing.T) {
	session := &cli.AttachSession{}
	a := sdkAttachment{ctx: context.Background(), session: session}
	if cmd, ok := a.Command().(*plainKeyboard); !ok || cmd.ExecCommand != tea.ExecCommand(session) {
		t.Error("Command is not the session, started with the plain keyboard")
	}
	if err := a.Finish(nil); err != nil {
		t.Errorf("exit 0 answered %v", err)
	}
	failure := actions.FailureOf(a.Finish(cli.ExitStatus(255)))
	if failure.Code != domain.CodeRunNotAttachable {
		t.Errorf("exit 255 answered %v", fmt.Sprint(failure.Code))
	}
}

type recordedCommand struct {
	out io.Writer
	log *strings.Builder
}

func (c *recordedCommand) Run() error          { c.log.WriteString("|run"); return nil }
func (c *recordedCommand) SetStdin(io.Reader)  {}
func (c *recordedCommand) SetStderr(io.Writer) {}
func (c *recordedCommand) SetStdout(w io.Writer) {
	c.out = w
}

// An attach starts with the keyboard in its plain mode, before anything runs.
func TestPlainKeyboard_ResetsTheKeyboardModesBeforeTheAttachRuns(t *testing.T) {
	var out strings.Builder
	inner := &recordedCommand{log: &out}
	cmd := &plainKeyboard{ExecCommand: inner}
	cmd.SetStdout(&out)
	if err := cmd.Run(); err != nil {
		t.Fatal(err)
	}
	if want := "\x1b[<u\x1b[>4;0m|run"; out.String() != want {
		t.Errorf("wrote %q, want %q", out.String(), want)
	}
}
