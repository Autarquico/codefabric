package cli

import (
	"bytes"
	"context"
	"strings"
	"testing"

	v1 "github.com/Autarquico/codefabric/internal/api/v1"
	"github.com/Autarquico/codefabric/internal/client"
)

// gappedOutput answers like the Controller: a read that begins inside the gap
// resumes after it, and says so with Truncated; a default read is the tail
// after the last gap.
type gappedOutput struct {
	before, after []byte
	gapTo         uint64 // the gap is [len(before), gapTo)
	requests      int
	tail          uint64 // when set, a default read is this many bytes, not everything after the gap
}

func (g *gappedOutput) end() uint64 { return g.gapTo + uint64(len(g.after)) }

func (g *gappedOutput) RunOutput(_ context.Context, _ string, q client.OutputQuery) (v1.RunOutput, error) {
	g.requests++
	if g.requests > 50 {
		return v1.RunOutput{}, context.DeadlineExceeded // a tail that does not finish
	}
	out := v1.RunOutput{LeaseEpoch: 1, End: g.end()}
	from, limit := g.gapTo, g.end()
	if q.Offset != nil {
		from, limit = *q.Offset, *q.Offset+q.Limit
	}
	out.Truncated = q.Offset == nil // the tail began after the gap
	if q.Offset == nil && g.tail > 0 {
		from, out.Truncated = max(g.gapTo, g.end()-g.tail), false
	}
	if from >= uint64(len(g.before)) && from < g.gapTo {
		from, out.Truncated = g.gapTo, true
	}
	to := min(limit, g.end())
	if from < uint64(len(g.before)) { // like ReadOutput: a page stops where the gap begins
		to = min(to, uint64(len(g.before)))
	}
	out.Offset = from
	switch {
	case from < uint64(len(g.before)):
		out.Data = g.before[from:min(to, uint64(len(g.before)))]
	case from < to:
		out.Data = g.after[from-g.gapTo : to-g.gapTo]
	}
	return out, nil
}

func TestReadTailDoesNotCrossAGapLargerThanAPage(t *testing.T) {
	big := uint64(v1.OutputPageBytes)*2 + 7
	for name, g := range map[string]*gappedOutput{
		"final gap":               {before: []byte("before\n"), gapTo: 7 + big},
		"gap with bytes after it": {before: []byte("before\n"), after: []byte("after\n"), gapTo: 7 + big},
	} {
		t.Run(name, func(t *testing.T) {
			_, tail, _, gap, err := readTail(t.Context(), g, "run", 10)
			if err != nil {
				t.Fatal(err)
			}
			if !gap {
				t.Error("gap = false, want the note about output before a gap")
			}
			if want := string(g.after); string(tail) != want {
				t.Errorf("tail = %q, want %q", tail, want)
			}
			if n := bytes.Count(tail, []byte("after")); n > 1 || strings.Contains(string(tail), "before") {
				t.Errorf("tail = %q repeats or crosses the gap", tail)
			}
		})
	}
}

func TestReadTailDoesNotPrependAPageThatEndsBeforeTheBytesRead(t *testing.T) {
	tail := uint64(v1.OutputDefaultTail)
	g := &gappedOutput{
		before: []byte(strings.Repeat("before\n", 300)),
		after:  []byte(strings.Repeat("a", int(tail)+500) + "\n"),
		gapTo:  uint64(7*300 + 1000), // a gap smaller than a page
		tail:   tail,
	}
	_, got, _, gap, err := readTail(t.Context(), g, "run", 10)
	if err != nil {
		t.Fatal(err)
	}
	if !gap {
		t.Error("gap = false, want the note about output before a gap")
	}
	if strings.Contains(string(got), "before") {
		t.Errorf("tail holds bytes from before the gap")
	}
}
