package authsync

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"time"

	"golang.org/x/sys/unix"

	"github.com/Autarquico/codefabric/internal/adapters/harness"
	"github.com/Autarquico/codefabric/internal/support/atomicfile"
	"github.com/Autarquico/codefabric/internal/support/clock"
)

// DefaultInterval is how often the Broker looks, when Options.Interval is zero.
const DefaultInterval = 5 * time.Second

// MaxClockSkew is how far ahead of the clock of the Worker a stamp may be and
// still be believed: the clock of the machine that wrote it may differ a
// little. A later stamp is not a refresh but a way to stay the most recent
// for ever, so such a version is not valid: it is neither propagated nor
// replaced.
const MaxClockSkew = 5 * time.Minute

// maxAuthSize is the most the Broker reads of an auth file; Codex writes a few
// KiB, and a Run chooses what its copy holds.
const maxAuthSize = 1 << 20

// Options are the options of a [Broker].
type Options struct {
	Clock    clock.Clock // defaults to clock.Real
	Log      *slog.Logger
	Interval time.Duration // defaults to DefaultInterval
}

// Broker propagates the most recent auth among the host and the Runs that
// are registered. Without a Run registered it does nothing.
type Broker struct {
	clock    clock.Clock
	log      *slog.Logger
	interval time.Duration

	mu      sync.Mutex
	runs    map[string]tracked
	watches map[string]*watch // by path of the file (host, resolved, or copy) whose replaced inode is watched

	// hook, when set by a test, is called at the named stage of replace of path.
	hook func(stage, path string)
}

type tracked struct {
	auth   harness.AuthSyncer
	global string
	copies []string
}

// New is a Broker with o.
func New(o Options) *Broker {
	b := &Broker{clock: o.Clock, log: o.Log, interval: o.Interval, runs: map[string]tracked{}, watches: map[string]*watch{}}
	if b.clock == nil {
		b.clock = clock.Real{}
	}
	if b.log == nil {
		b.log = slog.Default()
	}
	if b.interval <= 0 {
		b.interval = DefaultInterval
	}
	return b
}

// Track registers the auth copies of the Run id, as paths, for the adapter
// auth. It replaces what was registered for id.
func (b *Broker) Track(id string, auth harness.AuthSyncer, host harness.Host, copies []string) {
	if len(copies) == 0 {
		b.Untrack(id)
		return
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	b.forget(id)
	b.runs[id] = tracked{auth: auth, global: auth.GlobalAuth(host), copies: copies}
}

// Untrack forgets the Run id: the Broker never touches its copies again. Once
// it returns, no poll is writing them.
func (b *Broker) Untrack(id string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.forget(id)
}

// forget drops the Run id and closes the descriptors it holds. Needs b.mu.
func (b *Broker) forget(id string) {
	for _, c := range b.runs[id].copies {
		b.unwatch(c)
	}
	delete(b.runs, id)
	if len(b.runs) == 0 {
		b.unwatchAll()
	}
}

// unwatchAll closes every descriptor held, whichever file it was of. Needs b.mu.
func (b *Broker) unwatchAll() {
	for p := range b.watches {
		b.unwatch(p)
	}
}

func (b *Broker) unwatch(path string) {
	if w := b.watches[path]; w != nil {
		_ = w.file.Close()
		delete(b.watches, path)
	}
}

// Run polls every interval until ctx ends.
func (b *Broker) Run(ctx context.Context) error {
	defer func() {
		b.mu.Lock()
		defer b.mu.Unlock()
		b.unwatchAll()
	}()
	t := b.clock.NewTicker(b.interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-t.C():
			b.Poll()
		}
	}
}

// Poll looks once at the auth of the host and of the Runs, and propagates the
// most recent valid version.
func (b *Broker) Poll() {
	b.mu.Lock()
	defer b.mu.Unlock()
	type group struct {
		auth  harness.AuthSyncer
		files []string
	}
	groups := map[string]*group{}
	for _, r := range b.runs {
		g := groups[r.global]
		if g == nil {
			g = &group{auth: r.auth, files: []string{r.global}}
			groups[r.global] = g
		}
		g.files = append(g.files, r.copies...)
	}
	for _, g := range groups {
		b.sync(g.auth, g.files)
	}
}

// version is one readable, valid auth file.
type version struct {
	path     string
	content  []byte
	stamp    time.Time
	identity string
}

// sync propagates the most recent of the versions of files, the first of
// which is the auth of the host, to the others that are older. The auth of the
// host says whose account it is: a version of another account (or of none) is
// neither the most recent nor replaced, and with no valid auth of an account
// in the host nothing is propagated, since nothing says which account the
// copies are to be.
func (b *Broker) sync(auth harness.AuthSyncer, files []string) {
	hostVersion, ok := b.read(auth, files[0], true)
	if !ok || hostVersion.identity == "" {
		return
	}
	versions := []version{hostVersion}
	for _, f := range files[1:] {
		if v, ok := b.read(auth, f, false); ok && v.identity == hostVersion.identity {
			versions = append(versions, v)
		}
	}
	// What Codex wrote into an inode that was replaced under it, in the host or
	// in a copy, is one more candidate, a source only: its path now holds what
	// the Broker put there.
	for _, f := range append([]string{hostVersion.path}, files[1:]...) {
		if v, ok := b.orphan(auth, f); ok && v.identity == hostVersion.identity {
			versions = append(versions, v)
		}
	}
	best := -1
	for i, v := range versions {
		if !v.stamp.IsZero() && (best < 0 || v.stamp.After(versions[best].stamp)) {
			best = i
		}
	}
	if best < 0 {
		return
	}
	for i, v := range versions {
		if i != best && v.stamp.Before(versions[best].stamp) {
			if v.path != "" {
				b.replace(auth, v.path, versions[best])
			}
		}
	}
}

// read is the version at path, when it is a regular file the adapter accepts
// and whose stamp is not in the future. The auth of the host may be a link (a
// dotfiles manager keeps it so) and is replaced where it points; a copy that
// is not a regular file is none of ours, and is opened without following a
// link, so that a Run cannot have the Broker read what it chooses.
func (b *Broker) read(auth harness.AuthSyncer, path string, host bool) (version, bool) {
	if host {
		resolved, err := filepath.EvalSymlinks(path)
		if err != nil {
			return version{}, false
		}
		path = resolved
	}
	content, ok := readRegular(path)
	if !ok {
		return version{}, false
	}
	return b.validate(auth, path, content)
}

// validate is the version of content, when the adapter accepts it and its
// stamp is not in the future.
func (b *Broker) validate(auth harness.AuthSyncer, path string, content []byte) (version, bool) {
	stamp, identity, err := auth.AuthStamp(content)
	if err != nil {
		b.log.Debug("an auth file is not valid; it is not propagated nor replaced", "file", path)
		return version{}, false
	}
	if stamp.After(b.clock.Now().Add(MaxClockSkew)) {
		b.log.Warn("an auth file is stamped in the future; it is not propagated nor replaced", "file", path)
		return version{}, false
	}
	return version{path: path, content: content, stamp: stamp, identity: identity}, true
}

// readRegular is the content of the file at path, if the last element is not
// a link and what it names is a regular file of a reasonable size: the type is
// looked at in the descriptor that is read, not in the name.
func readRegular(path string) ([]byte, bool) {
	f, _, content, ok := openRegular(path)
	if ok {
		_ = f.Close()
	}
	return content, ok
}

// openRegular is readRegular that keeps the descriptor open (and its info at
// the time of reading) when it succeeds.
func openRegular(path string) (*os.File, os.FileInfo, []byte, bool) {
	fd, err := unix.Open(path, unix.O_RDONLY|unix.O_NOFOLLOW|unix.O_NONBLOCK|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, nil, nil, false
	}
	f := os.NewFile(uintptr(fd), path)
	info, err := f.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Size() > maxAuthSize {
		_ = f.Close()
		return nil, nil, nil, false
	}
	content, err := io.ReadAll(io.LimitReader(f, maxAuthSize+1))
	if err != nil || len(content) > maxAuthSize {
		_ = f.Close()
		return nil, nil, nil, false
	}
	return f, info, content, true
}

// watchPolls is how many polls the Broker keeps looking at an inode it has
// replaced: Codex truncates and then writes, without a lock, so a write that
// began before the rename may land after it, in the inode no path names.
const watchPolls = 3

// watch is a replaced inode, still open, and the content it had when it was
// read. It holds one descriptor, until the polls run out or the Run is forgotten.
type watch struct {
	file  *os.File
	last  []byte
	polls int
}

// orphan is the version Codex left in the replaced inode of path, if it
// changed since the last look. Needs b.mu.
func (b *Broker) orphan(auth harness.AuthSyncer, path string) (version, bool) {
	w := b.watches[path]
	if w == nil {
		return version{}, false
	}
	if w.polls--; w.polls < 0 {
		b.unwatch(path)
		return version{}, false
	}
	content, err := io.ReadAll(io.NewSectionReader(w.file, 0, maxAuthSize+1))
	if err != nil || len(content) > maxAuthSize || bytes.Equal(content, w.last) {
		return version{}, false
	}
	w.last = content
	v, ok := b.validate(auth, path, content)
	v.path = ""
	return v, ok
}

// replace writes from over the file at path, if the file is still valid, of
// the same account and older than from: the harness may have refreshed it
// since it was read. A file whose identity changed between reading it and
// publishing is left to the next poll; one written after that check is caught
// by watching the inode that is replaced.
func (b *Broker) replace(auth harness.AuthSyncer, path string, from version) {
	f, info, content, ok := openRegular(path)
	if !ok {
		return
	}
	keep := false
	defer func() {
		if !keep {
			_ = f.Close()
		}
	}()
	cur, ok := b.validate(auth, path, content)
	if !ok || cur.identity != from.identity || !cur.stamp.Before(from.stamp) {
		return
	}
	b.call("read", path)
	if !unchanged(path, f, info) {
		return
	}
	b.call("publish", path)
	if err := atomicfile.Write(path, from.content, 0o600); err != nil {
		b.log.Warn("propagating the auth did not finish; it is tried again", "file", path, "err", err)
		return
	}
	b.unwatch(path)
	b.watches[path] = &watch{file: f, last: content, polls: watchPolls}
	keep = true
}

func (b *Broker) call(stage, path string) {
	if b.hook != nil {
		b.hook(stage, path)
	}
}

// unchanged reports whether path still names the inode of f, with the size and
// modification time it had when it was read.
func unchanged(path string, f *os.File, read os.FileInfo) bool {
	onDisk, err := os.Lstat(path)
	if err != nil || !os.SameFile(onDisk, read) {
		return false
	}
	now, err := f.Stat()
	return err == nil && now.Size() == read.Size() && now.ModTime().Equal(read.ModTime()) &&
		onDisk.Size() == read.Size() && onDisk.ModTime().Equal(read.ModTime())
}
