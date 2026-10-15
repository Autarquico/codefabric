//go:build e2e && (linux || darwin)

package e2e

import (
	"bufio"
	"cmp"
	crand "crypto/rand"
	"database/sql"
	"fmt"
	"maps"
	"math/rand/v2"
	"os"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	v1 "github.com/Autarquico/codefabric/internal/api/v1"
	"github.com/Autarquico/codefabric/internal/client"
	"github.com/Autarquico/codefabric/internal/domain"
	"github.com/Autarquico/codefabric/internal/e2e/netproxy"
)

// The property: two proxied Workers run a handful of Tasks while the test
// crashes, partitions and loses replies under them, and every invariant holds
// once the cluster settles. A template forces four transitions on every seed,
// and the test proves each one happened; the seed chooses their order, the
// times, the Workers and what happens in between. A seed repeats the program
// of actions, not the interleaving of the OS: the trace says what was chosen.

const (
	// envSeed runs exactly that seed; envSeeds adds that many random seeds to
	// the corpus; envRepeat runs each seed that many times.
	envSeed   = "CF_E2E_SEED"
	envSeeds  = "CF_E2E_SEEDS"
	envRepeat = "CF_E2E_REPEAT"
	// seedsFile is the corpus: one seed per line, as envSeed takes it, and # comments.
	seedsFile = "testdata/seeds.txt"
)

const (
	// firstTasks are created before anything happens; the seed may add one.
	firstTasks = 3
	// maxTasks bounds the Tasks of a seed: each attempt at losing a reply ends a harness.
	maxTasks = 16
	// retryAttempts lets every Task outlive the Runs a partition loses.
	retryAttempts = "4"
)

// transition is one of the transitions every seed has to go through.
type transition string

const (
	// daemonRestart: kill -9 of a daemon whose journal has issued reports, its
	// restart, and a report of its new epoch.
	daemonRestart transition = "daemon-restart"
	// longPartition: a Worker cut off past run_lost_timeout with its harness
	// alive, the retry running on the other Worker, and the cut-off harness
	// stopped once the partition heals.
	longPartition transition = "long-partition"
	// lostReply: a report the Controller applied whose reply was dropped, sent
	// again once the connection is back.
	lostReply transition = "lost-reply"
	// controllerKill: kill -9 of the Controller with harnesses alive.
	controllerKill transition = "controller-kill"
)

var template = []transition{daemonRestart, longPartition, lostReply, controllerKill}

// TestProperty_E2eV01_Intercalados runs the seeds of the corpus (or the one
// CF_E2E_SEED names), plus CF_E2E_SEEDS random ones, CF_E2E_REPEAT times each.
func TestProperty_E2eV01_Intercalados(t *testing.T) {
	seeds := propertySeeds(t)
	repeat := envCount(t, envRepeat, 1)
	for _, seed := range seeds {
		for range repeat {
			t.Run(fmt.Sprintf("seed=%#x", seed), func(t *testing.T) { runSeed(t, seed) })
		}
	}
}

func propertySeeds(t *testing.T) []uint64 {
	t.Helper()
	if text := os.Getenv(envSeed); text != "" {
		seed, err := strconv.ParseUint(text, 0, 64)
		if err != nil {
			t.Fatalf("e2e: %s=%q is not a seed", envSeed, text)
		}
		return []uint64{seed}
	}
	seeds := corpus(t)
	for range envCount(t, envSeeds, 0) {
		var key [32]byte
		_, _ = crand.Read(key[:])
		seeds = append(seeds, rand.NewChaCha8(key).Uint64())
	}
	return seeds
}

// corpus are the seeds of seedsFile, in order.
func corpus(t *testing.T) []uint64 {
	t.Helper()
	f, err := os.Open(seedsFile)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	var seeds []uint64
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line, _, _ := strings.Cut(sc.Text(), "#")
		fields := strings.Fields(line)
		if len(fields) == 0 {
			continue
		}
		seed, err := strconv.ParseUint(fields[0], 0, 64)
		if err != nil {
			t.Fatalf("e2e: %s: %q is not a seed", seedsFile, fields[0])
		}
		seeds = append(seeds, seed)
	}
	if err := sc.Err(); err != nil {
		t.Fatal(err)
	}
	return seeds
}

func envCount(t *testing.T, name string, def int) int {
	t.Helper()
	text := os.Getenv(name)
	if text == "" {
		return def
	}
	n, err := strconv.Atoi(text)
	if err != nil || n < 0 {
		t.Fatalf("e2e: %s=%q is not a count", name, text)
	}
	return n
}

// reproLine is the command that runs seed again, as this invocation ran it.
func reproLine(seed uint64) string {
	env := fmt.Sprintf("%s=%#x CF_REQUIRE_TMUX=1 CF_REQUIRE_GIT=1", envSeed, seed)
	if s := os.Getenv(envScale); s != "" {
		env += " " + envScale + "=" + s
	}
	flags := "-tags e2e -count=1"
	if race {
		env += " " + envRace + "=1"
		flags += " -race"
	}
	return fmt.Sprintf("%s go test %s -run '^TestProperty_E2eV01_Intercalados$/^seed=%#x$' ./internal/e2e/", env, flags, seed)
}

func runSeed(t *testing.T, seed uint64) {
	repro := reproLine(seed)
	t.Logf("e2e: seed %#x; to run it again: %s", seed, repro)
	rng := rand.New(rand.NewPCG(seed, seed)) //nolint:gosec // a seed to repeat, not a secret
	w := &trial{t: t, rng: rng, began: time.Now(), victims: map[string]bool{}, released: map[Execution]bool{}}
	t.Cleanup(func() {
		if t.Failed() {
			t.Logf("e2e: seed %#x failed; to run it again:\n  %s\ntrace:\n%s", seed, repro, w.render(true))
		}
	})
	w.c = NewCluster(t, Opts{Workers: 2, Proxied: true, MaxConcurrent: 2})
	w.note("cluster up: Workers %s (%s) and %s (%s)", w.c.Workers[0].Name, w.c.Workers[0].ID, w.c.Workers[1].Name, w.c.Workers[1].ID)
	sampler := w.c.StartSampler()

	for range firstTasks + w.rng.IntN(2) {
		w.createTask()
		w.pause(500 * time.Millisecond)
	}
	order := slices.Clone(template)
	w.rng.Shuffle(len(order), func(i, j int) { order[i], order[j] = order[j], order[i] })
	w.note("order: %v", order)
	for _, tr := range order {
		w.wander()
		w.force(tr)
	}
	w.quiesce()

	Check(t, w.c, Expect{
		Tasks: w.tasks, DaemonKills: w.kills, Sampler: sampler,
		Seed: fmt.Sprintf("%#x; to run it again: %s; actions: %s", seed, repro, w.render(false)),
	})
	w.checkModel()
	w.checkCoverage()
	t.Logf("e2e: seed %#x took %v", seed, time.Since(w.began).Round(time.Millisecond))
}

// trial is the cluster of a seed and the small model of it the test keeps:
// the Tasks it created, the Runs a partition may lose, and the evidence of
// each transition of the template.
type trial struct {
	t     *testing.T
	c     *Cluster
	rng   *rand.Rand
	began time.Time
	trace []step

	tasks []ExpectedTask
	kills []DaemonKill
	// victims are the Runs a partition caught on its Worker: only they may be lost.
	victims map[string]bool
	// released are the harnesses let go: none is picked again, while it ends.
	released map[Execution]bool

	restart   *DaemonKill
	partition *partitionSeen
	reply     *replySeen
	ctlKill   *ctlKillSeen
}

type step struct {
	at   time.Duration
	what string
}

type partitionSeen struct {
	other          *Worker
	origin         Instance // the harness cut off with its Worker
	retry          string   // the Run that retries the origin's
	aliveWithRetry bool     // the origin was alive while its retry ran on the other Worker
}

type replySeen struct {
	worker      string
	seq         int64 // the report the Controller applied while its replies were dropped
	dropped     int64 // reply bytes the proxy dropped
	unconfirmed int   // journal rows the Worker held then, waiting for an answer
	resent      bool  // it left the journal once the Worker was healed: sent again and answered
}

type ctlKillSeen struct {
	alive []Execution
}

// note adds an action to the trace, and logs it.
func (w *trial) note(format string, args ...any) {
	s := step{at: time.Since(w.began), what: fmt.Sprintf(format, args...)}
	w.trace = append(w.trace, s)
	w.t.Logf("e2e: +%7.3fs %s", s.at.Seconds(), s.what)
}

func (w *trial) render(times bool) string {
	var lines []string
	for _, s := range w.trace {
		if times {
			lines = append(lines, fmt.Sprintf("  +%7.3fs %s", s.at.Seconds(), s.what))
		} else {
			lines = append(lines, s.what)
		}
	}
	if times {
		return strings.Join(lines, "\n")
	}
	return strings.Join(lines, " | ")
}

// between is a duration in [lo, hi] the seed chooses, scaled.
func (w *trial) between(lo, hi time.Duration) time.Duration {
	return Scaled(lo + time.Duration(w.rng.Int64N(int64(hi-lo)+1)))
}

func (w *trial) pause(upTo time.Duration) {
	d := w.between(0, upTo)
	w.note("pause %v", d.Round(time.Millisecond))
	time.Sleep(d)
}

// createTask creates a Task whose harness is a ledger script, with a policy
// that retries a lost Run.
func (w *trial) createTask() {
	token := w.c.Ledger.Token()
	created := w.c.Do(w.c.Ledger.Script(token))
	w.c.TaskSet(created.Task.ID, "--retry-on-lost", string(domain.RetryReschedule), "--retry-max-attempts", retryAttempts)
	w.tasks = append(w.tasks, ExpectedTask{ID: created.Task.ID, Token: token})
	w.note("create Task %s (%s), Run %s", created.Task.ID, token, created.Run.ID)
}

// runs are the Runs of the Project, by ID.
func (w *trial) runs() (map[string]v1.Run, error) {
	list, err := w.c.API.Runs(w.c.H.Context(), client.RunQuery{Project: w.c.Project.ID})
	if err != nil {
		return nil, err
	}
	out := map[string]v1.Run{}
	for _, r := range list {
		out[r.ID] = r
	}
	return out, nil
}

// release lets the harness of e end.
func (w *trial) release(e Execution) {
	w.released[e] = true
	w.c.Ledger.ReleaseExecution(e)
}

// alive are the harnesses alive, not released, of Runs that are running, in a
// stable order.
func (w *trial) alive(runs map[string]v1.Run) []Instance {
	var out []Instance
	for _, in := range w.c.Ledger.All() {
		if in.Worker != "" && !w.released[in.Execution] && runs[in.Run].State == domain.RunRunning && w.c.Ledger.Live(in) {
			out = append(out, in)
		}
	}
	slices.SortFunc(out, func(a, b Instance) int {
		return cmp.Or(cmp.Compare(a.Worker, b.Worker), cmp.Compare(a.Run, b.Run))
	})
	return out
}

// coming says whether a Run is on its way to a harness: a Task waits for a
// Worker, or a Run was given one and its harness has not started.
func coming(runs map[string]v1.Run) bool {
	for _, r := range runs {
		if r.State == domain.RunQueued || r.State == domain.RunPreparing {
			return true
		}
	}
	return false
}

// pickLive waits for a harness alive of a running Run, creating a Task when
// none is on its way, and returns the one the seed chooses.
func (w *trial) pickLive(what string) Instance {
	w.t.Helper()
	var picked []Instance
	w.c.Eventually("a harness alive for "+what, func() (bool, string) {
		runs, err := w.runs()
		if err != nil {
			return false, err.Error()
		}
		if picked = w.alive(runs); len(picked) > 0 {
			return true, ""
		}
		if !coming(runs) && len(w.tasks) < maxTasks {
			w.createTask()
		}
		return false, fmt.Sprintf("%d Runs, %d Tasks created", len(runs), len(w.tasks))
	})
	return picked[w.rng.IntN(len(picked))]
}

// wander is what happens between two transitions, as the seed chooses: a new
// Task, a harness released, or nothing; then a pause.
func (w *trial) wander() {
	switch w.rng.IntN(4) {
	case 0:
		if len(w.tasks) < maxTasks {
			w.createTask()
		}
	case 1:
		runs, err := w.runs()
		if err != nil {
			w.t.Fatal(err)
		}
		if live := w.alive(runs); len(live) > 0 {
			in := live[w.rng.IntN(len(live))]
			w.release(in.Execution)
			w.note("release the harness of %v", in.Execution)
		}
	}
	w.pause(1500 * time.Millisecond)
}

func (w *trial) force(tr transition) {
	switch tr {
	case daemonRestart:
		w.restartDaemon()
	case longPartition:
		w.partitionLong()
	case lostReply:
		w.loseReply()
	case controllerKill:
		w.killController()
	}
}

// restartDaemon kills -9 the daemon of a Worker whose harness runs, and starts
// it again within 0-4 s: the Worker may go offline, never near run_lost_timeout.
func (w *trial) restartDaemon() {
	in := w.pickLive("a daemon restart")
	wk := workerNamed(w.c, in.Worker)
	down := w.between(0, 4*time.Second)
	kill := w.c.KillDaemon(wk)
	w.kills = append(w.kills, kill)
	w.restart = &kill
	w.note("kill -9 the daemon of %s with %v alive (journal high-water %d, epochs %v); restart it in %v",
		wk.Name, in.Execution, kill.HighWater, kill.Epochs, down.Round(time.Millisecond))
	time.Sleep(down)
	w.c.RestartWorker(wk)
	w.note("the daemon of %s is back", wk.Name)
}

// partitionLong cuts a Worker off with a harness alive until its Run is lost
// and retried on the other Worker, then heals it and waits for the cut-off
// harness to be stopped.
func (w *trial) partitionLong() {
	origin := w.pickLive("a long partition")
	wk := workerNamed(w.c, origin.Worker)
	other := otherWorker(w.c, wk)
	runs, err := w.runs()
	if err != nil {
		w.t.Fatal(err)
	}
	for _, r := range runs {
		if r.WorkerID == wk.ID && !settled(r.State) {
			w.victims[r.ID] = true
		}
	}
	p := &partitionSeen{other: other, origin: origin}
	w.partition = p
	wk.Proxy.SetMode(netproxy.Reject)
	w.note("partition %s (reject) with %v alive; Runs caught %v", wk.Name, origin.Execution, slices.Sorted(maps.Keys(w.victims)))

	w.c.Eventually("Run "+origin.Run+" to be lost and retried on "+other.Name, func() (bool, string) {
		runs, err := w.runs()
		if err != nil {
			return false, err.Error()
		}
		if runs[origin.Run].State != domain.RunLost {
			return false, RunStage(runs[origin.Run])
		}
		for _, in := range w.alive(runs) {
			r := runs[in.Run]
			if r.LineageParent == origin.Run && in.Worker == other.Name {
				p.retry = r.ID
				return true, ""
			}
			// A harness that is no retry of a caught Run holds a place the retry needs.
			if in.Worker == other.Name && !w.victims[r.LineageParent] {
				w.release(in.Execution)
				w.note("release the harness of %v to make room for the retry", in.Execution)
			}
		}
		return false, "lost; the retry has no harness on " + other.Name + " yet"
	})
	p.aliveWithRetry = w.c.Ledger.Live(origin)
	w.note("Run %s is lost and its retry %s runs on %s; the cut-off harness alive: %t", origin.Run, p.retry, other.Name, p.aliveWithRetry)

	w.pause(time.Second)
	wk.Proxy.Heal()
	w.note("heal %s", wk.Name)
	w.c.Eventually("the cut-off harness "+origin.String()+" to be stopped", func() (bool, string) {
		return !w.c.Ledger.Live(origin), "alive"
	})
	w.note("the cut-off harness %v is stopped", origin.Execution)
	w.c.WaitOnline(wk)
}

// loseReply makes the Controller apply a report of a Worker whose answer the
// Worker never gets, then heals the Worker and waits for the report to be
// confirmed: sent again, and taken as the duplicate it is.
//
// Under DropReplies a new connection never finishes its TLS handshake: only a
// connection the Worker already holds carries a request. Besides the long poll
// of assignments, the Worker needs two idle: one its heartbeat takes, and one
// for the report. It has them after a burst of requests, which is what a
// failed attempt leaves behind: its requests pile up and reconnect together
// once the Worker is healed. So an attempt whose report got nowhere is made
// again on the same Worker, with another harness, and waits for one connection
// more than the failed attempt had: the Worker may hold others, busy.
func (w *trial) loseReply() {
	in, want := w.pickLive("a lost reply"), replyConns
	for attempt := 1; attempt <= replyAttempts; attempt++ {
		ok, held := w.tryLoseReply(attempt, in, want)
		if ok {
			return
		}
		// A connection that held no spare one is busy with something else.
		want = min(max(want, int(held)+1), maxReplyConns)
		in = w.pickLiveOn(workerNamed(w.c, in.Worker))
	}
}

// pickLiveOn is pickLive on wk: it waits for a harness alive there, creating a
// Task when none is on its way, and falls back on any Worker after sameWait.
func (w *trial) pickLiveOn(wk *Worker) Instance {
	w.t.Helper()
	for limit := time.Now().Add(Scaled(sameWait)); time.Now().Before(limit); time.Sleep(pollEvery) {
		runs, err := w.runs()
		if err != nil {
			w.t.Fatal(err)
		}
		live := w.alive(runs)
		if same := slices.DeleteFunc(slices.Clone(live), func(in Instance) bool { return in.Worker != wk.Name }); len(same) > 0 {
			return same[w.rng.IntN(len(same))]
		}
		if !coming(runs) && len(w.tasks) < maxTasks {
			w.createTask()
		}
	}
	w.note("no harness came to %s; the next attempt goes to any Worker", wk.Name)
	return w.pickLive("a lost reply")
}

// replyAttempts bounds the attempts of loseReply, and sameWait the wait for
// a harness on the Worker of the attempt that failed. An attempt wants
// replyConns connections at first (the long poll and two idle), at most
// maxReplyConns. Before scaling, it waits up to spareWait for them, replyWait
// for its report to reach the Controller, and replyHeld with the report
// unconfirmed before it counts it as lost.
const (
	replyAttempts = 10
	replyConns    = 3
	maxReplyConns = 5
	sameWait      = 5 * time.Second
	spareWait     = 3 * time.Second
	replyWait     = 1500 * time.Millisecond
	replyHeld     = 300 * time.Millisecond
)

// tryLoseReply is one attempt of loseReply, with the harness in, once the
// Worker holds want connections or spareWait is over. It says whether the
// reply was lost, and how many connections the Worker held.
func (w *trial) tryLoseReply(attempt int, in Instance, want int) (bool, int64) {
	wk := workerNamed(w.c, in.Worker)
	for limit := time.Now().Add(Scaled(spareWait)); wk.Proxy.Stats().Active < int64(want) && time.Now().Before(limit); {
		time.Sleep(pollEvery)
	}
	before := lastWorkerSeq(w.c.ControllerSnapshot(), wk.ID)
	stats := wk.Proxy.Stats()
	wk.Proxy.SetMode(netproxy.DropReplies)
	w.note("attempt %d: drop the replies to %s, which holds %d connections (last seq applied %d)", attempt, wk.Name, stats.Active, before)
	// The heartbeat that finds its answer gone waits for it, and no longer
	// competes with the report for a connection.
	w.c.Eventually("a reply to "+wk.Name+" to be dropped", func() (bool, string) {
		return wk.Proxy.Stats().DroppedBytes > stats.DroppedBytes, "nothing dropped"
	})
	w.release(in.Execution)
	w.note("release the harness of %v", in.Execution)

	seen := &replySeen{worker: wk.Name}
	for limit := time.Now().Add(Scaled(replyWait)); time.Now().Before(limit) && seen.seq == 0; time.Sleep(pollEvery) {
		seen.seq = w.unanswered(wk, before)
	}
	if seen.seq != 0 {
		time.Sleep(Scaled(replyHeld)) // no answer can arrive while the replies are dropped
		if !slices.Contains(w.journalSeqs(wk), seen.seq) {
			seen.seq = 0
		}
	}
	if seen.seq == 0 {
		w.note("attempt %d: no report of %s was applied and left unanswered (%d new connections); heal it",
			attempt, wk.Name, wk.Proxy.Stats().Accepted-stats.Accepted)
		wk.Proxy.Heal()
		w.waitConfirmed(wk, 0)
		return false, stats.Active
	}
	w.reply = seen
	seen.dropped = wk.Proxy.Stats().DroppedBytes - stats.DroppedBytes
	seen.unconfirmed = len(w.journalSeqs(wk))
	w.note("seq %d of %s applied and unanswered; %d reply bytes dropped, %d journal rows unconfirmed",
		seen.seq, wk.Name, seen.dropped, seen.unconfirmed)
	w.pause(time.Second)
	wk.Proxy.Heal()
	w.note("heal %s", wk.Name)
	w.waitConfirmed(wk, seen.seq)
	seen.resent = true
	w.note("seq %d of %s is confirmed", seen.seq, wk.Name)
	return true, stats.Active
}

// unanswered is a seq above after of wk that the Controller applied and that
// is still in the journal of wk, unconfirmed; 0 if there is none.
func (w *trial) unanswered(wk *Worker, after int64) int64 {
	pending := w.journalSeqs(wk)
	for _, k := range journalOf(w.c.ControllerSnapshot(), wk.ID) {
		if k.seq > after && slices.Contains(pending, k.seq) {
			return k.seq
		}
	}
	return 0
}

// waitConfirmed waits for seq, or every row if seq is 0, to leave the journal of wk.
func (w *trial) waitConfirmed(wk *Worker, seq int64) {
	w.c.Eventually("the journal of "+wk.Name+" to be confirmed", func() (bool, string) {
		pending := w.journalSeqs(wk)
		if seq != 0 {
			return !slices.Contains(pending, seq), fmt.Sprintf("seq %d unconfirmed", seq)
		}
		return len(pending) == 0, fmt.Sprintf("seqs %v unconfirmed", pending)
	})
}

// journalSeqs are the seqs of the rows of the journal of wk not yet confirmed.
func (w *trial) journalSeqs(wk *Worker) []int64 {
	var seqs []int64
	ctx := w.c.H.Context()
	err := readOnly(ctx, wk.Box.Paths.WorkerStateFile(), func(tx *sql.Tx) error {
		return scanAll(ctx, tx, `SELECT seq FROM journal ORDER BY seq`, func(rows *sql.Rows) error {
			var seq int64
			err := rows.Scan(&seq)
			seqs = append(seqs, seq)
			return err
		})
	})
	if err != nil {
		w.t.Fatal(err)
	}
	return seqs
}

// lastWorkerSeq is the highest seq of worker the Controller applied.
func lastWorkerSeq(snap ControllerSnapshot, worker string) int64 {
	var last int64
	for _, k := range journalOf(snap, worker) {
		last = max(last, k.seq)
	}
	return last
}

// killController kills -9 the Controller with harnesses alive and starts it
// again within a time short of offline.
func (w *trial) killController() {
	w.pickLive("a Controller kill")
	runs, err := w.runs()
	if err != nil {
		w.t.Fatal(err)
	}
	seen := &ctlKillSeen{}
	for _, in := range w.alive(runs) {
		seen.alive = append(seen.alive, in.Execution)
	}
	w.ctlKill = seen
	down := w.between(0, 3*time.Second)
	w.c.Controller.Kill9()
	w.note("kill -9 the Controller with %v alive; restart it in %v", seen.alive, down.Round(time.Millisecond))
	time.Sleep(down)
	w.c.RestartController()
	w.note("the Controller is back")
}

// quiesce heals every Worker and releases every harness but those of lost
// Runs, which the cluster has to stop, until every Task is settled.
func (w *trial) quiesce() {
	for _, wk := range w.c.Workers {
		wk.Proxy.Heal()
	}
	w.note("quiesce: release every harness of a Run that is not lost")
	n := 0
	w.c.EventuallyWithin("the Tasks to settle", Scaled(quiesceLimit), func() (bool, string) {
		runs, err := w.runs()
		if err != nil {
			return false, err.Error()
		}
		for _, in := range w.c.Ledger.All() {
			if in.Worker == "" || w.released[in.Execution] || runs[in.Run].State == domain.RunLost || !w.c.Ledger.Live(in) {
				continue
			}
			w.release(in.Execution)
			n++
		}
		tasks, err := w.c.API.Tasks(w.c.H.Context(), client.TaskQuery{Project: w.c.Project.ID, All: true})
		if err != nil {
			return false, err.Error()
		}
		for _, t := range tasks {
			if t.State == domain.TaskQueued || t.State == domain.TaskRunning {
				return false, fmt.Sprintf("Task %s is %s", t.ID, t.State)
			}
		}
		return true, ""
	})
	w.note("quiesce: every Task settled; %d harnesses released", n)
}

// checkModel judges the end against what the test did: every Task completed,
// only Runs a partition caught were lost, and a harness that did not end is
// of a lost Run (it was stopped).
func (w *trial) checkModel() {
	w.t.Helper()
	runs, err := w.runs()
	if err != nil {
		w.t.Fatal(err)
	}
	tasks, err := w.c.API.Tasks(w.c.H.Context(), client.TaskQuery{Project: w.c.Project.ID, All: true})
	if err != nil {
		w.t.Fatal(err)
	}
	for _, t := range tasks {
		if t.State != domain.TaskCompleted {
			w.t.Errorf("e2e: model: Task %s ended %s, want completed", t.ID, taskState(t))
		}
	}
	for _, r := range runs {
		if r.State == domain.RunLost && !w.victims[r.ID] {
			w.t.Errorf("e2e: model: Run %s is lost, but no partition caught it", r.ID)
		}
	}
	for _, e := range w.tasks {
		for _, in := range w.c.Ledger.Instances(e.Token) {
			if !in.Ended && runs[in.Run].State != domain.RunLost {
				w.t.Errorf("e2e: model: the harness %v did not end, and its Run is %s", in.Execution, runs[in.Run].State)
			}
		}
	}
}

// checkCoverage fails the seed unless each transition of the template happened
// as it has to.
func (w *trial) checkCoverage() {
	w.t.Helper()
	runs, err := w.runs()
	if err != nil {
		w.t.Fatal(err)
	}
	snap := w.c.ControllerSnapshot()
	var missing []string
	miss := func(tr transition, format string, args ...any) {
		missing = append(missing, fmt.Sprintf("%s: %s", tr, fmt.Sprintf(format, args...)))
	}

	switch r := w.restart; {
	case r == nil:
		miss(daemonRestart, "it never happened")
	case r.HighWater == 0:
		miss(daemonRestart, "the journal of %s was empty when its daemon died", r.Worker)
	default:
		if fresh, _ := newEpochReports(snap, workerNamed(w.c, r.Worker).ID, *r); len(fresh) == 0 {
			miss(daemonRestart, "%s reported nothing from its new epoch", r.Worker)
		}
	}

	if p := w.partition; p == nil {
		miss(longPartition, "it never happened")
	} else {
		origin, retry := runs[p.origin.Run], runs[p.retry]
		orphaned := slices.ContainsFunc(snap.Events, func(e EventRow) bool {
			return e.RunID == origin.ID && e.Type == domain.EventRunOrphaned
		})
		switch {
		case origin.State != domain.RunLost:
			miss(longPartition, "Run %s is %s, not lost", origin.ID, origin.State)
		case !p.aliveWithRetry:
			miss(longPartition, "the cut-off harness %v was not alive while its retry ran", p.origin.Execution)
		case retry.State != domain.RunCompleted || retry.WorkerID != p.other.ID:
			miss(longPartition, "the retry %s is %s on %q, not completed on %s", retry.ID, retry.State, retry.WorkerID, p.other.Name)
		case !orphaned:
			miss(longPartition, "Run %s has no RunOrphaned", origin.ID)
		case slices.ContainsFunc(w.c.Ledger.Instances(p.origin.Token), func(in Instance) bool { return in.Execution == p.origin.Execution && in.Ended }):
			miss(longPartition, "the cut-off harness %v ended instead of being stopped", p.origin.Execution)
		}
	}

	switch r := w.reply; {
	case r == nil:
		miss(lostReply, "it never happened")
	case r.dropped == 0 || r.unconfirmed == 0 || !r.resent:
		miss(lostReply, "seq %d of %s: %d reply bytes dropped, %d rows unconfirmed, confirmed after healing %t",
			r.seq, r.worker, r.dropped, r.unconfirmed, r.resent)
	}

	switch k := w.ctlKill; {
	case k == nil:
		miss(controllerKill, "it never happened")
	case len(k.alive) == 0:
		miss(controllerKill, "no harness was alive when the Controller died")
	}

	if len(missing) > 0 {
		w.t.Errorf("e2e: the seed did not go through the template:\n  %s", strings.Join(missing, "\n  "))
	}
}
