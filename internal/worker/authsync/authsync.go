package authsync

import (
	"bytes"
	"context"
	"errors"
	"io"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
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
	watches map[string][]*watch // by path of the file (host, resolved, or copy) whose replaced inodes are watched, oldest first

	// hook, when set by a test, is called at the named stage of replace of path.
	hook func(stage, path string)
}

type tracked struct {
	auth   harness.AuthSyncer
	global string
	copies []string
	// ended is a Run whose Release did not finish: its copies are candidates
	// and nothing else, never written and holding no new descriptor.
	ended bool
}

var (
	// errHostUnavailable is a host that is not readable or valid now: Codex
	// truncates the file before it writes it, so it may be so for a moment.
	errHostUnavailable = errors.New("authsync: the auth of the host is not valid or readable now")
	// errHostChanged is a host that changed between being read and being replaced.
	errHostChanged = errors.New("authsync: the auth of the host changed while it was replaced")
)

// New is a Broker with o.
func New(o Options) *Broker {
	b := &Broker{clock: o.Clock, log: o.Log, interval: o.Interval, runs: map[string]tracked{}, watches: map[string][]*watch{}}
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
		if err := b.Release(id); err != nil {
			b.log.Warn("the auth of a Run was not reconciled before it was dropped", "run", id, "err", err)
		}
		return
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	b.forget(id)
	b.runs[id] = tracked{auth: auth, global: auth.GlobalAuth(host), copies: copies}
}

// Release ends the Run id. Before it forgets the Run it reconciles one last
// time, synchronously: the copy of the Run and every inode the Broker kept of
// it are candidates, under the validations of a poll, and the most recent
// version reaches the host and the other copies. Only then are the Run and its
// descriptors dropped, so that what Codex wrote after the last poll is not lost
// with the copy.
//
// If the host could not take the refresh (it could not be written, it is not
// valid or readable at this moment, or it changed while it was replaced),
// Release returns an error and the Run stays registered, as a source only: the
// caller keeps the copies and calls Release again, or gives the Run up with
// [Broker.Abandon]. A host that does not exist, or that holds no account, has
// nowhere to receive anything, and is not an error. Once Release returns nil,
// the Broker never touches the copies of the Run again and no poll is writing
// them. An id that is not registered is nil.
func (b *Broker) Release(id string) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	r, ok := b.runs[id]
	if !ok {
		return nil
	}
	r.ended = true
	b.runs[id] = r
	live, ended := b.group(r.global)
	if err := b.sync(r.auth, r.global, live, ended); err != nil {
		return err
	}
	b.forget(id)
	return nil
}

// Offer reconciles once the copies of a Run the Broker does not know (the
// Worker restarted since), as sources only, under the validations of a poll.
// It registers nothing; the error is the one of Release.
func (b *Broker) Offer(auth harness.AuthSyncer, host harness.Host, copies []string) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	global := auth.GlobalAuth(host)
	live, ended := b.group(global)
	return b.sync(auth, global, live, append(ended, copies...))
}

// Abandon drops the Run id and closes its descriptors without reconciling it:
// what it refreshed since its last publication is lost.
func (b *Broker) Abandon(id string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.forget(id)
}

// group is the copies of the Runs whose auth is global, live and ended. Needs b.mu.
func (b *Broker) group(global string) (live, ended []string) {
	for _, o := range b.runs {
		switch {
		case o.global != global:
		case o.ended:
			ended = append(ended, o.copies...)
		default:
			live = append(live, o.copies...)
		}
	}
	return live, ended
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
	for _, w := range b.watches[path] {
		_ = w.file.Close()
	}
	delete(b.watches, path)
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
	done := map[string]bool{}
	for _, r := range b.runs {
		if done[r.global] {
			continue
		}
		done[r.global] = true
		live, ended := b.group(r.global)
		_ = b.sync(r.auth, r.global, live, ended) // logged where it fails; the next poll tries again
	}
}

// version is one readable, valid auth file.
type version struct {
	path     string
	content  []byte
	stamp    time.Time
	identity string
	// seen is the replaced inode the version was read from, if it was.
	seen *watch
}

// sync propagates the most recent of the versions of host, live and ended to
// the host and to the live copies that are older. The auth of the host says
// whose account it is: a version of another account (or of none) is neither the
// most recent nor replaced, and with no valid auth of an account in the host
// nothing is propagated, since nothing says which account the copies are to be.
// The copies of ended Runs are candidates only.
//
// It returns the error of the host not taking the version, which is what must
// not be lost; a copy that could not be written is tried again by the next poll
// and is the business of its own Run. A version read from a replaced inode is
// seen only once it has been published, or lost to a better one.
func (b *Broker) sync(auth harness.AuthSyncer, host string, live, ended []string) error {
	hostVersion, ok := b.read(auth, host, true)
	if !ok {
		if _, err := os.Stat(host); errors.Is(err, fs.ErrNotExist) {
			return nil
		}
		return errHostUnavailable
	}
	if hostVersion.identity == "" {
		return nil
	}
	versions := []version{hostVersion}
	for _, f := range append(slices.Clone(live), ended...) {
		if v, ok := b.read(auth, f, false); ok && v.identity == hostVersion.identity {
			versions = append(versions, v)
		}
	}
	// What Codex wrote into an inode that was replaced under it, in the host or
	// in a copy, is one more candidate, a source only: its path now holds what
	// the Broker put there.
	for _, f := range slices.Concat([]string{hostVersion.path}, live, ended) {
		for _, v := range b.orphans(auth, f) {
			if v.identity == hostVersion.identity {
				versions = append(versions, v)
			} else {
				v.seen.last = v.content
			}
		}
	}
	best := -1
	for i, v := range versions {
		if !v.stamp.IsZero() && (best < 0 || v.stamp.After(versions[best].stamp)) {
			best = i
		}
	}
	var hostErr, failed error
	if best >= 0 {
		for i, v := range versions {
			if i == best || !v.stamp.Before(versions[best].stamp) || v.path == "" || (i > 0 && isEnded(v.path, ended)) {
				continue
			}
			if err := b.replace(auth, v.path, versions[best]); err != nil {
				failed = err
				if i == 0 {
					hostErr = err
				}
			}
		}
	}
	for i, v := range versions {
		if v.seen != nil && (i != best || failed == nil) {
			v.seen.last = v.content
		}
	}
	return hostErr
}

func isEnded(path string, ended []string) bool { return slices.Contains(ended, path) }

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

// maxRetained is the most inodes the Broker holds open for one file. Codex
// truncates and then writes, without a lock, so a write that began before a
// rename may land after it, in an inode no path names, and nothing bounds the
// time between its open and its write. The inodes replaced are therefore kept
// for the whole life of the Run (of the last Run, for the host) and read at
// every poll and at the final reconciliation. The bound is a descriptor limit:
// past it the oldest is read one last time, as a candidate, before it is
// closed. A refresh can be lost only if the same copy was replaced more than
// maxRetained times while a write begun before the first of them is still
// pending.
const maxRetained = 8

// watch is a replaced inode, still open, and the content it had when it was
// read. It holds one descriptor, until its Run is released.
type watch struct {
	file *os.File
	last []byte
}

// orphans are the versions Codex left in the replaced inodes of path that
// changed since the last look, each a source only. Needs b.mu.
func (b *Broker) orphans(auth harness.AuthSyncer, path string) []version {
	var out []version
	for _, w := range b.watches[path] {
		if v, ok := b.changed(auth, path, w); ok {
			out = append(out, v)
		}
	}
	return out
}

// changed is the valid version in w, if it changed since the last look. It
// is not marked as seen: the caller does it once the version has been
// published, so that a failed publication leaves it a candidate. What is not
// valid is seen at once; there is nothing to publish in it.
func (b *Broker) changed(auth harness.AuthSyncer, path string, w *watch) (version, bool) {
	content, err := io.ReadAll(io.NewSectionReader(w.file, 0, maxAuthSize+1))
	if err != nil || len(content) > maxAuthSize || bytes.Equal(content, w.last) {
		return version{}, false
	}
	v, ok := b.validate(auth, path, content)
	if !ok {
		w.last = content
		return version{}, false
	}
	v.path, v.seen = "", w
	return v, true
}

// upgrade is the version to publish at path: from, unless the oldest inode of
// path, which is about to be closed to stay within maxRetained, holds a valid
// version of the same account that is more recent. It does not mark what it
// reads as seen, so that a failed publication loses nothing. Needs b.mu.
func (b *Broker) upgrade(auth harness.AuthSyncer, path string, from version) version {
	ws := b.watches[path]
	if len(ws) < maxRetained {
		return from
	}
	content, err := io.ReadAll(io.NewSectionReader(ws[0].file, 0, maxAuthSize+1))
	if err != nil || len(content) > maxAuthSize {
		return from
	}
	if v, ok := b.validate(auth, path, content); ok && v.identity == from.identity && v.stamp.After(from.stamp) {
		v.path = ""
		return v
	}
	return from
}

// keepInode holds w, evicting the oldest inode of path if maxRetained is
// reached. Needs b.mu.
func (b *Broker) keepInode(path string, w *watch) {
	ws := b.watches[path]
	if len(ws) >= maxRetained {
		_ = ws[0].file.Close()
		ws = ws[1:]
	}
	b.watches[path] = append(ws, w)
}

// replace writes from over the file at path, if the file is still valid, of
// the same account and older than from: the harness may have refreshed it
// since it was read. A file that changed between reading it and publishing is
// not written and is an error, for the next attempt; one written after that
// check is caught by watching the inode that is replaced.
func (b *Broker) replace(auth harness.AuthSyncer, path string, from version) error {
	f, info, content, ok := openRegular(path)
	if !ok {
		return errHostUnavailable
	}
	keep := false
	defer func() {
		if !keep {
			_ = f.Close()
		}
	}()
	cur, ok := b.validate(auth, path, content)
	if !ok {
		return errHostUnavailable
	}
	if cur.identity != from.identity || !cur.stamp.Before(from.stamp) {
		return nil
	}
	b.call("read", path)
	if !unchanged(path, f, info) {
		return errHostChanged
	}
	b.call("publish", path)
	// The oldest inode, if it is to be closed after this write, is read now: if
	// it holds a more recent version, that one is what is published.
	from = b.upgrade(auth, path, from)
	if err := atomicfile.Write(path, from.content, 0o600); err != nil {
		b.log.Warn("propagating the auth did not finish; it is tried again", "file", path, "err", err)
		return err
	}
	b.keepInode(path, &watch{file: f, last: content})
	keep = true
	return nil
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
