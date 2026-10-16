package app_test

import (
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"maps"
	"math/rand/v2"
	"os"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/oklog/ulid/v2"

	v1 "github.com/Autarquico/codefabric/internal/api/v1"
	"github.com/Autarquico/codefabric/internal/app/workers"
	"github.com/Autarquico/codefabric/internal/controller/pki"
	"github.com/Autarquico/codefabric/internal/controller/store"
	"github.com/Autarquico/codefabric/internal/domain"
)

// The properties of the secrets of #23 at the level of the Controller (P1 of
// §7, as §11.9 amends it): the real use cases on a real store, two Workers
// whose daemons keep a catalog of their own and send heartbeats that are
// taken late, twice or never, restart with or without their state, and ask
// for grants on time, late, or without hearing the answer. A model written
// apart from the code says what each heartbeat and each grant must get, at
// the moment the Controller decides it, and every read of a value is checked
// against the grant that allowed it.

// The seeds of the properties of the secrets: CF_SECRETS_SEED replays one, and
// otherwise CF_SECRETS_SEEDS of them run from CF_SECRETS_FROM (1).
const (
	envSecretsSeed  = "CF_SECRETS_SEED"  //nolint:gosec // the name of a variable of the environment
	envSecretsSeeds = "CF_SECRETS_SEEDS" //nolint:gosec // the name of a variable of the environment
	envSecretsFrom  = "CF_SECRETS_FROM"
)

// actionTrace is what a seed did, to say it when it fails.
type actionTrace []string

func (a *actionTrace) add(format string, args ...any) { *a = append(*a, fmt.Sprintf(format, args...)) }

func (a actionTrace) String() string { return strings.Join(a, "; ") }

func secretSeeds(t *testing.T, def uint64) []uint64 {
	t.Helper()
	if text := os.Getenv(envSecretsSeed); text != "" {
		seed, err := strconv.ParseUint(text, 0, 64)
		if err != nil {
			t.Fatalf("%s=%q is not a seed", envSecretsSeed, text)
		}
		return []uint64{seed}
	}
	n, from := def, uint64(1)
	if text := os.Getenv(envSecretsSeeds); text != "" {
		v, err := strconv.ParseUint(text, 0, 64)
		if err != nil || v == 0 {
			t.Fatalf("%s=%q is not a number of seeds", envSecretsSeeds, text)
		}
		n = v
	}
	if text := os.Getenv(envSecretsFrom); text != "" {
		v, err := strconv.ParseUint(text, 0, 64)
		if err != nil {
			t.Fatalf("%s=%q is not a seed", envSecretsFrom, text)
		}
		from = v
	}
	seeds := make([]uint64, n)
	for i := range seeds {
		seeds[i] = from + uint64(i)
	}
	return seeds
}

// eachSecretSeed runs prop once per seed, with a PCG of that seed. The seed is
// said first, and again with the actions when the seed fails.
func eachSecretSeed(t *testing.T, def uint64, pkg string, prop func(t *testing.T, rng *rand.Rand, trace *actionTrace)) {
	t.Helper()
	name := t.Name()
	for _, seed := range secretSeeds(t, def) {
		t.Run(fmt.Sprintf("seed=%d", seed), func(t *testing.T) {
			replay := fmt.Sprintf("%s=%d go test -race -count=1 -run '^%s$' %s", envSecretsSeed, seed, name, pkg)
			t.Logf("seed %d (replay: %s)", seed, replay)
			var trace actionTrace
			t.Cleanup(func() {
				if t.Failed() {
					t.Logf("actions: %s", trace)
					t.Logf("FAILED seed %d; replay: %s", seed, replay)
				}
			})
			prop(t, rand.New(rand.NewPCG(seed, seed^0x9e3779b97f4a7c15)), &trace) //nolint:gosec // a seed to replay, not a secret
		})
	}
}

// secretRefs are the refs the seeds play with.
var secretRefs = []string{"gh/token", "npm/token", "pg/url", "aws/key"}

// catalogEntry is a secret a simulated daemon holds.
type catalogEntry struct {
	projects  []string // none: any Project
	updatedAt time.Time
	value     string
}

// simDaemon is the daemon of a Worker as the Controller sees it: its
// generation, epoch and seq, its catalog, and the heartbeats it sent that have
// not arrived yet.
type simDaemon struct {
	name      string
	s         pki.Session
	gen       uint64
	epoch     string
	old       []string // the epochs of its earlier runs
	seq       uint64
	reportSeq uint64
	catalog   map[string]catalogEntry
	captured  []v1.HeartbeatRequest
}

func (d *simDaemon) heartbeat() v1.HeartbeatRequest {
	d.seq++
	metas := []v1.SecretMeta{}
	for _, ref := range slices.Sorted(maps.Keys(d.catalog)) {
		c := d.catalog[ref]
		metas = append(metas, v1.SecretMeta{Ref: ref, Backend: v1.SecretBackendFile, Projects: slices.Clone(c.projects), UpdatedAt: c.updatedAt})
	}
	return v1.HeartbeatRequest{
		Epoch: d.epoch, Generation: d.gen, Hostname: "box.lan", OS: "linux", Arch: "amd64",
		Total: v1.Capacity{CPU: 16, MemoryMB: 65536, DiskMB: 500000}, Health: v1.Health{Status: v1.HealthOK},
		Secrets: &v1.SecretsReport{Seq: d.seq, Refs: metas},
	}
}

// holds says whether the catalog lets a Run of project read every ref.
func (d *simDaemon) holds(project string, refs []string) bool {
	for _, ref := range refs {
		c, ok := d.catalog[ref]
		if !ok || (len(c.projects) > 0 && !slices.Contains(c.projects, project)) {
			return false
		}
	}
	return true
}

// modelFence is what the model says the Controller holds of a daemon: the
// position of the last heartbeat it took, and the secrets it said.
type modelFence struct {
	gen   uint64
	epoch string
	seq   uint64
	refs  map[string][]string // ref -> Projects
}

// secretsModel is the state the Controller must decide from: the allowlist of
// each Project and, for each Worker, the heartbeat it took last.
type secretsModel struct {
	allow map[string]map[string]bool // project ID -> refs
	fence map[domain.ID]*modelFence
}

// admit is the fence of §11.6 as the model has it: a heartbeat is taken if its
// generation is above the last one taken, or the same daemon (generation and
// epoch) and not older (seq); otherwise it is refused, and why. A taken one
// moves the fence and, if newer, replaces the secrets.
func (m *secretsModel) admit(w domain.ID, req v1.HeartbeatRequest) (bool, string) {
	f := m.fence[w]
	take := func() {
		refs := map[string][]string{}
		for _, meta := range req.Secrets.Refs {
			refs[meta.Ref] = slices.Sorted(slices.Values(meta.Projects))
		}
		m.fence[w] = &modelFence{gen: req.Generation, epoch: req.Epoch, seq: req.Secrets.Seq, refs: refs}
	}
	switch {
	case f == nil || req.Generation > f.gen:
		take()
		return true, ""
	case req.Generation < f.gen || req.Epoch != f.epoch:
		return false, workers.FenceGenerationRetired
	case req.Secrets.Seq < f.seq:
		return false, workers.FenceOutOfOrder
	case req.Secrets.Seq > f.seq:
		take()
	}
	return true, ""
}

// modelTask is a Task as the model knows it: its Project and the refs it declares.
type modelTask struct {
	project string
	refs    []string
}

// grantCall is a grant as the Worker sends it, kept to be sent late.
type grantCall struct {
	caller *simDaemon
	run    domain.ID
	req    v1.GrantSecretsRequest
}

// pendingRead is a grant whose answer reached the daemon: it reads the values next.
type pendingRead struct {
	call  grantCall
	epoch string // the run of the daemon that got the answer
}

// valueRead is a value a daemon read, with the grant it read it under.
type valueRead struct {
	run         domain.ID
	leaseEpoch  uint64
	workerEpoch string
	ref         string
}

type secretsWorld struct {
	t        *testing.T
	e        *env
	rng      *rand.Rand
	trace    *actionTrace
	projects []v1.Project
	daemons  []*simDaemon
	model    secretsModel
	tasks    map[domain.ID]modelTask
	late     []grantCall
	toRead   []pendingRead
	reads    []valueRead
	granted  int // refs granted, for W5: reads never outnumber them
	outcomes map[string]int
	audited  int // refs decided, each with one audit entry
}

func newSecretsWorld(t *testing.T, rng *rand.Rand, trace *actionTrace) *secretsWorld {
	t.Helper()
	w := &secretsWorld{
		t: t, e: newEnv(t), rng: rng, trace: trace, tasks: map[domain.ID]modelTask{}, outcomes: map[string]int{},
		model: secretsModel{allow: map[string]map[string]bool{}, fence: map[domain.ID]*modelFence{}},
	}
	for _, name := range []string{"backend", "frontend"} {
		p := w.e.project(name)
		w.projects = append(w.projects, p)
		w.model.allow[p.ID] = map[string]bool{}
		for _, ref := range secretRefs {
			if rng.IntN(4) != 0 {
				must(t, w.apply(allowlistChange{project: p.ID, ref: ref, allow: true}))
				w.model.allow[p.ID][ref] = true
			}
		}
	}
	for _, name := range []string{"alpha", "beta"} {
		d := &simDaemon{name: name, s: w.e.joinWorker(name), catalog: map[string]catalogEntry{}}
		for _, ref := range secretRefs {
			if rng.IntN(4) != 0 {
				w.setSecret(d, ref)
			}
		}
		w.daemons = append(w.daemons, d)
		w.restart(d, false)
	}
	return w
}

// sentinel is a value of high entropy, to look for where no value may be.
func (w *secretsWorld) sentinel() string {
	return fmt.Sprintf("cfsv-%016x%016x%016x", w.rng.Uint64(), w.rng.Uint64(), w.rng.Uint64())
}

func (w *secretsWorld) setSecret(d *simDaemon, ref string) {
	var scope []string
	if w.rng.IntN(3) == 0 {
		scope = []string{w.projects[w.rng.IntN(len(w.projects))].ID}
	}
	w.e.clock.Advance(time.Second)
	d.catalog[ref] = catalogEntry{projects: scope, updatedAt: w.e.clock.Now(), value: w.sentinel()}
}

// beat delivers a heartbeat and checks the Controller did with it what the model says.
func (w *secretsWorld) beat(d *simDaemon, req v1.HeartbeatRequest) error {
	w.t.Helper()
	take, reason := w.model.admit(d.s.Principal.ID, req)
	_, err := w.e.app.Workers.Heartbeat(w.t.Context(), d.s, req)
	switch {
	case take && err != nil:
		w.t.Fatalf("heartbeat g%d %s seq %d of %s refused: %v; the model takes it", req.Generation, req.Epoch, req.Secrets.Seq, d.name, err)
	case !take && err == nil:
		w.t.Fatalf("heartbeat g%d %s seq %d of %s taken; the model refuses it (%s)", req.Generation, req.Epoch, req.Secrets.Seq, d.name, reason)
	case !take:
		var coded *domain.Error
		if !errors.As(err, &coded) || coded.Code != domain.CodeStaleLease || coded.Details["reason"] != reason {
			w.t.Fatalf("heartbeat of %s refused with %v, want %s %s", d.name, err, domain.CodeStaleLease, reason)
		}
	}
	return err
}

// restart is a new run of the daemon: a new epoch, and the next generation, or
// with its state lost, one it may have used already. A generation the
// Controller refuses as retired is replaced by one above the Controller's.
func (w *secretsWorld) restart(d *simDaemon, lostState bool) {
	w.t.Helper()
	if d.epoch != "" {
		d.old = append(d.old, d.epoch)
	}
	d.epoch, d.seq = ulid.Make().String(), 0
	if lostState && d.gen > 0 {
		d.gen = 1 + w.rng.Uint64N(d.gen)
	} else {
		d.gen++
	}
	for range 3 {
		err := w.beat(d, d.heartbeat())
		if err == nil {
			return
		}
		var coded *domain.Error
		_ = errors.As(err, &coded)
		g, ok := coded.Details["generation"].(uint64)
		if !ok {
			w.t.Fatalf("a retired generation without the Controller's: %#v", coded.Details)
		}
		d.gen = g + 1
	}
	w.t.Fatalf("daemon %s could not get a generation taken", d.name)
}

func (w *secretsWorld) runs() []store.Run {
	w.t.Helper()
	var out []store.Run
	must(w.t, w.e.db.Read(w.t.Context(), func(q store.Querier) (err error) {
		out, err = store.Runs(w.t.Context(), q, store.RunFilter{})
		return err
	}))
	return out
}

func (w *secretsWorld) daemonOf(id domain.ID) *simDaemon {
	for _, d := range w.daemons {
		if d.s.Principal.ID == id {
			return d
		}
	}
	return nil
}

func (w *secretsWorld) projectName(id string) string {
	for _, p := range w.projects {
		if p.ID == id {
			return p.Name
		}
	}
	return ""
}

// allowlistChange is an allow or a deny of one ref, made or to be made.
type allowlistChange struct {
	project string // ID
	ref     string
	allow   bool
}

func (w *secretsWorld) randomChange() allowlistChange {
	return allowlistChange{
		project: w.projects[w.rng.IntN(len(w.projects))].ID,
		ref:     secretRefs[w.rng.IntN(len(secretRefs))],
		allow:   w.rng.IntN(3) != 0,
	}
}

func (w *secretsWorld) apply(c allowlistChange) error {
	req := v1.ProjectSecretsRequest{Refs: []string{c.ref}}
	var err error
	if c.allow {
		_, err = w.e.app.Projects.AllowSecrets(w.t.Context(), w.e.owner, w.projectName(c.project), req)
	} else {
		_, err = w.e.app.Projects.DenySecrets(w.t.Context(), w.e.owner, w.projectName(c.project), req)
	}
	return err
}

// concurrently does what alongside the change, and returns the allowlists of
// the model before and after it: what the Controller did may be either.
func (w *secretsWorld) concurrently(c allowlistChange, what func()) (before, after map[string]bool) {
	w.t.Helper()
	before = maps.Clone(w.model.allow[c.project])
	done := make(chan error, 1)
	go func() { done <- w.apply(c) }()
	what()
	must(w.t, <-done)
	w.model.allow[c.project][c.ref] = c.allow
	return before, w.model.allow[c.project]
}

func (w *secretsWorld) createTask() {
	p := w.projects[w.rng.IntN(len(w.projects))]
	var env []v1.SecretEnv
	var refs []string
	for i, ref := range secretRefs {
		if w.rng.IntN(len(secretRefs)) == 0 || (i == len(secretRefs)-1 && len(refs) == 0 && w.rng.IntN(4) != 0) {
			env = append(env, v1.SecretEnv{Name: fmt.Sprintf("SECRET_%d", i), Ref: ref})
			refs = append(refs, ref)
		}
	}
	created := w.e.mustTask(p.Name, "x", func(r *v1.CreateTaskRequest) { r.Requirements.SecretEnv = env })
	w.tasks[domain.ID(created.Task.ID)] = modelTask{project: p.ID, refs: refs}
	w.trace.add("task %s %s %v", created.Task.ID, p.Name, refs)
}

// pass runs the scheduler, at times while the allowlist changes, and checks S1:
// every lease it creates is for a Worker that, as the Controller knows it,
// holds every ref of the Run for its Project, all of them in the allowlist.
func (w *secretsWorld) pass() {
	w.t.Helper()
	before := map[domain.ID]uint64{}
	for _, r := range w.runs() {
		before[r.ID] = r.LeaseEpoch
	}
	allowed := func(project, ref string) bool { return w.model.allow[project][ref] }
	if w.rng.IntN(4) == 0 {
		c := w.randomChange()
		w.trace.add("pass while %+v", c)
		was, now := w.concurrently(c, func() { w.e.pass() })
		allowed = func(project, ref string) bool {
			if project == c.project {
				return was[ref] || now[ref]
			}
			return w.model.allow[project][ref]
		}
	} else {
		w.trace.add("pass")
		w.e.pass()
	}
	for _, r := range w.runs() {
		if r.LeaseEpoch == before[r.ID] || r.WorkerID == "" {
			continue
		}
		task := w.tasks[r.TaskID]
		fence := w.model.fence[r.WorkerID]
		for _, ref := range task.refs {
			scope, held := fence.refs[ref]
			switch {
			case !allowed(task.project, ref):
				w.t.Fatalf("S1: %s leased to %s with %q outside the allowlist", r.ID, w.daemonOf(r.WorkerID).name, ref)
			case !held:
				w.t.Fatalf("S1: %s leased to %s, which did not announce %q", r.ID, w.daemonOf(r.WorkerID).name, ref)
			case len(scope) > 0 && !slices.Contains(scope, task.project):
				w.t.Fatalf("S1: %s leased to %s, which holds %q for other Projects", r.ID, w.daemonOf(r.WorkerID).name, ref)
			}
		}
	}
}

// answerLease has the Worker of a pending lease accept it or, if its catalog
// lacks a secret of the Run, reject it with CF_SECRET_UNAVAILABLE.
func (w *secretsWorld) answerLease(r store.Run) {
	w.t.Helper()
	d, task := w.daemonOf(r.WorkerID), w.tasks[r.TaskID]
	if d.holds(task.project, task.refs) {
		w.trace.add("accept %s e%d by %s", r.ID, r.LeaseEpoch, d.name)
		_, err := w.e.app.Leases.Accept(w.t.Context(), d.s, string(r.LeaseID), v1.AcceptLeaseRequest{Epoch: r.LeaseEpoch})
		must(w.t, err)
		return
	}
	w.trace.add("reject %s e%d by %s", r.ID, r.LeaseEpoch, d.name)
	_, err := w.e.app.Leases.Reject(w.t.Context(), d.s, string(r.LeaseID),
		v1.RejectLeaseRequest{Epoch: r.LeaseEpoch, Code: domain.CodeSecretUnavailable, Message: "not in the catalog"})
	must(w.t, err)
}

// endRun has the Worker of an accepted lease say the Run stopped before it started.
func (w *secretsWorld) endRun(r store.Run) {
	d := w.daemonOf(r.WorkerID)
	d.reportSeq += 2
	out, err := w.e.app.Runs.Report(w.t.Context(), d.s, string(r.ID), v1.RunReportRequest{Entries: []v1.RunReportEntry{
		{WorkerEpoch: d.epoch, WorkerSeq: d.reportSeq - 1, LeaseEpoch: r.LeaseEpoch, Kind: v1.ReportPrepareFailed, Message: "ended", At: w.e.clock.Now()},
		{WorkerEpoch: d.epoch, WorkerSeq: d.reportSeq, LeaseEpoch: r.LeaseEpoch, Kind: v1.ReportStopped, NeverStarted: true, At: w.e.clock.Now()},
	}})
	w.trace.add("end %s e%d: %v %v", r.ID, r.LeaseEpoch, results(out), err)
}

func (w *secretsWorld) pick(runs []store.Run, ok func(store.Run) bool) (store.Run, bool) {
	var in []store.Run
	for _, r := range runs {
		if ok(r) {
			in = append(in, r)
		}
	}
	if len(in) == 0 {
		return store.Run{}, false
	}
	return in[w.rng.IntN(len(in))], true
}

// grantRequest is what the Worker asks for a Run it holds: what it declares,
// at times less, or a ref it does not declare, or under an epoch of a daemon
// that is gone; and at times another Worker asks.
func (w *secretsWorld) grantRequest(r store.Run) grantCall {
	caller := w.daemonOf(r.WorkerID)
	if w.rng.IntN(10) == 0 {
		caller = w.daemons[w.rng.IntN(len(w.daemons))]
	}
	refs := slices.Clone(w.tasks[r.TaskID].refs)
	if len(refs) > 1 && w.rng.IntN(5) == 0 {
		refs = refs[:1]
	}
	if len(refs) == 0 || w.rng.IntN(6) == 0 {
		extra := secretRefs[w.rng.IntN(len(secretRefs))]
		if !slices.Contains(refs, extra) {
			refs = append(refs, extra)
		}
	}
	epoch := caller.epoch
	if len(caller.old) > 0 && w.rng.IntN(8) == 0 {
		epoch = caller.old[w.rng.IntN(len(caller.old))]
	}
	return grantCall{caller: caller, run: r.ID, req: v1.GrantSecretsRequest{
		LeaseID: string(r.LeaseID), LeaseEpoch: r.LeaseEpoch, WorkerEpoch: epoch, Refs: refs,
	}}
}

// grantExpectation is what the model says of a grant: the reasons each ref
// may be denied for; a grant is given when no ref has any.
type grantExpectation struct {
	code    domain.Code
	reasons map[string][]string
}

func (g grantExpectation) granted() bool {
	for _, r := range g.reasons {
		if len(r) > 0 {
			return false
		}
	}
	return true
}

// expect is the decision on c in the state of this instant: the lease and the
// Run as the store has them, and the rest as the model has it, with allow the
// allowlist of the Project of the Run.
func (w *secretsWorld) expect(c grantCall, allow map[string]bool) grantExpectation {
	w.t.Helper()
	var (
		r store.Run
		l store.Lease
	)
	must(w.t, w.e.db.Read(w.t.Context(), func(q store.Querier) (err error) {
		if r, err = store.RunByID(w.t.Context(), q, c.run); err != nil {
			return err
		}
		l, err = store.LeaseByRunEpoch(w.t.Context(), q, c.run, c.req.LeaseEpoch)
		return err
	}))
	var auth []string
	if l.WorkerID != c.caller.s.Principal.ID {
		auth = append(auth, v1.GrantNotLeaseHolder)
	}
	if l.State != domain.LeaseAccepted || r.LeaseEpoch != l.Epoch {
		auth = append(auth, v1.GrantStaleLease)
	}
	if r.State.IsTerminal() {
		auth = append(auth, v1.GrantRunNotPreparable)
	}
	if fence := w.model.fence[c.caller.s.Principal.ID]; c.req.WorkerEpoch != fence.epoch {
		auth = append(auth, v1.GrantDaemonRetired)
	}
	out := grantExpectation{reasons: map[string][]string{}}
	if len(auth) > 0 {
		out.code = domain.CodeStaleLease
		for _, ref := range c.req.Refs {
			out.reasons[ref] = auth
		}
		return out
	}
	task := w.tasks[r.TaskID]
	fence := w.model.fence[c.caller.s.Principal.ID]
	out.code = domain.CodePolicyDenied
	for _, ref := range c.req.Refs {
		var reasons []string
		scope, held := fence.refs[ref]
		if !slices.Contains(task.refs, ref) {
			reasons = append(reasons, v1.SecretNotDeclared)
		}
		if !allow[ref] {
			reasons = append(reasons, v1.SecretNotAllowlisted)
		}
		if !held {
			reasons = append(reasons, v1.SecretNotAnnounced)
		}
		if held && len(scope) > 0 && !slices.Contains(scope, task.project) {
			reasons = append(reasons, v1.SecretOutOfScope)
		}
		out.reasons[ref] = reasons
	}
	return out
}

// matches says whether what the Controller answered is what the model expects.
func (g grantExpectation) matches(out v1.GrantSecretsResponse, err error) string {
	if g.granted() {
		if err != nil {
			return fmt.Sprintf("denied (%v); the model grants it", err)
		}
		return ""
	}
	if err == nil {
		return fmt.Sprintf("granted %v; the model denies it: %v", out.Granted, g.reasons)
	}
	var coded *domain.Error
	if !errors.As(err, &coded) || coded.Code != g.code {
		return fmt.Sprintf("denied with %v; the model says %s %v", err, g.code, g.reasons)
	}
	denied, _ := coded.Details["denied"].([]v1.SecretDenial)
	got := map[string]string{}
	for _, d := range denied {
		got[d.Ref] = d.Reason
	}
	for ref, reasons := range g.reasons {
		reason, ok := got[ref]
		switch {
		case len(reasons) == 0 && ok:
			return fmt.Sprintf("%q denied (%s); the model passes it", ref, reason)
		case len(reasons) > 0 && !slices.Contains(reasons, reason):
			return fmt.Sprintf("%q denied with %q; the model says one of %v", ref, reason, reasons)
		}
	}
	return ""
}

// grant sends c, now or alongside a change of the allowlist, and checks S2
// against the model at the point the Controller decides: the answer, one
// audit entry per ref, the grant kept. If the answer reaches the daemon, it
// reads the values next.
func (w *secretsWorld) grant(c grantCall, lost bool) {
	w.t.Helper()
	project := w.tasks[w.taskOf(c.run)].project
	var (
		out  v1.GrantSecretsResponse
		err  error
		exps []grantExpectation
	)
	call := func() { out, err = w.e.app.Runs.GrantSecrets(w.t.Context(), c.caller.s, string(c.run), c.req) }
	if ch, ok := w.allowlistAround(c, project); ok {
		w.trace.add("grant %s e%d %v by %s/%s while %+v", c.run, c.req.LeaseEpoch, c.req.Refs, c.caller.name, c.req.WorkerEpoch, ch)
		// Either order is a linearization: the expectation of each, before the call.
		before := maps.Clone(w.model.allow[project])
		after := maps.Clone(before)
		after[ch.ref] = ch.allow
		exps = []grantExpectation{w.expect(c, before), w.expect(c, after)}
		w.concurrently(ch, call)
	} else {
		w.trace.add("grant %s e%d %v by %s/%s lost=%v", c.run, c.req.LeaseEpoch, c.req.Refs, c.caller.name, c.req.WorkerEpoch, lost)
		exps = []grantExpectation{w.expect(c, w.model.allow[project])}
		auditsBefore := w.countAudits()
		call()
		if n := w.countAudits() - auditsBefore; n != len(c.req.Refs) {
			w.t.Fatalf("S2: a grant of %d refs left %d audit entries", len(c.req.Refs), n)
		}
	}
	w.audited += len(c.req.Refs)
	var why []string
	for _, exp := range exps {
		if m := exp.matches(out, err); m != "" {
			why = append(why, m)
		}
	}
	if len(why) == len(exps) {
		w.t.Fatalf("S2: grant of %v for %s by %s: %s", c.req.Refs, c.run, c.caller.name, strings.Join(why, " | "))
	}
	w.outcomes[outcomeOf(err)]++
	if err != nil {
		return
	}
	w.granted += len(c.req.Refs)
	if !lost {
		w.toRead = append(w.toRead, pendingRead{call: c, epoch: c.caller.epoch})
	}
}

// allowlistAround changes the allowlist of project around the grant c: at
// times one of the refs c asks for is denied just before it, and at times a
// ref, mostly one c asks for, is allowed or denied while it is decided, which
// it returns. The lease of c was made with every ref allowlisted: only a change
// of those refs between the lease and the grant, or during it, tells a grant
// that checks the allowlist from one that does not.
func (w *secretsWorld) allowlistAround(c grantCall, project string) (allowlistChange, bool) {
	asked := allowlistChange{project: project, ref: c.req.Refs[w.rng.IntN(len(c.req.Refs))], allow: w.rng.IntN(2) == 0}
	switch w.rng.IntN(6) {
	case 0:
		asked.allow = false
		w.trace.add("allowlist %+v before the grant", asked)
		must(w.t, w.apply(asked))
		w.model.allow[project][asked.ref] = false
	case 1:
		return asked, true
	case 2:
		ch := w.randomChange()
		ch.project = project
		return ch, true
	}
	return allowlistChange{}, false
}

// outcomeOf is a grant answer as "granted" or the first reason it was denied for.
func outcomeOf(err error) string {
	var coded *domain.Error
	if !errors.As(err, &coded) {
		return "granted"
	}
	if denied, _ := coded.Details["denied"].([]v1.SecretDenial); len(denied) > 0 {
		return denied[0].Reason
	}
	return string(coded.Code)
}

func (w *secretsWorld) taskOf(run domain.ID) domain.ID {
	return w.e.storedRun(string(run)).TaskID
}

func (w *secretsWorld) countAudits() int {
	var n int
	must(w.t, w.e.db.Reader().QueryRowContext(w.t.Context(),
		`SELECT COUNT(*) FROM audit_entries WHERE action = ?`, store.AuditSecretGrant).Scan(&n))
	return n
}

// read is the daemon reading the values a grant gave it, unless it restarted
// since (the attempt died with it) or its catalog no longer lets it (the
// prepare fails): S4, every read is under a grant the Controller kept as given
// to that daemon, for that lease and that ref.
func (w *secretsWorld) read(p pendingRead) {
	w.t.Helper()
	d := p.call.caller
	project := w.tasks[w.taskOf(p.call.run)].project
	switch {
	case d.epoch != p.epoch:
		w.trace.add("read %s: daemon restarted", p.call.run)
		return
	case !d.holds(project, p.call.req.Refs):
		w.trace.add("read %s: catalog changed", p.call.run)
		return
	}
	w.trace.add("read %s %v", p.call.run, p.call.req.Refs)
	grants := w.e.secretGrants(string(p.call.run))
	for _, ref := range p.call.req.Refs {
		found := false
		for _, g := range grants {
			found = found || g.Granted() && g.LeaseEpoch == p.call.req.LeaseEpoch && g.WorkerID == d.s.Principal.ID &&
				g.WorkerEpoch == p.call.req.WorkerEpoch && slices.ContainsFunc(g.Refs, func(v store.SecretVerdict) bool { return v.Ref == ref })
		}
		if !found {
			w.t.Fatalf("S4: %s read %q for %s e%d with no grant given", d.name, ref, p.call.run, p.call.req.LeaseEpoch)
		}
		w.reads = append(w.reads, valueRead{run: p.call.run, leaseEpoch: p.call.req.LeaseEpoch, workerEpoch: p.call.req.WorkerEpoch, ref: ref})
	}
}

// checkView is the metadata of every Worker as the model says the Controller
// keeps it, and its daemon epoch.
func (w *secretsWorld) checkView(what string) {
	w.t.Helper()
	for _, d := range w.daemons {
		var metas []v1.SecretMeta
		must(w.t, w.e.db.Read(w.t.Context(), func(q store.Querier) (err error) {
			metas, err = store.SecretsOfWorker(w.t.Context(), q, d.s.Principal.ID)
			return err
		}))
		got := map[string][]string{}
		for _, m := range metas {
			got[m.Ref] = slices.Sorted(slices.Values(m.Projects))
		}
		fence := w.model.fence[d.s.Principal.ID]
		if !maps.EqualFunc(got, fence.refs, slices.Equal) {
			w.t.Fatalf("%s: the Controller holds %v of %s; the model %v", what, got, d.name, fence.refs)
		}
		if epoch := w.e.storedWorker(d.s).Report.Epoch; epoch != fence.epoch {
			w.t.Fatalf("%s: the Controller holds %s at epoch %s; the model %s", what, d.name, epoch, fence.epoch)
		}
	}
}

// step does one thing chosen by the seed.
func (w *secretsWorld) step() {
	w.t.Helper()
	ctx := w.t.Context()
	d := w.daemons[w.rng.IntN(len(w.daemons))]
	runs := func() []store.Run { return w.runs() }
	action := w.rng.IntN(22)
	switch action {
	case 0:
		ref := secretRefs[w.rng.IntN(len(secretRefs))]
		w.setSecret(d, ref)
		w.trace.add("catalog %s set %s %v", d.name, ref, d.catalog[ref].projects)
	case 1:
		if w.rng.IntN(2) == 0 {
			return
		}
		ref := secretRefs[w.rng.IntN(len(secretRefs))]
		delete(d.catalog, ref)
		w.trace.add("catalog %s rm %s", d.name, ref)
	case 2, 3:
		w.trace.add("beat %s g%d seq %d", d.name, d.gen, d.seq+1)
		_ = w.beat(d, d.heartbeat())
	case 4:
		d.captured = append(d.captured, d.heartbeat())
		w.trace.add("capture %s g%d seq %d", d.name, d.gen, d.seq)
	case 5:
		if len(d.captured) == 0 {
			return
		}
		i := w.rng.IntN(len(d.captured))
		req := d.captured[i]
		if w.rng.IntN(3) != 0 { // otherwise it arrives again later
			d.captured = slices.Delete(d.captured, i, i+1)
		}
		w.trace.add("deliver %s g%d %s seq %d", d.name, req.Generation, req.Epoch, req.Secrets.Seq)
		_ = w.beat(d, req)
	case 6:
		if len(d.captured) > 0 {
			d.captured = d.captured[1:]
			w.trace.add("drop %s", d.name)
		}
	case 7:
		lost := w.rng.IntN(3) == 0
		w.trace.add("restart %s lost=%v", d.name, lost)
		w.restart(d, lost)
	case 8:
		c := w.randomChange()
		w.trace.add("allowlist %+v", c)
		must(w.t, w.apply(c))
		w.model.allow[c.project][c.ref] = c.allow
	case 9:
		if len(w.tasks) < 10 {
			w.createTask()
		}
	case 10, 11:
		w.pass()
		// A daemon answers an offer within seconds; one left unanswered expires.
		for _, r := range runs() {
			if r.LeaseState == domain.LeasePending && !r.State.IsTerminal() && w.rng.IntN(2) == 0 {
				w.answerLease(r)
			}
		}
	case 12, 20:
		if r, ok := w.pick(runs(), func(r store.Run) bool { return r.LeaseState == domain.LeasePending }); ok {
			w.answerLease(r)
		}
	case 13:
		w.e.clock.Advance(domain.Defaults().LeaseAcceptBy + time.Second)
		w.trace.add("expire")
		// A daemon beats every few seconds: most stay online across the wait.
		for _, d := range w.daemons {
			if w.rng.IntN(4) != 0 {
				w.trace.add("beat %s g%d seq %d", d.name, d.gen, d.seq+1)
				_ = w.beat(d, d.heartbeat())
			}
		}
		_, err := w.e.app.Leases.SweepExpired(ctx)
		must(w.t, err)
	case 14, 15, 21:
		anyEnd := w.rng.IntN(8) == 0 // mostly a Run being prepared; at times one that ended
		if r, ok := w.pick(runs(), func(r store.Run) bool {
			return r.LeaseID != "" && r.LeaseState == domain.LeaseAccepted && (anyEnd || !r.State.IsTerminal())
		}); ok {
			c := w.grantRequest(r)
			switch w.rng.IntN(4) {
			case 0:
				w.late = append(w.late, c) // sent now, it arrives later
				w.trace.add("grant %s e%d sent late", c.run, c.req.LeaseEpoch)
			case 1:
				w.grant(c, true)
			default:
				w.grant(c, false)
			}
		}
	case 16:
		if len(w.late) > 0 {
			i := w.rng.IntN(len(w.late))
			c := w.late[i]
			w.late = slices.Delete(w.late, i, i+1)
			w.grant(c, false)
		}
	case 17:
		if len(w.toRead) > 0 {
			i := w.rng.IntN(len(w.toRead))
			p := w.toRead[i]
			w.toRead = slices.Delete(w.toRead, i, i+1)
			w.read(p)
		}
	case 18:
		if r, ok := w.pick(runs(), func(r store.Run) bool { return r.LeaseState == domain.LeaseAccepted && !r.State.IsTerminal() }); ok && w.rng.IntN(2) == 0 {
			w.endRun(r)
		}
	case 19:
		if r, ok := w.pick(runs(), func(r store.Run) bool { return !r.State.IsTerminal() }); ok && w.rng.IntN(3) == 0 {
			w.trace.add("cancel %s", r.TaskID)
			_, _ = w.e.app.Tasks.Cancel(ctx, w.e.owner, string(r.TaskID), "", v1.ActionRequest{})
		}
	}
	switch action {
	case 2, 3, 5, 7, 13: // what a heartbeat moves
		w.checkView(fmt.Sprintf("after action %d", len(*w.trace)))
	}
}

// drain delivers every heartbeat still on its way, in any order and some
// twice, and a fresh one from every daemon: L1, the Controller then holds
// every catalog as it is. Then the scheduler runs with every Worker answering
// what it is offered: L2, every Run some Worker can take, by its catalog and
// the allowlist, gets a lease.
func (w *secretsWorld) drain() {
	w.t.Helper()
	w.trace.add("drain")
	for _, d := range w.daemons {
		w.rng.Shuffle(len(d.captured), func(i, j int) { d.captured[i], d.captured[j] = d.captured[j], d.captured[i] })
		for _, req := range d.captured {
			_ = w.beat(d, req)
			if w.rng.IntN(4) == 0 {
				_ = w.beat(d, req)
			}
		}
		d.captured = nil
	}
	for _, d := range w.daemons {
		must(w.t, w.beat(d, d.heartbeat()))
	}
	w.checkView("drain")
	for _, d := range w.daemons {
		fence := w.model.fence[d.s.Principal.ID]
		for ref, c := range d.catalog {
			if scope, ok := fence.refs[ref]; !ok || !slices.Equal(scope, slices.Sorted(slices.Values(c.projects))) {
				w.t.Fatalf("L1: %s holds %q %v; the Controller %v", d.name, ref, c.projects, fence.refs)
			}
		}
		if len(fence.refs) != len(d.catalog) {
			w.t.Fatalf("L1: %s holds %d secrets; the Controller %d", d.name, len(d.catalog), len(fence.refs))
		}
	}
	satisfiable := func(r store.Run) bool {
		task := w.tasks[r.TaskID]
		for _, ref := range task.refs {
			if !w.model.allow[task.project][ref] {
				return false
			}
		}
		return slices.ContainsFunc(w.daemons, func(d *simDaemon) bool { return d.holds(task.project, task.refs) })
	}
	waiting := func() (store.Run, bool) {
		return w.pick(w.runs(), func(r store.Run) bool { return r.State == domain.RunQueued && r.LeaseID == "" && satisfiable(r) })
	}
	for range 12 {
		w.e.clock.Advance(domain.LeaseMissBackoff + time.Second)
		for _, d := range w.daemons {
			must(w.t, w.beat(d, d.heartbeat()))
		}
		w.pass()
		if _, ok := waiting(); !ok {
			return
		}
		// Room for the next pass: every offer answered, every Run taken ended.
		for _, r := range w.runs() {
			if r.LeaseState == domain.LeasePending && !r.State.IsTerminal() {
				w.answerLease(r)
			}
		}
		for _, r := range w.runs() {
			if r.LeaseState == domain.LeaseAccepted && !r.State.IsTerminal() {
				w.endRun(r)
			}
		}
	}
	if r, ok := waiting(); ok {
		explain, err := w.e.app.Scheduler.Explain(w.t.Context(), string(r.ID))
		must(w.t, err)
		w.t.Fatalf("L2: %s waits (%s) though a Worker holds %v for it: %s", r.ID, r.QueuedReason, w.tasks[r.TaskID].refs, jsonOf(w.t, explain))
	}
}

// noValueAnywhere is S3: no table holds a value of any catalog, as it is,
// in base64 or in hex.
func (w *secretsWorld) noValueAnywhere() {
	w.t.Helper()
	tables := func() []string {
		rows, err := w.e.db.Reader().QueryContext(w.t.Context(), `SELECT name FROM sqlite_master WHERE type = 'table'`)
		must(w.t, err)
		defer func() { _ = rows.Close() }()
		var out []string
		for rows.Next() {
			var name string
			must(w.t, rows.Scan(&name))
			out = append(out, name)
		}
		must(w.t, rows.Err())
		return out
	}()
	var all strings.Builder
	for _, table := range tables {
		all.WriteString(w.e.dump(`"` + table + `"`))
	}
	text := all.String()
	for _, d := range w.daemons {
		for ref, c := range d.catalog {
			for _, form := range []string{c.value, base64.StdEncoding.EncodeToString([]byte(c.value)), hex.EncodeToString([]byte(c.value))} {
				if strings.Contains(text, form) {
					w.t.Fatalf("S3: the value of %q of %s is in the store", ref, d.name)
				}
			}
		}
	}
}

// TestProperty_SecretsController: S1 (the scheduler), S2 (the grant), S3 (no
// value in the store), S4 (no read without its grant), the fence of the
// heartbeat step by step, and L1 and L2 once the seed drains.
func TestProperty_SecretsController(t *testing.T) {
	eachSecretSeed(t, 3, "./internal/app/", func(t *testing.T, rng *rand.Rand, trace *actionTrace) {
		w := newSecretsWorld(t, rng, trace)
		for range 300 {
			w.step()
		}
		w.drain()
		w.noValueAnywhere()
		if len(w.reads) > w.granted {
			t.Fatalf("W5: %d values read under %d refs granted", len(w.reads), w.granted)
		}
		if n := w.countAudits(); n != w.audited {
			t.Fatalf("S2: %d secret.grant audit entries for %d refs decided", n, w.audited)
		}
		t.Logf("%d refs decided, %d granted, %d values read; answers %v", w.audited, w.granted, len(w.reads), w.outcomes)
	})
}
