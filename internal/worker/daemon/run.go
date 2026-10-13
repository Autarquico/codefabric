package daemon

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"os/user"
	"time"

	"github.com/oklog/ulid/v2"
	"golang.org/x/sync/errgroup"

	cfruntime "github.com/Autarquico/codefabric/internal/adapters/runtime"

	"github.com/Autarquico/codefabric/internal/adapters/harness"
	"github.com/Autarquico/codefabric/internal/adapters/harness/command"
	"github.com/Autarquico/codefabric/internal/adapters/runtime/tmux"
	v1 "github.com/Autarquico/codefabric/internal/api/v1"
	"github.com/Autarquico/codefabric/internal/buildinfo"
	"github.com/Autarquico/codefabric/internal/client"
	"github.com/Autarquico/codefabric/internal/config"
	"github.com/Autarquico/codefabric/internal/domain"
	"github.com/Autarquico/codefabric/internal/support/clock"
	"github.com/Autarquico/codefabric/internal/worker/executor"
	"github.com/Autarquico/codefabric/internal/worker/gitops"
	"github.com/Autarquico/codefabric/internal/worker/journal"
	"github.com/Autarquico/codefabric/internal/worker/outputship"
	"github.com/Autarquico/codefabric/internal/worker/procident"
	"github.com/Autarquico/codefabric/internal/worker/state"
	"github.com/Autarquico/codefabric/internal/worker/sysinfo"
)

// Options parameterise Run.
type Options struct {
	Paths config.Paths
	// Controller dials this host:port instead of the one stored at join time.
	Controller string
	// Interval is the time between heartbeats; zero means the default of Appendix A.
	Interval time.Duration
	// Version is what this daemon reports; zero means the version of the binary.
	Version string
	Logger  *slog.Logger // defaults to slog.Default()
	Clock   clock.Clock  // defaults to clock.Real
	// Measure replaces sysinfo.Collect, which tests cannot make fail.
	Measure func(dir string) (sysinfo.Info, error)
	// Runtime runs the sessions of the Runs. It defaults to tmux, in a server
	// of this Worker; with no tmux, the daemon still heartbeats but offers no
	// harness and rejects what it is given.
	Runtime cfruntime.Runtime
	// WrapperArgv replaces `<this binary> worker exec`, for tests.
	WrapperArgv []string
	// StopGrace replaces stop.grace (Appendix A), for tests.
	StopGrace time.Duration
	// GitPath replaces the git found on the PATH, for tests.
	GitPath string
	// SSHTarget is how clients reach this machine to attach to a session
	// ([user@]host[:port]). Empty announces <user>@<hostname> when that is a
	// valid target; "none" announces nothing, and the Runs cannot be attached.
	SSHTarget string
	// OutputInterval replaces the second between the passes that send output, for tests.
	OutputInterval time.Duration
}

// NoSSHTarget is the value of Options.SSHTarget that announces none.
const NoSSHTarget = "none"

// Run sends a heartbeat now and then every Interval until ctx ends, and returns
// nil when it does. Each Run is a new instance of the daemon: it has an epoch
// of its own, which tells the Controller that the daemon restarted.
//
// Next to the heartbeat it runs the executor of the Worker (§6.4): it recovers
// the Runs of the previous instance, takes the assignments of the Controller and
// supervises the harnesses, and sends the journal of reports. The harnesses do
// not belong to the daemon: ending Run leaves them running.
//
// A Controller that cannot be reached is retried with a backoff that never
// exceeds the interval, so the Worker is back as soon as the Controller is.
// Anything else (credentials revoked or expired, a Controller that refuses the
// heartbeat) ends Run with that error: retrying would not change it.
//
// A machine that cannot be measured does not stop the daemon: it keeps
// heartbeating with the last measures it has and reports itself degraded.
func Run(ctx context.Context, o Options) error {
	if exists, err := client.CredentialsExist(o.Paths.WorkerCredentialsDir()); err != nil {
		return err
	} else if !exists {
		return domain.NewError(domain.CodeUnauthenticated, "this machine has not joined as a Worker; run `cf worker join <invitation>`", nil)
	}
	var opts []client.Option
	if o.Controller != "" {
		opts = append(opts, client.WithController(o.Controller))
	}
	opts = append(opts, client.WithRecoveryHint(client.WorkerRecoveryHint))
	c, err := client.Open(o.Paths.WorkerCredentialsDir(), opts...)
	if err != nil {
		return err
	}
	d := &daemon{
		client: c, dataDir: o.Paths.DataDir, epoch: ulid.Make().String(),
		interval: o.Interval, version: o.Version, log: o.Logger, measure: o.Measure, clock: o.Clock,
	}
	if d.measure == nil {
		d.measure = sysinfo.Collect
	}
	if d.clock == nil {
		d.clock = clock.Real{}
	}
	if d.interval <= 0 {
		d.interval = domain.Defaults().WorkerHeartbeatInterval
	}
	if d.version == "" {
		d.version = buildinfo.Get().Version
	}
	if d.log == nil {
		d.log = slog.Default()
	}
	switch o.SSHTarget {
	case "", NoSSHTarget:
		d.sshTarget, d.defaultSSH = "", o.SSHTarget == ""
	default:
		if !domain.ValidSSHTarget(o.SSHTarget) {
			return domain.NewError(domain.CodeInvalidRequest, "--ssh-target must be [user@]host[:port], or none", nil)
		}
		d.sshTarget = o.SSHTarget
	}

	db, err := state.Open(ctx, o.Paths.WorkerStateFile())
	if err != nil {
		return err
	}
	defer func() { _ = db.Close() }()
	jrn := journal.New(d.epoch, d.clock)
	rt, launchers := d.runtimeAndHarnesses(ctx, o)
	eo := executor.Options{
		DB: db, Controller: c, Runtime: rt, Journal: jrn, Harnesses: launchers, Paths: o.Paths, Clock: d.clock, Log: d.log,
		WrapperArgv: o.WrapperArgv, StopGrace: o.StopGrace,
	}
	// Publishing the result of a Run needs git: without it this Worker offers no harness.
	if git, gitErr := gitops.New(ctx, gitops.Options{Paths: o.Paths, Clock: d.clock, Log: d.log, GitPath: o.GitPath}); gitErr != nil {
		d.log.WarnContext(ctx, "git is not usable: this Worker offers no harness and will reject the Runs it is given", "err", gitErr)
		eo.Runtime, eo.Harnesses, launchers = unavailable{}, nil, nil
	} else {
		files := c.TLSFiles()
		eo.Git, eo.RelayTLS = git, executor.RelayTLS{Cert: files.Cert, Key: files.Key, CA: files.CA}
	}
	ex, err := executor.New(eo)
	if err != nil {
		return err
	}
	for _, l := range launchers {
		d.capabilities = append(d.capabilities, "harness:"+l.ID())
	}
	if p, ok := eo.Runtime.(cfruntime.AttachBinaryProvider); ok {
		if name, path := p.AttachBinary(); name != "" {
			d.attachBinaries = map[string]string{name: path}
		}
	}
	d.onResponse = func(r v1.HeartbeatResponse) {
		ex.SetLimits(executor.Limits{MaxConcurrentRuns: r.MaxConcurrentRuns, CPU: r.Allocatable.CPU, MemoryMB: r.Allocatable.MemoryMB})
		if r.ReconcileRequired && ex.Reconciled() {
			// The Controller restarted, or lost what it knew of this epoch: reconcile again.
			ex.RequestReconcile()
		}
	}
	d.executorHealth = ex.Health
	sender := &journal.Sender{
		DB: db, Reporter: c, Journal: jrn, Clock: d.clock, Log: d.log, OnView: ex.ConfirmView,
		// A fence is settled by a reconciliation: one is asked for at once, not at the next restart.
		OnFence: func(state.Key) { ex.RequestReconcile() },
	}

	g, gctx := errgroup.WithContext(ctx)
	g.Go(func() error { return d.loop(gctx) })
	g.Go(func() error { return ex.Run(gctx) })
	g.Go(func() error { return sender.Run(gctx) })
	shipper := outputship.New(outputship.Options{
		DB: db, Uploader: c, Paths: o.Paths, Clock: d.clock, Log: d.log, Interval: o.OutputInterval,
	})
	g.Go(func() error { return shipper.Run(gctx) })
	return g.Wait()
}

// selfIdentity is how the daemon checks that a wrapper of this machine can
// identify itself; tests replace it.
var selfIdentity = procident.StartOf

// runtimeAndHarnesses are what this Worker can run Runs with. The harness
// `command` needs a Runtime; without one the Worker offers none.
func (d *daemon) runtimeAndHarnesses(ctx context.Context, o Options) (cfruntime.Runtime, []harness.Launcher) {
	if _, err := selfIdentity(os.Getpid()); err != nil {
		// The wrapper has to identify itself, or it starts nothing: no harness is offered.
		d.log.WarnContext(ctx, "this process cannot be identified: this Worker offers no harness and will reject the Runs it is given", "err", err)
		return unavailable{}, nil
	}
	rt := o.Runtime
	if rt == nil {
		t, err := tmux.New(tmux.Options{Socket: tmux.SocketName(o.Paths.WorkerDataDir())})
		if err == nil {
			err = t.Check(ctx)
		}
		if err != nil {
			d.log.WarnContext(ctx, "no tmux: this Worker offers no harness and will reject the Runs it is given", "err", err)
			return unavailable{}, nil
		}
		rt = t
	}
	return rt, []harness.Launcher{command.Launcher{}}
}

type daemon struct {
	client   *client.Client
	dataDir  string
	epoch    string
	interval time.Duration
	version  string
	log      *slog.Logger
	measure  func(dir string) (sysinfo.Info, error)
	clock    clock.Clock

	capabilities   []string
	sshTarget      string            // what clients attach through; "" announces none
	defaultSSH     bool              // announce <user>@<hostname>
	attachBinaries map[string]string // runtime name to the absolute path that attaches
	onResponse     func(v1.HeartbeatResponse)
	executorHealth func() v1.Health

	lastMeasured    sysinfo.Info
	hasLastMeasured bool
}

func (d *daemon) loop(ctx context.Context) error {
	var (
		failures int
		state    domain.WorkerState
	)
	d.log.InfoContext(ctx, "worker daemon started", "epoch", d.epoch, "version", d.version, "controller", d.client.Controller())
	for {
		wait := d.interval
		resp, err := d.beat(ctx)
		switch {
		case ctx.Err() != nil:
			return nil
		case err == nil:
			failures = 0
			if d.onResponse != nil {
				d.onResponse(resp)
			}
			if resp.State != state {
				d.log.InfoContext(ctx, "worker state", "state", resp.State)
				state = resp.State
			}
		case retryable(err):
			failures++
			wait = d.backoff(failures)
			d.log.WarnContext(ctx, "heartbeat failed; retrying", "err", err, "in", wait)
		default:
			return err
		}
		if !d.sleep(ctx, wait) {
			return nil
		}
	}
}

func (d *daemon) beat(ctx context.Context) (v1.HeartbeatResponse, error) {
	info, health, err := d.measureMachine(ctx)
	if err != nil {
		return v1.HeartbeatResponse{}, err
	}
	if d.executorHealth != nil && health.Status == v1.HealthOK {
		health = d.executorHealth()
	}
	return d.client.Heartbeat(ctx, v1.HeartbeatRequest{
		Epoch: d.epoch, DaemonVersion: d.version, Hostname: info.Hostname, OS: info.OS, Arch: info.Arch,
		Capabilities: d.capabilities,
		Total:        v1.Capacity{CPU: info.CPU, MemoryMB: info.MemoryMB, DiskMB: info.DiskMB},
		Health:       health,
		SSHTarget:    d.announcedSSHTarget(info.Hostname), AttachBinaries: d.attachBinaries,
	})
}

// announcedSSHTarget is the target the heartbeat carries. The default is the
// user of the daemon at the hostname the machine reports, which is a guess that
// other machines may not resolve: `cf worker show` lets a person see it, and a
// client can override it. It is only announced if it is a valid target.
func (d *daemon) announcedSSHTarget(hostname string) string {
	if !d.defaultSSH {
		return d.sshTarget
	}
	u, err := user.Current()
	if err != nil {
		return ""
	}
	guess := u.Username + "@" + hostname
	if !domain.ValidSSHTarget(guess) {
		return ""
	}
	return guess
}

// measureMachine is what the machine has and how healthy the daemon says it is.
// If it cannot be measured now, the last measures stand and the health is
// degraded; with no measures yet there is nothing to report, and the beat fails
// as a retryable error.
func (d *daemon) measureMachine(ctx context.Context) (sysinfo.Info, v1.Health, error) {
	const unmeasurable = "cannot measure this machine"
	info, err := d.measure(d.dataDir)
	switch {
	case err == nil:
		d.lastMeasured, d.hasLastMeasured = info, true
		return info, v1.Health{Status: v1.HealthOK}, nil
	case !d.hasLastMeasured:
		d.log.ErrorContext(ctx, unmeasurable, "err", err)
		return sysinfo.Info{}, v1.Health{}, domain.NewError(domain.CodeInternal, unmeasurable, nil)
	}
	d.log.WarnContext(ctx, unmeasurable+"; reporting the last measures", "err", err)
	return d.lastMeasured, v1.Health{Status: v1.HealthDegraded, Message: unmeasurable}, nil
}

// retryable: only failures that a later attempt can fix.
func retryable(err error) bool {
	var coded *domain.Error
	if !errors.As(err, &coded) {
		return false
	}
	return coded.Code == domain.CodeControllerUnreachable || coded.Code == domain.CodeInternal
}

// backoff grows from a tenth of the interval to the interval itself.
func (d *daemon) backoff(failures int) time.Duration {
	delay := d.interval / 10
	for range failures - 1 {
		delay *= 2
		if delay >= d.interval {
			return d.interval
		}
	}
	return delay
}

// sleep waits for wait and reports false if ctx ended first.
func (d *daemon) sleep(ctx context.Context, wait time.Duration) bool {
	timer := d.clock.NewTimer(wait)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C():
		return true
	}
}
