//go:build e2e && (linux || darwin)

package e2e

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	v1 "github.com/Autarquico/codefabric/internal/api/v1"
	"github.com/Autarquico/codefabric/internal/client"
	"github.com/Autarquico/codefabric/internal/config"
	"github.com/Autarquico/codefabric/internal/domain"
	"github.com/Autarquico/codefabric/internal/e2e/netproxy"
	"github.com/Autarquico/codefabric/internal/support/gittest"
)

// envScale multiplies every time of the cluster: heartbeats, timeouts, the
// waits of the tests and the cap of the ledger's script. A slow machine sets
// it above 1 instead of the tests guessing.
const envScale = "CF_E2E_SCALE"

var scale = sync.OnceValues(func() (float64, error) {
	text := os.Getenv(envScale)
	if text == "" {
		return 1, nil
	}
	f, err := strconv.ParseFloat(text, 64)
	if err != nil || f <= 0 {
		return 0, fmt.Errorf("e2e: %s=%q is not a positive number", envScale, text)
	}
	return f, nil
})

// Scaled is d times CF_E2E_SCALE.
func Scaled(d time.Duration) time.Duration {
	f, _ := scale()
	return time.Duration(float64(d) * f)
}

// The times of a cluster before scaling: wide margins, so that a pause of the
// machine is not taken for a partition (no boundary is tested with a real clock).
const (
	defaultHeartbeat   = 250 * time.Millisecond
	defaultOfflineTTL  = 3 * time.Second
	defaultLostTimeout = 8 * time.Second
	// waitLimit bounds an Eventually, and startLimit the start of a daemon.
	waitLimit  = 30 * time.Second
	startLimit = 20 * time.Second
	// bindAttempts is how often a Controller whose port is not free yet is started again.
	bindAttempts = 10
	pollEvery    = 50 * time.Millisecond
)

// Opts shape a cluster. Times are before CF_E2E_SCALE, which NewCluster applies
// to them; zero is the default.
type Opts struct {
	// Workers is how many Workers join (default 1), named w1, w2, ...
	Workers int
	// Proxied puts a netproxy between each Worker and the Controller, its API
	// and its git relay alike.
	Proxied bool
	// HeartbeatEvery is the heartbeat of the daemons (default 250ms).
	HeartbeatEvery time.Duration
	// OfflineTTL is worker_offline_ttl (default 3s) and LostTimeout
	// run_lost_timeout (default 8s), in controller.json.
	OfflineTTL  time.Duration
	LostTimeout time.Duration
	// MaxConcurrent is `worker join --max-concurrent`; zero leaves the product's default.
	MaxConcurrent int
}

func (o Opts) withDefaults() Opts {
	if o.Workers == 0 {
		o.Workers = 1
	}
	if o.HeartbeatEvery == 0 {
		o.HeartbeatEvery = defaultHeartbeat
	}
	if o.OfflineTTL == 0 {
		o.OfflineTTL = defaultOfflineTTL
	}
	if o.LostTimeout == 0 {
		o.LostTimeout = defaultLostTimeout
	}
	o.HeartbeatEvery, o.OfflineTTL, o.LostTimeout = Scaled(o.HeartbeatEvery), Scaled(o.OfflineTTL), Scaled(o.LostTimeout)
	return o
}

// Cluster is a Controller, its Workers and a client that only observes, each
// in a box of its own, with a Project whose repository the owner has cloned.
// The owner is the machine of the Controller.
type Cluster struct {
	t *testing.T
	H *Harness
	// Opts are those the cluster runs with, scaled.
	Opts Opts

	// Ctl is the machine of the Controller and of its owner, and Controller its daemon.
	Ctl        *Box
	Controller *Proc
	// Addr is the 127.0.0.1:<port> the Controller listens on, the same across restarts.
	Addr string
	// API is the owner's client: what the waits ask, without a cf per look.
	API *client.Client

	Workers []*Worker
	// Observer is a third client, connected in a box of its own.
	Observer *Box

	// Origin is the bare repository of the Project and Repo the owner's clone of it.
	Origin, Repo string
	Project      v1.Project
	Ledger       *Ledger

	decoy *decoy
	cfN   atomic.Int64
}

// Worker is a Worker machine: its daemon and, in a proxied cluster, the proxy
// it reaches the Controller through.
type Worker struct {
	Name   string
	ID     string
	Box    *Box
	Daemon *Proc
	Proxy  *netproxy.Proxy
}

// NewCluster brings a cluster up: the Controller answering, every Worker online
// with its harness offered, the observer connected and the Project created.
func NewCluster(t *testing.T, o Opts) *Cluster {
	t.Helper()
	if _, err := scale(); err != nil {
		t.Fatal(err)
	}
	c := &Cluster{t: t, H: New(t), Opts: o.withDefaults()}
	c.startController()
	if c.Opts.Proxied {
		c.decoy = listenDecoy(t)
		c.H.OnTeardown(c.decoy.close)
	}
	for i := range c.Opts.Workers {
		c.Workers = append(c.Workers, c.joinWorker(fmt.Sprintf("w%d", i+1)))
	}
	c.connectObserver()
	c.makeProject()
	c.Ledger = newLedger(t, filepath.Join(c.H.Dir(), "ledger"), c.Workers)
	return c
}

// startController inits the Controller on a port of its own, writes its
// timeouts and starts it.
func (c *Cluster) startController() {
	c.t.Helper()
	c.Ctl = c.H.Box("ctl")
	c.Addr = freeAddr(c.t)
	c.mustCF(c.Ctl, "controller", "init", "--advertise", c.Addr, "--listen", c.Addr, "--name", "owner")
	if min(c.Opts.OfflineTTL, c.Opts.LostTimeout) < time.Second {
		c.t.Fatalf("e2e: worker_offline_ttl %v and run_lost_timeout %v: controller.json takes 1s at least; raise %s or the Opts",
			c.Opts.OfflineTTL, c.Opts.LostTimeout, envScale)
	}
	cfg, err := config.LoadController(c.Ctl.Paths)
	if err != nil {
		c.t.Fatal(err)
	}
	cfg.WorkerOfflineTTL, cfg.RunLostTimeout = config.Duration(c.Opts.OfflineTTL), config.Duration(c.Opts.LostTimeout)
	if err := config.SaveController(c.Ctl.Paths, cfg); err != nil {
		c.t.Fatal(err)
	}
	if c.API, err = client.Open(c.Ctl.Paths.CredentialsDir()); err != nil {
		c.t.Fatal(err)
	}
	c.Controller = c.Ctl.CF(RoleController, "controller", "controller", "run")
	c.waitController()
}

// RestartController starts the Controller again, after a kill -9 if it is
// running, on the same port, and returns once it answers.
func (c *Cluster) RestartController() {
	c.t.Helper()
	c.Controller.Restart()
	c.waitController()
}

// waitController waits for the Controller to answer. One that exits because
// its port is not free yet (TIME_WAIT) is started again, and that is logged.
func (c *Cluster) waitController() {
	c.t.Helper()
	for attempt := 1; ; attempt++ {
		ctx, cancel := context.WithTimeout(c.H.Context(), Scaled(startLimit))
		err := c.untilAnswers(ctx)
		cancel()
		if err == nil {
			return
		}
		log := c.Controller.Log()
		if c.Controller.Running() || attempt == bindAttempts || !strings.Contains(log, "address already in use") {
			c.t.Fatalf("e2e: the Controller did not answer: %v\n%s", err, c.Logs())
		}
		c.t.Logf("e2e: the Controller could not bind %s yet (attempt %d); starting it again", c.Addr, attempt)
		time.Sleep(200 * time.Millisecond)
		c.Controller.Restart()
	}
}

func (c *Cluster) untilAnswers(ctx context.Context) error {
	for {
		if !c.Controller.Running() {
			return errors.New("it exited")
		}
		reqCtx, cancel := context.WithTimeout(ctx, time.Second)
		_, err := c.API.Status(reqCtx)
		cancel()
		if err == nil {
			return nil
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("%w (last: %w)", ctx.Err(), err)
		case <-time.After(pollEvery):
		}
	}
}

// joinWorker joins a Worker named name, puts its proxy in front if the cluster
// is proxied, and starts its daemon.
func (c *Cluster) joinWorker(name string) *Worker {
	c.t.Helper()
	w := &Worker{Name: name, Box: c.H.Box(name)}
	invite := strings.TrimSpace(c.mustCF(c.Ctl, "controller", "token", "create", "--worker").Stdout)
	// A small --disk-min-free: the disk of the CI machine must not degrade a Worker.
	join := []string{"worker", "join", invite, "--name", name, "--disk-min-free", "1MiB"}
	if c.Opts.MaxConcurrent > 0 {
		join = append(join, "--max-concurrent", strconv.Itoa(c.Opts.MaxConcurrent))
	}
	c.mustCF(w.Box, join...)
	run := []string{"worker", "run", "--heartbeat-interval", c.Opts.HeartbeatEvery.String()}
	if c.Opts.Proxied {
		p, err := netproxy.Listen(context.Background(), "127.0.0.1:0", func() string { return c.Addr })
		if err != nil {
			c.t.Fatal(err)
		}
		c.H.OnTeardown(func() { _ = p.Close() })
		w.Proxy = p
		// The address the Worker stored at join is one nobody serves: whatever
		// dials it instead of --controller is counted, and gets nothing.
		file := filepath.Join(w.Box.Paths.WorkerCredentialsDir(), "controller")
		if err := os.WriteFile(file, []byte(c.decoy.addr()+"\n"), 0o600); err != nil {
			c.t.Fatal(err)
		}
		run = append(run, "--controller", p.Addr())
	}
	w.Daemon = w.Box.CF(RoleDaemon, "daemon", run...)
	c.WaitOnline(w)
	return w
}

// RestartWorker starts the daemon of w again, after a kill -9 if it is
// running, and returns once the Worker is online.
func (c *Cluster) RestartWorker(w *Worker) {
	c.t.Helper()
	w.Daemon.Restart()
	c.WaitOnline(w)
}

// WaitOnline waits for w to be online with its harness offered.
func (c *Cluster) WaitOnline(w *Worker) {
	c.t.Helper()
	c.EventuallyWithin("Worker "+w.Name+" to be online", Scaled(startLimit), func() (bool, string) {
		got, err := c.API.Worker(c.H.Context(), w.Name)
		if err != nil {
			return false, err.Error()
		}
		w.ID = got.ID
		return got.State == domain.WorkerOnline && len(got.Capabilities) > 0,
			fmt.Sprintf("state=%s capabilities=%v daemon running=%t", got.State, got.Capabilities, w.Daemon.Running())
	})
}

// Bypasses is how many connections went to the address the Workers stored at
// join instead of to their proxies: zero, or a partition partitions nothing.
func (c *Cluster) Bypasses() int64 {
	if c.decoy == nil {
		return 0
	}
	return c.decoy.n.Load()
}

func (c *Cluster) connectObserver() {
	c.t.Helper()
	invite := strings.TrimSpace(c.mustCF(c.Ctl, "controller", "token", "create", "--client").Stdout)
	c.Observer = c.H.Box("obs")
	c.mustCF(c.Observer, "connect", invite, "--name", "observer")
}

// makeProject makes a bare origin with one commit on main, the owner's clone
// of it, and the Project of that clone, whose Runs ask for 1 CPU and 256 MB:
// two fit any CI runner.
func (c *Cluster) makeProject() {
	c.t.Helper()
	c.Origin = filepath.Join(c.H.Dir(), "origin.git")
	c.Repo = filepath.Join(c.Ctl.Dir, "repo")
	gittest.Run(c.t, c.H.Dir(), "init", "--quiet", "--bare", "--initial-branch=main", c.Origin)
	gittest.Run(c.t, c.Ctl.Dir, "clone", "--quiet", "file://"+c.Origin, c.Repo)
	gittest.Commit(c.t, c.Repo, "README", "e2e\n", "first")
	gittest.Run(c.t, c.Repo, "push", "--quiet", "origin", "main")
	JSON[v1.Project](c, c.Ctl, "project", "create", "e2e")
	c.Project = JSON[v1.Project](c, c.Ctl, "project", "set", "e2e", "--cpu", "1", "--memory-mb", "256")
	if r := c.Project.Defaults.Resources; r == nil || r.CPU != 1 || r.MemoryMB != 256 {
		c.t.Fatalf("e2e: the Project asks for %+v", r)
	}
}

// RelayRepo is the bare repository of the Project in the Controller's git relay.
func (c *Cluster) RelayRepo() string {
	return filepath.Join(c.Ctl.Paths.RelayDir(), c.Project.ID+".git")
}

// WaitRun waits for the Run to be as ok says, and returns it as it was then.
func (c *Cluster) WaitRun(run, what string, ok func(v1.Run) bool) v1.Run {
	c.t.Helper()
	var got v1.Run
	c.Eventually("Run "+run+" to be "+what, func() (bool, string) {
		r, err := c.API.Run(c.H.Context(), run)
		if err != nil {
			return false, err.Error()
		}
		got = r
		return ok(r), RunStage(r)
	})
	return got
}

// RunStage is where a Run stands, for a wait that fails.
func RunStage(r v1.Run) string {
	s := fmt.Sprintf("run %s state=%s runtime=%s publication=%s worker=%q queued_reason=%q", r.ID, r.State, r.Runtime, r.Publication, r.WorkerID, r.QueuedReason)
	if r.Lease != nil {
		s += fmt.Sprintf(" lease=%s epoch=%d", r.Lease.State, r.Lease.Epoch)
	}
	return s
}

// Eventually waits, for as long as waitLimit scaled, for check to say done.
// check also says where things stand; a wait that fails shows the last of it
// and the logs of every process. It is called from the test's goroutine only.
func (c *Cluster) Eventually(what string, check func() (done bool, stage string)) {
	c.t.Helper()
	c.EventuallyWithin(what, Scaled(waitLimit), check)
}

// EventuallyWithin is Eventually with a limit of its own.
func (c *Cluster) EventuallyWithin(what string, limit time.Duration, check func() (done bool, stage string)) {
	c.t.Helper()
	start := time.Now()
	deadline := start.Add(limit)
	if d := c.H.Deadline(); !d.IsZero() && d.Before(deadline) {
		deadline = d
	}
	for {
		done, stage := check()
		if done {
			return
		}
		if time.Now().After(deadline) {
			c.t.Fatalf("e2e: timed out after %v waiting for %s; it stood at: %s\n%s",
				time.Since(start).Round(time.Millisecond), what, stage, c.Logs())
		}
		time.Sleep(pollEvery)
	}
}

// logTail bounds what Logs shows of each log.
const logTail = 16 << 10

// Logs is the tail of the log of every process of the harness but the cf
// commands that ended well: what a failure shows.
func (c *Cluster) Logs() string {
	var out strings.Builder
	for _, p := range c.H.Procs() {
		running := p.Running()
		var err error
		if !running {
			err = p.Wait(context.Background())
		}
		if p.role == RoleClient && !running && err == nil {
			continue
		}
		status := "running"
		if !running {
			status = fmt.Sprintf("ended: %v", err)
		}
		fmt.Fprintf(&out, "--- %s/%s (start %d, pid %d, %s) %s\n%s\n", p.box.Name, p.name, p.Starts(), p.PID(), status, p.LogPath(), tail(p.Log()))
		if p.splitStderr {
			fmt.Fprintf(&out, "--- stderr:\n%s\n", tail(p.Stderr()))
		}
	}
	return hideInvites(out.String())
}

func tail(s string) string {
	if len(s) <= logTail {
		return s
	}
	return "[...]" + s[len(s)-logTail:]
}

// freeAddr is a 127.0.0.1 address with a port that was free a moment ago.
func freeAddr(t *testing.T) string {
	t.Helper()
	ln, err := new(net.ListenConfig).Listen(context.Background(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	_ = ln.Close()
	return addr
}

// decoy listens where nothing should dial, and counts and drops what does.
type decoy struct {
	ln   net.Listener
	n    atomic.Int64
	done chan struct{}
}

func listenDecoy(t *testing.T) *decoy {
	t.Helper()
	ln, err := new(net.ListenConfig).Listen(context.Background(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	d := &decoy{ln: ln, done: make(chan struct{})}
	go func() {
		defer close(d.done)
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			d.n.Add(1)
			_ = conn.Close()
		}
	}()
	return d
}

func (d *decoy) addr() string { return d.ln.Addr().String() }

func (d *decoy) close() {
	_ = d.ln.Close()
	<-d.done
}
