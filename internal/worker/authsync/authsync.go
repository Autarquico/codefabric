package authsync

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/Autarquico/codefabric/internal/adapters/harness"
	"github.com/Autarquico/codefabric/internal/support/atomicfile"
	"github.com/Autarquico/codefabric/internal/support/clock"
)

// DefaultInterval is how often the Broker looks, when Options.Interval is zero.
const DefaultInterval = 5 * time.Second

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
	path    string
	content []byte
	stamp   time.Time
}

// sync propagates the most recent of the versions of files, the first of
// which is the auth of the host, to the others that are older.
func (b *Broker) sync(auth harness.AuthSyncer, files []string) {
	var versions []version
	for i, f := range files {
		if v, ok := b.read(auth, f, i == 0); ok {
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

// read is the version at path, when it is a regular file the adapter accepts.
// The auth of the host may be a link (a dotfiles manager keeps it so) and is
// replaced where it points; a copy that is not a regular file is none of ours.
func (b *Broker) read(auth harness.AuthSyncer, path string, host bool) (version, bool) {
	if host {
		resolved, err := filepath.EvalSymlinks(path)
		if err != nil {
			return version{}, false
		}
		path = resolved
	}
	info, err := os.Lstat(path)
	if err != nil || !info.Mode().IsRegular() {
		return version{}, false
	}
	content, err := os.ReadFile(path) //nolint:gosec // the auth of the host or a copy in a Run configuration, by design
	if err != nil {
		return version{}, false
	}
	stamp, err := auth.AuthStamp(content)
	if err != nil {
		b.log.Debug("an auth file is not valid; it is not propagated nor replaced", "file", path)
		return version{}, false
	}
	return version{path: path, content: content, stamp: stamp}, true
}

// replace writes from over the file at path, if the file is still valid and
// older than from: the harness may have refreshed it since it was read.
func (b *Broker) replace(auth harness.AuthSyncer, path string, from version) {
	cur, ok := b.read(auth, path, false)
	if !ok || !cur.stamp.Before(from.stamp) {
		return
	}
	if err := atomicfile.Write(path, from.content, 0o600); err != nil {
		b.log.Warn("propagating the auth did not finish; it is tried again", "file", path, "err", err)
	}
}
