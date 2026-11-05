package notify

import (
	"bytes"
	"context"
	"os/exec"
	"sync"
	"time"

	"github.com/Autarquico/codefabric/internal/support/procgroup"
)

// maxOutput bounds what is kept of the output of a command.
const maxOutput = 4 << 10

// Exec runs name with args (never through a shell), stdin as input, and
// returns its output cut to maxOutput bytes. The command ends with ctx.
type Exec func(ctx context.Context, name string, args []string, stdin []byte) ([]byte, error)

// drainFor is how long a command's pipes may stay open once it has exited: what
// still holds them then (a backgrounded child) is killed with its group.
const drainFor = time.Second

// OSExec is Exec over os/exec. The command leads a process group of its own,
// and the whole group is killed when ctx ends or when the command leaves its
// pipes held, so a hook cannot outlive its timeout through its descendants.
func OSExec(ctx context.Context, name string, args []string, stdin []byte) ([]byte, error) {
	cmd := exec.Command(name, args...) //nolint:gosec // argv only: name and args are configured or fixed, no shell
	cmd.Stdin = bytes.NewReader(stdin)
	out := &boundedBuffer{max: maxOutput}
	group, err := procgroup.Start(ctx, cmd, procgroup.Options{Stdout: out, Stderr: out, Drain: drainFor})
	if err != nil {
		return nil, err
	}
	err = group.Wait()
	return out.bytes(), err
}

// boundedBuffer keeps the first max bytes and swallows the rest, so a chatty
// command is not held up by a full pipe.
type boundedBuffer struct {
	mu  sync.Mutex // stdout and stderr write to it from two goroutines
	buf bytes.Buffer
	max int
}

func (b *boundedBuffer) bytes() []byte {
	b.mu.Lock()
	defer b.mu.Unlock()
	return bytes.Clone(b.buf.Bytes())
}

func (b *boundedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if room := b.max - b.buf.Len(); room > 0 {
		b.buf.Write(p[:min(room, len(p))])
	}
	return len(p), nil
}
