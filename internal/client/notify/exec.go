package notify

import (
	"bytes"
	"context"
	"os/exec"
)

// maxOutput bounds what is kept of the output of a command.
const maxOutput = 4 << 10

// Exec runs name with args (never through a shell), stdin as input, and
// returns its output cut to maxOutput bytes. The command ends with ctx.
type Exec func(ctx context.Context, name string, args []string, stdin []byte) ([]byte, error)

// OSExec is Exec over os/exec.
func OSExec(ctx context.Context, name string, args []string, stdin []byte) ([]byte, error) {
	cmd := exec.CommandContext(ctx, name, args...) //nolint:gosec // argv only: name and args are configured or fixed, no shell
	cmd.Stdin = bytes.NewReader(stdin)
	out := &boundedBuffer{max: maxOutput}
	cmd.Stdout, cmd.Stderr = out, out
	err := cmd.Run()
	return out.buf.Bytes(), err
}

// boundedBuffer keeps the first max bytes and swallows the rest, so a chatty
// command is not held up by a full pipe.
type boundedBuffer struct {
	buf bytes.Buffer
	max int
}

func (b *boundedBuffer) Write(p []byte) (int, error) {
	if room := b.max - b.buf.Len(); room > 0 {
		b.buf.Write(p[:min(room, len(p))])
	}
	return len(p), nil
}
