package daemon_test

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	cfruntime "github.com/Autarquico/codefabric/internal/adapters/runtime"
	v1 "github.com/Autarquico/codefabric/internal/api/v1"
	"github.com/Autarquico/codefabric/internal/client"
	"github.com/Autarquico/codefabric/internal/config"
	controllerdaemon "github.com/Autarquico/codefabric/internal/controller/daemon"
	"github.com/Autarquico/codefabric/internal/domain"
	"github.com/Autarquico/codefabric/internal/worker/daemon"
	"github.com/Autarquico/codefabric/internal/worker/sysinfo"
)

const interval = 20 * time.Millisecond

// controller is a real Controller on localhost that a test can stop and start again.
type controller struct {
	t     *testing.T
	paths config.Paths
	owner *client.Client
	stop  func()

	mutate []func(*controllerdaemon.Options)
}

func newController(t *testing.T, mutate ...func(*controllerdaemon.Options)) *controller {
	t.Helper()
	root := t.TempDir()
	paths := config.Paths{ConfigDir: filepath.Join(root, "config"), DataDir: filepath.Join(root, "data")}
	ln, err := new(net.ListenConfig).Listen(t.Context(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	_ = ln.Close()
	if _, err := controllerdaemon.Init(t.Context(), controllerdaemon.InitOptions{Paths: paths, Advertise: addr, Listen: addr, OwnerName: "owner"}); err != nil {
		t.Fatal(err)
	}
	c := &controller{t: t, paths: paths, mutate: mutate}
	c.start()
	t.Cleanup(c.stopIt)
	if c.owner, err = client.Open(paths.CredentialsDir()); err != nil {
		t.Fatal(err)
	}
	return c
}

func (c *controller) start() {
	c.t.Helper()
	ctx, cancel := context.WithCancel(c.t.Context())
	opts := controllerdaemon.Options{Paths: c.paths, Logger: slog.New(slog.DiscardHandler)}
	for _, m := range c.mutate {
		m(&opts)
	}
	ctrl, err := controllerdaemon.Start(ctx, opts)
	if err != nil {
		cancel()
		c.t.Fatalf("Start: %v", err)
	}
	c.stop = func() {
		cancel()
		if err := ctrl.Wait(); err != nil {
			c.t.Errorf("Wait: %v", err)
		}
		c.stop = func() {}
	}
}

func (c *controller) stopIt() { c.stop() }

// machine is a Worker's own config and data directories.
func machine(t *testing.T) config.Paths {
	t.Helper()
	root := t.TempDir()
	return config.Paths{ConfigDir: filepath.Join(root, "config"), DataDir: filepath.Join(root, "data")}
}

// join enrols a machine as a Worker of the Controller.
func (c *controller) join(name string, paths config.Paths) {
	c.t.Helper()
	res, err := c.owner.CreateToken(c.t.Context(), domain.KindWorker, time.Hour)
	if err != nil {
		c.t.Fatal(err)
	}
	if _, err := daemon.Join(c.t.Context(), daemon.JoinOptions{Paths: paths, Invitation: res.Invite, Name: name}); err != nil {
		c.t.Fatalf("Join: %v", err)
	}
}

// logs is a log destination that tests can read while the daemon writes.
type logs struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (l *logs) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.buf.Write(p)
}

func (l *logs) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.buf.String()
}

// running is a daemon started by a test.
type running struct {
	cancel func()
	done   chan struct{}
	err    error
}

// stop ends the daemon and returns what Run returned.
func (r *running) stop() error {
	r.cancel()
	<-r.done
	return r.err
}

func run(t *testing.T, paths config.Paths, out *logs, rt ...cfruntime.Runtime) *running {
	t.Helper()
	ctx, cancel := context.WithCancel(t.Context())
	r := &running{cancel: cancel, done: make(chan struct{})}
	opts := daemon.Options{Paths: paths, Interval: interval, Logger: slog.New(slog.NewTextHandler(out, nil))}
	if len(rt) > 0 {
		opts.Runtime = rt[0]
	}
	go func() {
		defer close(r.done)
		r.err = daemon.Run(ctx, opts)
	}()
	t.Cleanup(func() { _ = r.stop() })
	return r
}

// eventually polls until the Worker satisfies ok, and returns it.
func (c *controller) eventually(ref string, what string, ok func(v1.Worker) bool) v1.Worker {
	c.t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	var last v1.Worker
	for time.Now().Before(deadline) {
		if w, err := c.owner.Worker(c.t.Context(), ref); err == nil {
			if last = w; ok(w) {
				return w
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	c.t.Fatalf("timed out waiting for %s; last seen %+v", what, last)
	return last
}

func TestScenario_WorkerRegistryHeartbeat_Epoch(t *testing.T) {
	c := newController(t)
	paths := machine(t)
	c.join("box", paths)

	first := run(t, paths, &logs{})
	firstState := c.eventually("box", "the first run to be online", func(w v1.Worker) bool { return w.State == domain.WorkerOnline })
	if firstState.Epoch == "" {
		t.Fatal("the Worker reported no epoch")
	}
	if err := first.stop(); err != nil {
		t.Fatalf("stopping the daemon: %v", err)
	}

	// The same machine, started again: a new instance of the daemon.
	run(t, paths, &logs{})
	second := c.eventually("box", "the epoch to change", func(w v1.Worker) bool { return w.Epoch != "" && w.Epoch != firstState.Epoch })
	if second.State != domain.WorkerOnline {
		t.Errorf("after the restart: %+v", second)
	}
}

func TestRun_ReportsTheMachineAndItsVersion(t *testing.T) {
	c := newController(t)
	paths := machine(t)
	c.join("box", paths)
	run(t, paths, &logs{})

	w := c.eventually("box", "the Worker to be online", func(w v1.Worker) bool { return w.State == domain.WorkerOnline })
	host, _ := os.Hostname()
	if w.Hostname != host || w.OS == "" || w.Arch == "" || w.DaemonVersion == "" || !w.Compatible {
		t.Errorf("identity of the machine: %+v", w)
	}
	if w.Resources.Total.CPU < 1 || w.Resources.Total.MemoryMB < 1 || w.Resources.Total.DiskMB < 1 ||
		w.Resources.Allocatable != w.Resources.Total || w.Resources.Free != w.Resources.Allocatable || w.Health.Status != v1.HealthOK {
		t.Errorf("resources and health: %+v", w)
	}
}

func TestRun_KeepsTryingWhileTheControllerIsDownAndRecoversWhenItReturns(t *testing.T) {
	c := newController(t)
	paths := machine(t)
	c.join("box", paths)
	out := &logs{}
	daemonRun := run(t, paths, out)
	c.eventually("box", "the Worker to be online", func(w v1.Worker) bool { return w.State == domain.WorkerOnline })
	before := c.eventually("box", "a heartbeat", func(w v1.Worker) bool { return w.LastHeartbeatAt != nil })

	c.stop()
	deadline := time.Now().Add(10 * time.Second)
	for strings.Count(out.String(), "heartbeat failed") < 3 {
		if time.Now().After(deadline) {
			t.Fatalf("the daemon did not retry:\n%s", out)
		}
		time.Sleep(10 * time.Millisecond)
	}

	c.start()
	var err error
	if c.owner, err = client.Open(c.paths.CredentialsDir()); err != nil {
		t.Fatal(err)
	}
	c.eventually("box", "a heartbeat after the Controller returned", func(w v1.Worker) bool {
		return w.LastHeartbeatAt != nil && w.LastHeartbeatAt.After(*before.LastHeartbeatAt)
	})
	if err := daemonRun.stop(); err != nil {
		t.Errorf("a daemon that retried and recovered ended with %v", err)
	}
}

// runMeasuring starts a daemon whose measures are the real ones until broken is set.
func runMeasuring(t *testing.T, paths config.Paths, broken *atomic.Bool) *running {
	t.Helper()
	ctx, cancel := context.WithCancel(t.Context())
	r := &running{cancel: cancel, done: make(chan struct{})}
	go func() {
		defer close(r.done)
		r.err = daemon.Run(ctx, daemon.Options{
			Paths: paths, Interval: interval, Logger: slog.New(slog.DiscardHandler),
			Measure: func(dir string) (sysinfo.Info, error) {
				if broken.Load() {
					return sysinfo.Info{}, errors.New("statfs: input/output error")
				}
				return sysinfo.Collect(dir)
			},
		})
	}()
	t.Cleanup(func() { _ = r.stop() })
	return r
}

func TestRun_AMachineThatCannotBeMeasuredIsDegradedWithItsLastMeasuresAndTheDaemonGoesOn(t *testing.T) {
	c := newController(t)
	paths := machine(t)
	c.join("box", paths)
	var broken atomic.Bool
	daemonRun := runMeasuring(t, paths, &broken)
	online := c.eventually("box", "the Worker to be online", func(w v1.Worker) bool { return w.State == domain.WorkerOnline })

	broken.Store(true)
	sick := c.eventually("box", "the Worker to be degraded", func(w v1.Worker) bool { return w.State == domain.WorkerDegraded })
	if sick.Health.Status != v1.HealthDegraded || sick.Health.Message == "" || strings.Contains(sick.Health.Message, "statfs") {
		t.Errorf("health = %+v, want degraded with a message that keeps the cause out", sick.Health)
	}
	if sick.Resources.Total != online.Resources.Total {
		t.Errorf("total = %+v, want the last known, %+v", sick.Resources.Total, online.Resources.Total)
	}
	select {
	case <-daemonRun.done:
		t.Fatalf("the daemon ended: %v", daemonRun.err)
	default:
	}

	broken.Store(false)
	c.eventually("box", "the Worker to be online again", func(w v1.Worker) bool { return w.State == domain.WorkerOnline })
}

func TestRun_AMachineThatNeverCouldBeMeasuredKeepsTheDaemonRunning(t *testing.T) {
	c := newController(t)
	paths := machine(t)
	c.join("box", paths)
	var broken atomic.Bool
	broken.Store(true)
	daemonRun := runMeasuring(t, paths, &broken)

	time.Sleep(20 * interval)
	select {
	case <-daemonRun.done:
		t.Fatalf("the daemon ended: %v", daemonRun.err)
	default:
	}
	if w, err := c.owner.Worker(t.Context(), "box"); err != nil || w.State != domain.WorkerJoining {
		t.Fatalf("a Worker that reported nothing: %+v, %v", w, err)
	}

	broken.Store(false)
	c.eventually("box", "the Worker to be online", func(w v1.Worker) bool { return w.State == domain.WorkerOnline })
}

func TestRun_EndsWhenTheWorkerIsRemoved(t *testing.T) {
	c := newController(t)
	paths := machine(t)
	c.join("box", paths)
	daemonRun := run(t, paths, &logs{})
	c.eventually("box", "the Worker to be online", func(w v1.Worker) bool { return w.State == domain.WorkerOnline })

	if _, err := c.owner.RemoveWorker(t.Context(), "box"); err != nil {
		t.Fatal(err)
	}

	select {
	case <-daemonRun.done:
		var coded *domain.Error
		err := daemonRun.err
		if !errors.As(err, &coded) || coded.Code != domain.CodeUnauthenticated || !strings.Contains(coded.Message, "cf worker join") {
			t.Errorf("Run ended with %v, want CF_UNAUTHENTICATED with the way out for a Worker", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the daemon kept running with revoked credentials")
	}
}

func TestRun_NeedsWorkerCredentials(t *testing.T) {
	err := daemon.Run(t.Context(), daemon.Options{Paths: machine(t), Interval: interval})
	var coded *domain.Error
	if !errors.As(err, &coded) || coded.Code != domain.CodeUnauthenticated {
		t.Errorf("Run = %v, want CF_UNAUTHENTICATED", err)
	}
}

func TestJoin_KeepsTheWorkersCredentialsApartFromTheClients(t *testing.T) {
	c := newController(t)
	paths := machine(t)

	c.join("box", paths)

	for _, f := range []string{"cert.pem", "key.pem", "ca.pem", "controller"} {
		if _, err := os.Stat(filepath.Join(paths.WorkerCredentialsDir(), f)); err != nil {
			t.Errorf("worker credentials: %v", err)
		}
	}
	if _, err := os.Stat(paths.CredentialsDir()); !os.IsNotExist(err) {
		t.Errorf("the client's credentials directory exists: %v", err)
	}
	info, err := client.Describe(paths.WorkerCredentialsDir())
	if err != nil {
		t.Fatal(err)
	}
	if kind, _, err := domain.ParseID(info.IdentityID); err != nil || kind != domain.KindWorker {
		t.Errorf("identity %q is not a Worker's", info.IdentityID)
	}
}

func TestJoin_ReplacesStoredCredentialsOnlyWithForce(t *testing.T) {
	c := newController(t)
	paths := machine(t)
	c.join("box", paths)
	before, err := client.Describe(paths.WorkerCredentialsDir())
	if err != nil {
		t.Fatal(err)
	}

	res, err := c.owner.CreateToken(t.Context(), domain.KindWorker, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	opts := daemon.JoinOptions{Paths: paths, Invitation: res.Invite, Name: "box2"}
	if _, err := daemon.Join(t.Context(), opts); err == nil || !strings.Contains(err.Error(), "--force") {
		t.Fatalf("join over existing credentials: %v", err)
	}
	// The refusal came before the token was spent.
	opts.Force = true
	if _, err := daemon.Join(t.Context(), opts); err != nil {
		t.Fatalf("join with force: %v", err)
	}
	after, err := client.Describe(paths.WorkerCredentialsDir())
	if err != nil || after.IdentityID == before.IdentityID {
		t.Errorf("credentials after force: %+v, %v", after, err)
	}
}
