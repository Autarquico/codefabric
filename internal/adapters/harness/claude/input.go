package claude

import (
	"context"
	"fmt"
	"os"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/Autarquico/codefabric/internal/adapters/harness"
)

// MinStructuredInputVersion is the first Claude Code whose MCP permission
// tool accepts the answers this adapter gives (an allow carries the original
// input; from this version an allow without it is accepted too).
const MinStructuredInputVersion = "2.1.207"

// inputWriteTimeout bounds a write to the FIFO of a Run whose process does not read.
const inputWriteTimeout = 5 * time.Second

var _ harness.StructuredInputChecker = Adapter{}

// CheckStructuredInput is nil when the Claude Code of the host is at least
// MinStructuredInputVersion.
func (a Adapter) CheckStructuredInput(ctx context.Context, host harness.Host) error {
	v, err := a.Version(ctx, host)
	if err != nil {
		return err
	}
	if !versionAtLeast(v, MinStructuredInputVersion) {
		return fmt.Errorf("claude-code: %s is older than %s, the first with the structured input this Worker uses", v, MinStructuredInputVersion)
	}
	return nil
}

// versionAtLeast compares the X.Y.Z of two versions; a suffix after the patch
// number is ignored, and a version that is not X.Y.Z is older than anything.
func versionAtLeast(v, floor string) bool {
	have, ok := triple(v)
	if !ok {
		return false
	}
	want, _ := triple(floor)
	for i := range have {
		if have[i] != want[i] {
			return have[i] > want[i]
		}
	}
	return true
}

func triple(v string) (out [3]int, ok bool) {
	parts := strings.SplitN(v, ".", 3)
	if len(parts) != 3 {
		return out, false
	}
	for i, p := range parts {
		end := 0
		for end < len(p) && p[end] >= '0' && p[end] <= '9' {
			end++
		}
		n, err := strconv.Atoi(p[:end])
		if err != nil {
			return out, false
		}
		out[i] = n
	}
	return out, true
}

// SendInput writes the text as one more user message of the stream-json input
// of the Run, to the FIFO the wrapper made. The wrapper holds the other end
// open, so the message is not lost when nobody reads at that instant, and
// closing this end never ends the input of the process. A Run whose wrapper is
// gone has nobody on the other end: that is an error, not a wait.
func (Adapter) SendInput(_ context.Context, _ harness.Host, h harness.Handle, in harness.Input) error {
	if h.InputPath == "" {
		return fmt.Errorf("claude-code: this Run was not started with structured input: %w", harness.ErrInputNotSent)
	}
	msg, err := userMessage(in.Text)
	if err != nil {
		return fmt.Errorf("%w: %w", harness.ErrInputNotSent, err)
	}
	// ENXIO here is a FIFO nobody reads. The descriptor is opened by hand so that
	// the file is a non-blocking one the runtime polls, which a write deadline needs.
	fd, err := syscall.Open(h.InputPath, syscall.O_WRONLY|syscall.O_NONBLOCK|syscall.O_CLOEXEC, 0)
	if err != nil {
		return fmt.Errorf("claude-code: opening the input of the Run: %w: %w", harness.ErrInputNotSent, err)
	}
	f := os.NewFile(uintptr(fd), h.InputPath)
	defer func() { _ = f.Close() }()
	if err := f.SetWriteDeadline(time.Now().Add(inputWriteTimeout)); err != nil {
		return fmt.Errorf("claude-code: writing the input of the Run: %w: %w", harness.ErrInputNotSent, err)
	}
	if n, err := f.WriteString(msg); n == 0 && err != nil {
		return fmt.Errorf("claude-code: writing the input of the Run: %w: %w", harness.ErrInputNotSent, err)
	} else if err != nil {
		return fmt.Errorf("claude-code: writing the input of the Run: %d of %d bytes written: %w", n, len(msg), err)
	}
	return nil
}
