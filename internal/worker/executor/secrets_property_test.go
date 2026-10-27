//go:build linux || darwin

package executor

import (
	"bytes"
	"cmp"
	"context"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	cfruntime "github.com/Autarquico/codefabric/internal/adapters/runtime"
	"github.com/Autarquico/codefabric/internal/adapters/secrets"
	v1 "github.com/Autarquico/codefabric/internal/api/v1"
	"github.com/Autarquico/codefabric/internal/controller/store"
	"github.com/Autarquico/codefabric/internal/domain"
	"github.com/Autarquico/codefabric/internal/worker/gitops"
	"github.com/Autarquico/codefabric/internal/worker/secretjit"
	"github.com/Autarquico/codefabric/internal/worker/state"
)

// The properties of the secrets of #23 at the level of the Worker (P2 of §7,
// as §11.9 amends it): the real executor and the real Controller in one
// process, on the world of the convergence test, with a catalog that counts
// every value read. The daemon crashes at the hooks between the grant, the
// read of the values, the delivery, the stop and the release; answers of the
// grant are lost; the catalog and the allowlist change in the middle of a
// prepare; a manifest is corrupted and the capture of a Run dies. Every read
// of a value is tied to the attempt and the grant it was read under, and the
// values are looked for, step by step, everywhere only refs belong.
//
// CF_SECRETS_SEED replays one seed; CF_SECRETS_SEEDS runs that many from
// CF_SECRETS_FROM (1).

// crash is the panic a hook raises to stand for the daemon dying there: the
// step that was running unwinds, and the world restarts the daemon.
type crash struct{ at string }

// maxTasksOfWorld bounds the Tasks a seed creates.
const maxTasksOfWorld = 8

// secretRefsOfWorld are the refs the catalog of the Worker may hold.
var secretRefsOfWorld = []string{"gh/token", "api/key", "npm/token"}

// projectElsewhere is a Project no Run of the world is of.
const projectElsewhere = "project_01JZZZZZZZZZZZZZZZZZZZZZZZ"

// grantCallOf is a grant the executor asked for: what it asked, what the
// Controller decided, and whether the answer reached it.
type grantCallOf struct {
	run         string
	attempt     uint64
	leaseEpoch  uint64
	workerEpoch string
	refs        []string
	granted     bool
	answered    bool
}

// grantingController is the Controller as the executor sees it, with hooks
// around the grant: its answer can be lost, the daemon can die just after the
// Controller decided, and anything can happen between the grant and the read.
type grantingController struct {
	*appController
	s *secretWorld
}

func (c *grantingController) GrantSecrets(ctx context.Context, run string, req v1.GrantSecretsRequest) (v1.GrantSecretsResponse, error) {
	s := c.s
	r, err := state.RunBy(ctx, s.f.db.Reader(), s.keyOf(run, req.LeaseEpoch))
	if err != nil {
		s.t.Fatalf("a grant for a Run the Worker does not hold: %v", err)
	}
	call := &grantCallOf{run: run, attempt: r.PrepareAttempt, leaseEpoch: req.LeaseEpoch, workerEpoch: req.WorkerEpoch, refs: slices.Clone(req.Refs)}
	s.lastGrant = call
	allowed := maps.Clone(s.allow) // the linearization point: the grant decides on the allowlist of now
	out, err := c.appController.GrantSecrets(ctx, run, req)
	s.requested += len(req.Refs)
	call.granted = err == nil
	if call.granted {
		for _, ref := range req.Refs {
			if !allowed[ref] {
				s.t.Fatalf("S2: %q granted for %s e%d outside the allowlist %v", ref, run, req.LeaseEpoch, allowed)
			}
		}
	}
	s.late = append(s.late, lateGrant{run: run, req: req})
	if s.calm {
		call.answered = true
		return out, err
	}
	switch s.rng.IntN(12) {
	case 0:
		s.log = append(s.log, "grant:lost")
		return v1.GrantSecretsResponse{}, errLost
	case 1:
		s.log = append(s.log, "grant:crash")
		panic(crash{at: "after the grant"})
	case 2, 3:
		s.interfere("between the grant and the read")
	}
	call.answered = true
	return out, err
}

// lateGrant is a grant request that may arrive again much later.
type lateGrant struct {
	run string
	req v1.GrantSecretsRequest
}

// hookedRuntime is the fake Runtime with a hook in its listing, which the
// release asks under the lock of the run directory.
type hookedRuntime struct {
	*fakeRuntime
	onList func()
}

func (r *hookedRuntime) List(ctx context.Context) ([]string, error) {
	if hook := r.onList; hook != nil {
		r.onList = nil
		hook()
	}
	return r.fakeRuntime.List(ctx)
}

// secretWorld is the world of the convergence test with secrets.
type secretWorld struct {
	*world
	store *fakeStore
	rt    *hookedRuntime
	gen   uint64 // the generation of the daemon
	seq   uint64 // the seq of its secrets, in this generation
	allow map[string]bool
	tasks []string

	values    map[string]bool // every value the catalog ever held
	foreign   map[string]bool // values held only for a Project no Run is of
	lastGrant *grantCallOf
	late      []lateGrant
	requested int // refs asked for, each with its audit entry
	reads     int

	holds     map[state.Key]func() // captures that are draining
	seen      map[state.Key]bool   // Runs whose delivery was seen
	corrupt   map[state.Key]bool   // Runs whose manifest the test broke
	forgotten map[state.Key]bool   // Runs a state.db that was lost knew
	guarded   int                  // sealings and pushes checked against the values
	keys      map[state.Key]bool
	calm      bool // nothing is lost and nothing crashes: the drain
}

func newSecretWorld(t *testing.T, seed uint64) *secretWorld {
	t.Helper()
	w := newWorld(t, seed)
	s := &secretWorld{
		world: w, allow: map[string]bool{}, values: map[string]bool{}, foreign: map[string]bool{},
		holds: map[state.Key]func(){}, seen: map[state.Key]bool{}, corrupt: map[state.Key]bool{}, keys: map[state.Key]bool{},
		forgotten: map[state.Key]bool{},
	}
	s.store = newFakeStore(nil)
	for _, ref := range secretRefsOfWorld {
		if s.rng.IntN(10) != 0 {
			s.setSecret(ref)
		}
	}
	s.store.onGet = s.onGet
	s.rt = &hookedRuntime{fakeRuntime: w.f.rt}
	w.f.opts.Secrets, w.f.opts.Runtime = s.store, s.rt
	w.f.opts.RedactPipeArgv = []string{"/cf", "worker", "redact-pipe"}
	w.f.opts.Controller = &grantingController{appController: w.ctrl, s: s}
	w.g.guard = s.guard
	for _, ref := range secretRefsOfWorld {
		if s.rng.IntN(10) != 0 {
			s.setAllowed(ref, true)
		}
	}
	s.restart(false)
	return s
}

func (s *secretWorld) keyOf(run string, epoch uint64) state.Key {
	return state.Key{RunID: run, LeaseEpoch: epoch}
}

func (s *secretWorld) setSecret(ref string) {
	value := fmt.Sprintf("cfsv-%016x%016x", s.rng.Uint64(), s.rng.Uint64())
	s.values[value] = true
	must(s.t, s.store.Set(s.t.Context(), ref, []byte(value), nil))
}

func (s *secretWorld) setAllowed(ref string, allow bool) {
	req := v1.ProjectSecretsRequest{Refs: []string{ref}}
	var err error
	if allow {
		_, err = s.ctrl.app.Projects.AllowSecrets(s.t.Context(), s.owner, "backend", req)
	} else {
		_, err = s.ctrl.app.Projects.DenySecrets(s.t.Context(), s.owner, "backend", req)
	}
	must(s.t, err)
	s.allow[ref] = allow
}

// restart is a new daemon, after a crash or not, with the next generation, and
// its first heartbeat.
func (s *secretWorld) restart(loseState bool) {
	if loseState {
		// What the lost state.db knew is no longer retained: the janitor may
		// release it, and its publication, if it comes back, fails closed (guard).
		for _, k := range s.knownKeys() {
			s.forgotten[k] = true
		}
	}
	// The recovery prepares Runs too, so the daemon may die again in it.
	for {
		var crashed bool
		s.releasing("in the recovery", func() { crashed = s.recovered(func() { s.world.restart(loseState) }) })
		if !crashed {
			break
		}
		s.log = append(s.log, "crash in the recovery")
		loseState = false
	}
	s.gen++
	s.seq = 0
	s.lastGrant = nil
	s.heartbeat()
	// A grant a dead daemon sent arrives now: the Controller refuses it.
	var late []lateGrant
	for _, g := range s.late {
		if g.req.WorkerEpoch != s.beat {
			late = append(late, g)
		}
	}
	if len(late) > 0 && s.rng.IntN(2) == 0 {
		g := late[s.rng.IntN(len(late))]
		before := s.audits()
		_, err := s.ctrl.app.Runs.GrantSecrets(s.t.Context(), s.ctrl.w, g.run, g.req)
		s.requested += len(g.req.Refs)
		var coded *domain.Error
		if err == nil || !errors.As(err, &coded) || coded.Code != domain.CodeStaleLease {
			s.t.Fatalf("S2: a grant of a daemon that is gone was answered %v", err)
		}
		if n := s.audits() - before; n != len(g.req.Refs) {
			s.t.Fatalf("S2: a late grant of %d refs left %d audit entries", len(g.req.Refs), n)
		}
		s.log = append(s.log, "late-grant")
	}
	s.late = nil
	if !s.calm && s.rng.IntN(4) != 0 {
		s.reconcile(s.rng.IntN(6) == 0) // what a daemon does first, unless its answer is lost
	}
}

// heartbeat says what the catalog holds, as the daemon of this generation.
func (s *secretWorld) heartbeat() {
	catalog, err := s.store.List(s.t.Context())
	must(s.t, err)
	metas := []v1.SecretMeta{}
	for _, m := range catalog {
		metas = append(metas, v1.SecretMeta{Ref: m.Ref, Backend: v1.SecretBackendFile, Projects: m.Projects, UpdatedAt: t0})
	}
	s.seq++
	_, err = s.ctrl.app.Workers.Heartbeat(s.t.Context(), s.ctrl.w, v1.HeartbeatRequest{
		Epoch: s.beat, Generation: s.gen, Hostname: "box", OS: "linux", Arch: "amd64", Capabilities: []string{"harness:command"},
		Total: v1.Capacity{CPU: 16, MemoryMB: 65536, DiskMB: 1000}, Health: v1.Health{Status: v1.HealthOK},
		Secrets: &v1.SecretsReport{Seq: s.seq, Refs: metas},
	})
	must(s.t, err)
	s.f.e.HeartbeatTaken()
}

// onGet is W1: a value is read only in the attempt of a grant whose answer
// reached the executor, for a ref it granted, which the Controller kept as
// given to this daemon for that lease. The daemon may die right there, or
// something may change before the value is delivered.
func (s *secretWorld) onGet(ref string) {
	g := s.lastGrant
	switch {
	case g == nil || !g.answered || !g.granted:
		s.t.Fatalf("W1: %q read with no grant answered (%+v)", ref, g)
	case g.workerEpoch != s.f.j.Epoch():
		s.t.Fatalf("W1: %q read under a grant of another daemon", ref)
	case !slices.Contains(g.refs, ref):
		s.t.Fatalf("W1: %q read under a grant of %v", ref, g.refs)
	}
	r, err := state.RunBy(s.t.Context(), s.f.db.Reader(), s.keyOf(g.run, g.leaseEpoch))
	if err != nil || r.PrepareAttempt != g.attempt {
		s.t.Fatalf("W1: %q read in attempt %d of %s, granted to attempt %d (%v)", ref, r.PrepareAttempt, g.run, g.attempt, err)
	}
	var grants []store.SecretGrant
	must(s.t, s.db.Read(s.t.Context(), func(q store.Querier) (err error) {
		grants, err = store.SecretGrantsOf(s.t.Context(), q, domain.ID(g.run))
		return err
	}))
	if !slices.ContainsFunc(grants, func(kept store.SecretGrant) bool {
		return kept.Granted() && kept.LeaseEpoch == g.leaseEpoch && kept.WorkerEpoch == g.workerEpoch &&
			slices.ContainsFunc(kept.Refs, func(v store.SecretVerdict) bool { return v.Ref == ref })
	}) {
		s.t.Fatalf("W1: %q read for %s e%d with no grant kept by the Controller", ref, g.run, g.leaseEpoch)
	}
	s.reads++
	if s.calm {
		return
	}
	switch s.rng.IntN(12) {
	case 0:
		s.log = append(s.log, "get:crash")
		panic(crash{at: "reading " + ref})
	case 1:
		s.interfere("between the read and the delivery")
	}
}

// interfere is what may happen in the middle of a prepare: the catalog or the
// allowlist changes, a ref among them taking a value for another Project, or
// the Task is cancelled.
func (s *secretWorld) interfere(where string) {
	ref := secretRefsOfWorld[s.rng.IntN(len(secretRefsOfWorld))]
	switch s.rng.IntN(4) {
	case 0:
		s.log = append(s.log, "catalog-rm "+where)
		must(s.t, s.store.Delete(s.t.Context(), ref))
	case 1:
		s.log = append(s.log, "deny "+where)
		s.setAllowed(ref, false)
	case 2:
		s.log = append(s.log, "cancel "+where)
		_, _ = s.ctrl.app.Tasks.Cancel(s.t.Context(), s.owner, s.tasks[s.rng.IntN(len(s.tasks))], "", v1.ActionRequest{})
	case 3:
		s.log = append(s.log, "catalog-rescope "+where)
		value := fmt.Sprintf("cfsv-%016x%016x", s.rng.Uint64(), s.rng.Uint64())
		s.values[value], s.foreign[value] = true, true
		must(s.t, s.store.Set(s.t.Context(), ref, []byte(value), []string{projectElsewhere}))
	}
}

// crashable runs fn, and restarts the daemon if a hook crashed it.
func (s *secretWorld) crashable(fn func()) {
	if s.recovered(fn) {
		s.restart(false)
	}
}

// recovered runs fn and says whether a hook crashed it.
func (s *secretWorld) recovered(fn func()) (crashed bool) {
	defer func() {
		if p := recover(); p != nil {
			if _, ok := p.(crash); !ok {
				panic(p)
			}
			crashed = true
		}
	}()
	fn()
	return false
}

func (s *secretWorld) audits() int {
	var n int
	must(s.t, s.db.Reader().QueryRowContext(s.t.Context(),
		`SELECT COUNT(*) FROM audit_entries WHERE action = ?`, store.AuditSecretGrant).Scan(&n))
	return n
}

func (s *secretWorld) createTask() {
	var env []v1.SecretEnv
	for i, ref := range secretRefsOfWorld {
		if s.rng.IntN(2) == 0 || (i == len(secretRefsOfWorld)-1 && len(env) == 0) {
			env = append(env, v1.SecretEnv{Name: fmt.Sprintf("SECRET_%d", i), Ref: ref})
		}
	}
	created, err := s.ctrl.app.Tasks.Create(s.t.Context(), s.owner, v1.CreateTaskRequest{
		Project: "backend", Prompt: "echo hi", ResolvedRef: "main", SourceCommit: "0123456789abcdef0123456789abcdef01234567",
		Requirements: v1.Requirements{Harness: "command", SecretEnv: env},
	})
	must(s.t, err)
	s.tasks = append(s.tasks, created.Task.ID)
}

// harness runs the harness of a Run whose session is alive: the wrapper
// resolves the secrets it may read, and the capture of its output, held at
// times, outlives it until it closes or dies.
func (s *secretWorld) harness(r state.Run) {
	k := r.Key
	name := cfruntime.NameFor(r.RunID, r.LeaseEpoch)
	if st, _ := s.f.rt.Status(s.t.Context(), s.f.rt.HandleFor(name)); !st.Alive {
		return
	}
	dir := s.f.runDir(k)
	resolve := secretjit.Resolver(dir)
	for _, ref := range parseSpec(r.Spec).refs() {
		_, _ = resolve(ref) // what the wrapper reads, if it still may
	}
	if s.holds[k] == nil && s.rng.IntN(3) != 0 {
		release, err := HoldCapture(dir)
		must(s.t, err)
		s.holds[k] = release
	}
	s.harnessExit(r)
}

// closeCapture ends the capture of a Run: it drained, or its process died.
func (s *secretWorld) closeCapture() {
	keys := slices.SortedFunc(maps.Keys(s.holds), func(a, b state.Key) int { return strings.Compare(a.RunID, b.RunID) })
	if len(keys) == 0 {
		return
	}
	k := keys[s.rng.IntN(len(keys))]
	s.holds[k]()
	delete(s.holds, k)
}

// guard stands for the guard of the publication: every Run of the world
// declares secrets, so every sealing and every push is checked against their
// values, never against none, and only against values of the catalog. At
// times it finds one, and the publication is blocked.
func (s *secretWorld) guard(f gitops.Forbidden) error {
	if len(f.Values) == 0 {
		s.t.Fatalf("guard: a result of a Run with secrets was sealed or pushed unchecked")
	}
	for _, v := range f.Values {
		if !s.values[string(v.Value)] {
			s.t.Fatalf("guard: checked against %q, which is no value of the catalog", v.Ref)
		}
	}
	s.guarded++
	if !s.calm && s.rng.IntN(10) == 0 {
		s.log = append(s.log, "guard:blocked")
		return &gitops.BlockedError{Reason: "a secret was found"}
	}
	return nil
}

// corruptManifest breaks the manifest of a delivery.
func (s *secretWorld) corruptManifest() {
	for _, k := range s.knownKeys() {
		path := filepath.Join(s.f.runDir(k), "secrets", "manifest.json")
		if _, err := os.Stat(path); err == nil {
			must(s.t, os.WriteFile(path, []byte("{"), 0o600))
			s.corrupt[k] = true
			s.log = append(s.log, "corrupt "+k.RunID)
			return
		}
	}
}

// release has the executor release what is queued, at times with a listing
// of the Runtime that fails, or with the daemon dying under the lock.
func (s *secretWorld) release() {
	switch s.rng.IntN(5) {
	case 0:
		s.rt.mu.Lock()
		s.rt.listErr = errors.New("tmux: no server")
		s.rt.mu.Unlock()
		defer func() {
			s.rt.mu.Lock()
			s.rt.listErr = nil
			s.rt.mu.Unlock()
		}()
	case 1:
		s.rt.onList = func() { panic(crash{at: "in the release"}) }
		defer func() { s.rt.onList = nil }()
	}
	s.releasing("in the release", func() { s.crashable(func() { s.f.e.releaseQueued(s.t.Context()) }) })
}

func (s *secretWorld) janitor() {
	if s.rng.IntN(5) == 0 {
		s.rt.onList = func() { panic(crash{at: "in the janitor"}) }
		defer func() { s.rt.onList = nil }()
	}
	s.releasing("in the janitor", func() { s.crashable(func() { _ = s.f.e.sweepSecrets(s.t.Context()) }) })
}

func (s *secretWorld) knownKeys() []state.Key {
	all, err := state.AllRuns(s.t.Context(), s.f.db.Reader())
	must(s.t, err)
	for _, r := range all {
		s.keys[r.Key] = true
	}
	return slices.SortedFunc(maps.Keys(s.keys), func(a, b state.Key) int {
		if c := strings.Compare(a.RunID, b.RunID); c != 0 {
			return c
		}
		return cmp.Compare(a.LeaseEpoch, b.LeaseEpoch)
	})
}

// needs says why the Run k still needs its values to sanitise: its capture
// is draining, or its publication is not final; "" if it does not. A Run
// whose manifest the test broke, or that a lost state.db knew, is left out.
func (s *secretWorld) needs(k state.Key) string {
	if s.corrupt[k] || s.forgotten[k] {
		return ""
	}
	if s.holds[k] != nil {
		return "its capture is held"
	}
	p, err := state.PublicationOf(s.t.Context(), s.f.db.Reader(), k)
	if err == nil && p.State != state.PubPublished && p.State != state.PubFailed {
		return fmt.Sprintf("its publication is %q", p.State)
	}
	return ""
}

// delivered says whether the values of the Run k are in its directory.
func (s *secretWorld) delivered(k state.Key) bool {
	_, err := secretjit.Load(s.f.runDir(k))
	return err == nil
}

// releasing runs fn, which may release secrets, and checks W2 where the
// release decides rather than after a whole step, in which a publication may
// fail closed for want of its values and so look final: every delivery a Run
// needed when fn began, and still needs when it ends, is still there. A pass of
// the release changes nothing a Run needs its values for, so a delivery gone
// across it was released while it was needed.
func (s *secretWorld) releasing(what string, fn func()) {
	s.t.Helper()
	var needed []state.Key
	for _, k := range s.knownKeys() {
		if s.needs(k) != "" && s.delivered(k) {
			needed = append(needed, k)
		}
	}
	fn()
	for _, k := range needed {
		if why := s.needs(k); why != "" && !s.delivered(k) {
			s.t.Fatalf("W2 %s: the values of %s e%d were released while %s", what, k.RunID, k.LeaseEpoch, why)
		}
	}
}

// check is what holds after every step. W2: the values of a Run stay while
// it needs them to sanitise, whoever releases (and releasing checks it as
// each release decides). W4: no file of the Worker but a delivery holds a
// value. W6: no delivery holds a value the catalog held only for another
// Project.
func (s *secretWorld) check(what string) {
	s.t.Helper()
	for _, k := range s.knownKeys() {
		if needles, err := secretjit.Load(s.f.runDir(k)); err == nil {
			s.seen[k] = true
			for _, n := range needles {
				if s.foreign[string(n.Value)] {
					s.t.Fatalf("W6 %s: %s e%d was given %q, held only for another Project", what, k.RunID, k.LeaseEpoch, n.Ref)
				}
			}
		}
		if !s.seen[k] {
			continue
		}
		if why := s.needs(k); why != "" && !s.delivered(k) {
			s.t.Fatalf("W2 %s: the values of %s e%d are gone while %s", what, k.RunID, k.LeaseEpoch, why)
		}
	}
	s.noValueOutside(what, true)
}

// noValueOutside fails if a value, as it is, in base64 or in hex, is in a file
// under the root of the Worker; skipDeliveries leaves out the deliveries.
func (s *secretWorld) noValueOutside(what string, skipDeliveries bool) {
	s.t.Helper()
	root := filepath.Dir(s.f.paths.DataDir)
	var forms [][]byte
	for v := range s.values {
		forms = append(forms, []byte(v), []byte(base64.StdEncoding.EncodeToString([]byte(v))), []byte(hex.EncodeToString([]byte(v))))
	}
	_ = filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil //nolint:nilerr // a file that went while walking holds nothing
		}
		if d.IsDir() {
			if skipDeliveries && strings.HasPrefix(d.Name(), "secrets") {
				return filepath.SkipDir
			}
			return nil
		}
		raw, err := os.ReadFile(path) //nolint:gosec // a file of the test
		if err != nil {
			return nil //nolint:nilerr // as above
		}
		for _, f := range forms {
			if bytes.Contains(raw, f) {
				s.t.Fatalf("W4 %s: %s holds a value", what, path)
			}
		}
		return nil
	})
}

// step does one thing chosen by the seed.
func (s *secretWorld) step(i int) {
	ctx := s.t.Context()
	action := s.rng.IntN(21)
	switch action {
	case 0, 19:
		s.longPoll(s.rng.IntN(5) == 0)
	case 1, 20:
		if s.rng.IntN(5) == 0 {
			s.ctrl.mu.Lock()
			s.ctrl.lose["accept"] = true
			s.ctrl.mu.Unlock()
		}
		s.f.e.decideOnce(ctx)
		s.ctrl.mu.Lock()
		delete(s.ctrl.lose, "accept")
		s.ctrl.mu.Unlock()
	case 2, 3:
		s.crashable(s.stepAll)
	case 4:
		live, err := state.LiveRuns(ctx, s.f.db.Reader())
		must(s.t, err)
		if len(live) > 0 {
			s.harness(live[s.rng.IntN(len(live))])
		}
	case 5:
		// Only when there is something to publish: the publisher moves the clock past its waits.
		if pubs, err := state.UnsettledPublications(ctx, s.f.db.Reader()); err == nil && len(pubs) > 0 {
			s.crashable(func() { s.publish(s.rng.IntN(3) == 0) })
		}
	case 6:
		s.flush(s.rng.IntN(4) == 0)
	case 7:
		s.reconcile(s.rng.IntN(5) == 0)
	case 8:
		if i >= s.lateFrom && s.rng.IntN(8) == 0 {
			_, _ = s.ctrl.app.Tasks.Cancel(ctx, s.owner, s.tasks[s.rng.IntN(len(s.tasks))], "", v1.ActionRequest{})
		}
	case 9:
		if s.rng.IntN(2) == 0 {
			s.restart(s.rng.IntN(4) == 0)
		}
	case 10:
		// Work keeps coming: a seed whose Tasks all ended early (an offer that
		// expired, a cancel) would otherwise have nothing left to prepare.
		if !slices.ContainsFunc(s.runs(), func(r store.Run) bool { return !r.State.IsTerminal() }) && len(s.tasks) < maxTasksOfWorld {
			s.createTask()
			s.log = append(s.log, "task")
		}
		_, err := s.ctrl.app.Scheduler.Pass(ctx)
		must(s.t, err)
	case 11:
		if s.rng.IntN(4) == 0 {
			s.f.clock.Advance(domain.Defaults().LeaseAcceptBy + time.Second)
			_, err := s.ctrl.app.Leases.SweepExpired(ctx)
			must(s.t, err)
		}
	case 12:
		s.release()
	case 13:
		s.janitor()
	case 14:
		s.closeCapture()
	case 15:
		ref := secretRefsOfWorld[s.rng.IntN(len(secretRefsOfWorld))]
		if s.rng.IntN(5) == 0 {
			must(s.t, s.store.Delete(ctx, ref))
		} else {
			s.setSecret(ref)
		}
	case 16:
		s.setAllowed(secretRefsOfWorld[s.rng.IntN(len(secretRefsOfWorld))], s.rng.IntN(6) != 0)
	case 17:
		if s.rng.IntN(3) == 0 {
			s.corruptManifest()
		}
	case 18:
		s.f.clock.Advance(prepareRetryInitial * time.Duration(1+s.rng.IntN(8))) // a prepare that was put off comes due
	}
	s.log = append(s.log, strconv.Itoa(action))
	s.heartbeat()
	s.check(fmt.Sprintf("after step %d (action %d)", i, action))
}

// drain lets everything finish: the catalog, for every Project, and the
// allowlist hold every ref again, every capture closes, no answer is lost, one
// more Task comes, and every harness ends.
// Then W3: no delivery is left in any run directory, and no value anywhere.
func (s *secretWorld) drain() {
	ctx := s.t.Context()
	s.calm = true
	held, err := s.store.List(ctx)
	must(s.t, err)
	for _, ref := range secretRefsOfWorld {
		if !slices.ContainsFunc(held, func(m secrets.Metadata) bool { return m.Ref == ref && len(m.Projects) == 0 }) {
			s.setSecret(ref)
		}
		s.setAllowed(ref, true)
	}
	for k, release := range s.holds {
		release()
		delete(s.holds, k)
	}
	// One Task more, so that every seed takes at least one Run with secrets
	// through its whole life: grant, delivery, harness, sealing, push, release.
	s.createTask()
	settled := false
	for range 60 {
		s.heartbeat()
		s.longPoll(false)
		if !s.f.e.Reconciled() {
			s.reconcile(false)
		}
		s.f.e.decideOnce(ctx)
		s.f.clock.Advance(prepareRetryMax + time.Second)
		s.stepAll()
		live, err := state.LiveRuns(ctx, s.f.db.Reader())
		must(s.t, err)
		for _, r := range live {
			if r.Phase == state.PhaseRunning || r.Phase == state.PhaseStopping {
				s.harnessExit(r)
			}
		}
		s.f.clock.Advance(noWrapperLimit + time.Second)
		s.stepAll()
		s.publish(false)
		s.flush(false)
		_, err = s.ctrl.app.Scheduler.Pass(ctx)
		must(s.t, err)
		s.releasing("in the drain", func() { s.f.e.releaseQueued(ctx) })
		if s.quiet() {
			s.reconcile(false)
			s.publish(false)
			s.flush(false)
			if s.quiet() {
				settled = true
				break
			}
		}
		s.check("drain")
	}
	if !settled {
		s.dump()
		s.t.Fatalf("no convergence after the drain; actions %v", s.log)
	}
	s.releasing("after the drain", func() {
		s.f.e.releaseQueued(ctx)
		must(s.t, s.f.e.sweepSecrets(ctx))
	})
	runs := filepath.Join(s.f.paths.WorkerDataDir(), "runs")
	entries, _ := os.ReadDir(runs)
	for _, e := range entries {
		for _, name := range []string{"secrets", "secrets.staging", "secrets.deleting"} {
			if _, err := os.Lstat(filepath.Join(runs, e.Name(), name)); err == nil {
				s.t.Fatalf("W3: %s/%s is left after the drain", e.Name(), name)
			}
		}
	}
	s.noValueOutside("after the drain", false)
	for _, table := range []string{"audit_entries", "events", "runs", "secret_grants", "secret_grant_refs", "worker_secrets", "idempotency_keys"} {
		s.noValueInTable(table)
	}
}

// noValueInTable is S3 on the Controller: no row holds a value.
func (s *secretWorld) noValueInTable(table string) {
	rows, err := s.db.Reader().QueryContext(s.t.Context(), `SELECT * FROM `+table) //nolint:gosec // a table of the test
	if err != nil {
		return // a table this schema does not have
	}
	defer func() { _ = rows.Close() }()
	cols, _ := rows.Columns()
	for rows.Next() {
		cells := make([]any, len(cols))
		ptrs := make([]any, len(cols))
		for i := range cells {
			ptrs[i] = &cells[i]
		}
		must(s.t, rows.Scan(ptrs...))
		line := fmt.Sprint(cells...)
		for v := range s.values {
			if strings.Contains(line, v) {
				s.t.Fatalf("S3: a row of %s holds a value", table)
			}
		}
	}
}

// TestProperty_SecretsWorker: W1 (no read without its grant), W2 (the values
// stay while the capture or the publication needs them), W3 (nothing is left
// once all is over), W4 (no value outside a delivery), W5 (one audit entry per
// ref asked for, and no more reads than refs granted), W6 (no value held only
// for another Project is delivered), with the daemon crashing at the hooks of
// the secrets.
func TestProperty_SecretsWorker(t *testing.T) {
	name := t.Name()
	for _, seed := range secretSeedsOfWorld(t, 3) {
		t.Run(fmt.Sprintf("seed=%d", seed), func(t *testing.T) {
			replay := fmt.Sprintf("CF_SECRETS_SEED=%d go test -race -count=1 -run '^%s$' ./internal/worker/executor/", seed, name)
			t.Logf("seed %d (replay: %s)", seed, replay)
			s := newSecretWorld(t, seed)
			t.Cleanup(func() {
				if t.Failed() {
					t.Logf("actions: %v", s.log)
					t.Logf("FAILED seed %d; replay: %s", seed, replay)
				}
			})
			for range 3 {
				s.createTask()
			}
			_, err := s.ctrl.app.Scheduler.Pass(t.Context())
			must(t, err)
			s.lateFrom = 20 + s.rng.IntN(60)
			for i := range 100 {
				s.step(i)
			}
			s.drain()
			if n := s.audits(); n != s.requested {
				t.Fatalf("W5: %d secret.grant audit entries for %d refs asked for", n, s.requested)
			}
			t.Logf("%d refs asked for, %d values read, %d sealings and pushes guarded", s.requested, s.reads, s.guarded)
		})
	}
}

// secretSeedsOfWorld is CF_SECRETS_SEED alone, or CF_SECRETS_SEEDS (def) seeds
// from CF_SECRETS_FROM (1).
func secretSeedsOfWorld(t *testing.T, def uint64) []uint64 {
	t.Helper()
	if text := os.Getenv("CF_SECRETS_SEED"); text != "" {
		seed, err := strconv.ParseUint(text, 0, 64)
		if err != nil {
			t.Fatalf("CF_SECRETS_SEED=%q is not a seed", text)
		}
		return []uint64{seed}
	}
	n, from := envUint("CF_SECRETS_SEEDS", def), envUint("CF_SECRETS_FROM", 1)
	seeds := make([]uint64, n)
	for i := range seeds {
		seeds[i] = from + uint64(i)
	}
	return seeds
}
