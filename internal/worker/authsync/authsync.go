package authsync

import (
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

	mu   sync.Mutex
	runs map[string]tracked
}

type tracked struct {
	auth   harness.AuthSyncer
	global string
	copies []string
}

// New is a Broker with o.
func New(o Options) *Broker {
	b := &Broker{clock: o.Clock, log: o.Log, interval: o.Interval, runs: map[string]tracked{}}
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
	b.runs[id] = tracked{auth: auth, global: auth.GlobalAuth(host), copies: copies}
}

// Untrack forgets the Run id: the Broker never touches its copies again. Once
// it returns, no poll is writing them.
func (b *Broker) Untrack(id string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	delete(b.runs, id)
}

// Run polls every interval until ctx ends.
func (b *Broker) Run(ctx context.Context) error {
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
			b.replace(auth, v.path, versions[best])
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
	fd, err := unix.Open(path, unix.O_RDONLY|unix.O_NOFOLLOW|unix.O_NONBLOCK|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, false
	}
	f := os.NewFile(uintptr(fd), path)
	defer func() { _ = f.Close() }()
	info, err := f.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Size() > maxAuthSize {
		return nil, false
	}
	content, err := io.ReadAll(io.LimitReader(f, maxAuthSize+1))
	if err != nil || len(content) > maxAuthSize {
		return nil, false
	}
	return content, true
}

// replace writes from over the file at path, if the file is still valid, of
// the same account and older than from: the harness may have refreshed it
// since it was read.
func (b *Broker) replace(auth harness.AuthSyncer, path string, from version) {
	cur, ok := b.read(auth, path, false)
	if !ok || cur.identity != from.identity || !cur.stamp.Before(from.stamp) {
		return
	}
	if err := atomicfile.Write(path, from.content, 0o600); err != nil {
		b.log.Warn("propagating the auth did not finish; it is tried again", "file", path, "err", err)
	}
}
