package execwrap

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"sync"
	"syscall"
	"time"
)

// inputPoll is how often the wrapper looks for InputEOFFile while the FIFO is open.
const inputPoll = 250 * time.Millisecond

// inputPipe is the writing end of the FIFO that is the stdin of a headless
// harness. The wrapper holds it for as long as the Run lives: the daemon opens
// the FIFO to write a message and closes it again, a daemon that dies and
// another that takes its place do the same, and none of it makes the harness
// read EOF. Only close does (InputEOFFile, or the end of the wrapper). All its
// methods are safe on a nil *inputPipe, which is a harness with a plain stdin.
type inputPipe struct {
	mu sync.Mutex
	wr *os.File
}

// openInput makes the FIFO if it is not there and opens both of its ends.
// The order is the point: the reading end first, without blocking (which
// succeeds with no writer), then the writing end (which succeeds because a
// reader exists), and only then the reading end is made blocking, as a harness
// expects its stdin to be. The harness is started with rd; wr is ours.
func openInput(path string) (pipe *inputPipe, rd *os.File, err error) {
	if err := syscall.Mkfifo(path, 0o600); err != nil && !errors.Is(err, fs.ErrExist) {
		return nil, nil, fmt.Errorf("making the input FIFO: %w", err)
	}
	if fi, err := os.Lstat(path); err != nil || fi.Mode()&fs.ModeNamedPipe == 0 {
		return nil, nil, fmt.Errorf("%s is not a FIFO", path)
	}
	rfd, err := syscall.Open(path, syscall.O_RDONLY|syscall.O_NONBLOCK|syscall.O_CLOEXEC, 0)
	if err != nil {
		return nil, nil, fmt.Errorf("opening the input FIFO to read: %w", err)
	}
	wfd, err := syscall.Open(path, syscall.O_WRONLY|syscall.O_CLOEXEC, 0)
	if err != nil {
		_ = syscall.Close(rfd)
		return nil, nil, fmt.Errorf("opening the input FIFO to write: %w", err)
	}
	if err := syscall.SetNonblock(rfd, false); err != nil {
		_ = syscall.Close(rfd)
		_ = syscall.Close(wfd)
		return nil, nil, fmt.Errorf("making the input FIFO block: %w", err)
	}
	return &inputPipe{wr: os.NewFile(uintptr(wfd), path)}, os.NewFile(uintptr(rfd), path), nil
}

// start writes the first lines of the input, without holding up the wrapper if
// the harness is slow to read them.
func (p *inputPipe) start(initial string) {
	if p == nil || initial == "" {
		return
	}
	p.mu.Lock()
	wr := p.wr
	p.mu.Unlock()
	if wr == nil {
		return
	}
	go func() { _, _ = wr.WriteString(initial) }()
}

// close ends the input: the harness reads EOF once it has read what was
// written. It is idempotent.
func (p *inputPipe) close() {
	if p == nil {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.wr != nil {
		_ = p.wr.Close()
		p.wr = nil
	}
}

func fileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}
