package daemon

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"os/user"
	"slices"
	"time"

	"github.com/oklog/ulid/v2"
	"golang.org/x/sync/errgroup"

	cfruntime "github.com/Autarquico/codefabric/internal/adapters/runtime"

	"github.com/Autarquico/codefabric/internal/adapters/harness"
	"github.com/Autarquico/codefabric/internal/adapters/harness/claude"
	"github.com/Autarquico/codefabric/internal/adapters/harness/codex"
	"github.com/Autarquico/codefabric/internal/adapters/harness/command"
	"github.com/Autarquico/codefabric/internal/adapters/harness/localhost"
	"github.com/Autarquico/codefabric/internal/adapters/runtime/tmux"
	"github.com/Autarquico/codefabric/internal/adapters/secrets"
	v1 "github.com/Autarquico/codefabric/internal/api/v1"
	"github.com/Autarquico/codefabric/internal/buildinfo"
	"github.com/Autarquico/codefabric/internal/client"
	"github.com/Autarquico/codefabric/internal/config"
	"github.com/Autarquico/codefabric/internal/domain"
	"github.com/Autarquico/codefabric/internal/support/clock"
	"github.com/Autarquico/codefabric/internal/support/envutil"
	"github.com/Autarquico/codefabric/internal/support/redact"
	"github.com/Autarquico/codefabric/internal/support/slogtee"
	"github.com/Autarquico/codefabric/internal/worker/applier"
	"github.com/Autarquico/codefabric/internal/worker/authsync"
	"github.com/Autarquico/codefabric/internal/worker/doctor"
	"github.com/Autarquico/codefabric/internal/worker/executor"
	"github.com/Autarquico/codefabric/internal/worker/gitops"
	"github.com/Autarquico/codefabric/internal/worker/host"
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
	// RedactPipeArgv replaces `<this binary> worker redact-pipe`, for tests.
	RedactPipeArgv []string
	// AskpassArgv replaces `<this binary> worker git-askpass`, for tests.
	AskpassArgv []string
	// PermissionServerArgv replaces `<this binary> worker mcp-bridge`, for tests.
	PermissionServerArgv []string
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
	// Harnesses replace the adapters this Worker offers, for tests; nil is
	// the default set, which is `command`.
	Harnesses []harness.Adapter
	// DiskMinFree is the free disk below which this Worker fails (20% or 50GiB);
	// empty reads the Worker config, then 20%.
	DiskMinFree string
	// Secrets replaces the store of the secrets of this Worker (the catalog
	// with the OS keyring), for tests.
	Secrets secrets.Store
	// DoctorEvery replaces the minute between runs of the doctor, for tests.
	DoctorEvery time.Duration
}

// runtimeModes are the modes the Runtime runs a harness in: tmux gives an
// interactive one a terminal and a headless one a log.
var runtimeModes = []domain.Mode{domain.ModeHeadless, domain.ModeInteractive}

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
	logJournal, logSecrets, stopLogs := d.teeLogs(ctx, db, c)
	defer stopLogs() // before the database closes: what is left is written to it
	if d.generation, err = db.StartGeneration(ctx); err != nil {
		return err
	}
	d.stateDB, d.secrets = db, o.Secrets
	if d.secrets == nil {
		d.secrets = newSecretStore(o.Paths, d.clock)
	}
	jrn := journal.New(d.epoch, d.clock)
	// One reading of the config decides what this run of the daemon offers, and
	// is what a later change is compared with (restartNeeded): the wizard may
	// write it at any time.
	if d.settings, err = doctor.LoadSettings(o.Paths); err != nil {
		d.log.WarnContext(ctx, "the Worker config cannot be read: every harness is offered", "err", err)
		d.settings = doctor.Settings{}
	}
	rt, adapters := d.runtimeAndHarnesses(ctx, o)
	eo := executor.Options{
		DB: db, Controller: c, Workflows: c, Runtime: rt, Journal: jrn, Paths: o.Paths, Clock: d.clock, Log: d.log,
		WrapperArgv: o.WrapperArgv, RedactPipeArgv: o.RedactPipeArgv, StopGrace: o.StopGrace, Secrets: d.secrets,
		PermissionServerArgv: o.PermissionServerArgv,
		LogSecrets:           logSecrets,
		WorktreeRetention:    time.Duration(d.settings.Retention.Worktrees),
	}
	eo.AskpassScript = d.writeAskpassScript(ctx, o)
	broker := authsync.New(authsync.Options{Clock: d.clock, Log: d.log})
	eo.AuthSync = broker
	// Publishing the result of a Run needs git: without it this Worker offers no harness.
	if git, gitErr := gitops.New(ctx, gitops.Options{Paths: o.Paths, Clock: d.clock, Log: d.log, GitPath: o.GitPath}); gitErr != nil {
		d.log.WarnContext(ctx, "git is not usable: this Worker offers no harness and will reject the Runs it is given", "err", gitErr)
		eo.Runtime, adapters = unavailable{}, nil
	} else {
		files := c.TLSFiles()
		eo.Git, eo.RelayTLS = git, executor.RelayTLS{Cert: files.Cert, Key: files.Key, CA: files.CA}
	}
	if len(adapters) > 0 {
		if eo.Host, err = d.host(); err != nil {
			d.log.WarnContext(ctx, "the host of the harnesses cannot be built: this Worker offers no harness and will reject the Runs it is given", "err", err)
			eo.Runtime, adapters = unavailable{}, nil
		}
	}
	if len(adapters) > 0 {
		if eo.Harnesses, eo.Withheld, d.capabilities, err = d.announce(ctx, eo.Host, adapters); err != nil {
			return err
		}
		// Here git is usable (gitops.New checked its version) and the Worker offers harnesses.
		d.capabilities = withOriginAskpass(d.capabilities, eo.AskpassScript)
		d.capabilities, eo.StructuredInput = d.withStructuredInput(ctx, d.capabilities, eo.Harnesses, o.PermissionServerArgv)
	}
	if err := d.startDoctor(ctx, o); err != nil {
		return err
	}
	ap := d.applier(ctx, o, eo.Host, c)
	eo.GlobalConfig = ap
	ex, err := newExecutor(eo)
	if err != nil {
		return err
	}
	ap.WatchRuns(ex)
	if p, ok := eo.Runtime.(cfruntime.AttachBinaryProvider); ok {
		if name, path := p.AttachBinary(); name != "" {
			d.attachBinaries = map[string]string{name: path}
		}
	}
	g, gctx := errgroup.WithContext(ctx)
	takeApplies := d.takeApplies(gctx, g, ap)
	d.onResponse = func(r v1.HeartbeatResponse) {
		takeApplies(r)
		// A heartbeat taken is this daemon held as the current one: it may ask for grants.
		ex.HeartbeatTaken()
		d.ackJournal(gctx, r.JournalAck)
		ex.SetLimits(executor.Limits{MaxConcurrentRuns: r.MaxConcurrentRuns, CPU: r.Allocatable.CPU, MemoryMB: r.Allocatable.MemoryMB})
		if r.ReconcileRequired && ex.Reconciled() {
			// The Controller restarted, or lost what it knew of this epoch: reconcile again.
			ex.RequestReconcile()
		}
	}
	d.executorHealth = ex.Health
	shipper := outputship.New(outputship.Options{
		DB: db, Uploader: c, Paths: o.Paths, Clock: d.clock, Log: d.log, Interval: o.OutputInterval,
	})
	maxSize, hardMax, limitsErr := doctor.JournalLimits(o.Paths)
	if limitsErr != nil {
		d.log.WarnContext(ctx, "the journal sizes of the Worker config are not used: the defaults apply", "err", limitsErr)
	}
	degrader := journal.NewDegrader(journal.DegradeOptions{
		DB: db, Output: shipper, Clock: d.clock, Log: d.log, Limits: journal.Limits{MaxSize: maxSize, HardMax: hardMax},
		OnDegraded: ex.SetJournalDegraded,
	})
	d.journalBacklog = degrader.Backlog
	sender := &journal.Sender{
		DB: db, Reporter: c, Journal: jrn, Clock: d.clock, Log: d.log, OnView: ex.ConfirmView,
		// A fence is settled by a reconciliation: one is asked for at once, not at the next restart.
		OnFence: func(state.Key) { ex.RequestReconcile() },
	}

	g.Go(func() error { return d.loop(gctx) })
	g.Go(func() error { return ex.Run(gctx) })
	g.Go(func() error { return broker.Run(gctx) })
	g.Go(func() error { return sender.Run(gctx) })
	g.Go(func() error { return logJournal.Run(gctx) })
	g.Go(func() error { return shipper.Run(gctx) })
	g.Go(func() error { return degrader.Run(gctx) })
	return g.Wait()
}

// teeLogs keeps a structured copy of what the daemon logs from here on in the
// log journal of state.db, which sends it to the Controller (§34); stderr
// still gets every line. The journal, and the writer of the tee, log only to
// the logger the daemon was given: what they say about a line is not another
// line. stop writes what is left and waits; it runs before db is closed.
//
// The daemon does hold secret values: the executor reads them from the
// keyring to deliver them to a Run (secretjit) and to render its
// configuration, and reads the credential of an origin for git-askpass. So
// before a line is stored it is redacted of every value a Run of this daemon
// retains: the executor holds them in held from the delivery until the
// release (executor.Options.LogSecrets), and again for a Run a previous daemon
// delivered to, once it touches it. Then come redact.Text and the keys that
// name secrets. This does not replace the scrubbing at each site (e.scrub on
// the errors of a prepare, OriginAuth.Needles on the output of git): stderr
// gets the line as it is. A value read from the keyring and not yet delivered
// is in no set: the paths between the read and the delivery log refs only.
func (d *daemon) teeLogs(ctx context.Context, db *state.DB, c *client.Client) (logs *journal.Logs, held *redact.Live, stop func()) {
	fallback := d.log
	held = &redact.Live{}
	tee := slogtee.New(slogtee.Options{MaxMessageBytes: v1.LogMessageMaxBytes, MaxAttrsBytes: v1.LogAttrsMaxBytes, Redact: held.Text, Clock: d.clock})
	d.log = slog.New(tee.Wrap(fallback.Handler()))
	logs = journal.NewLogs(journal.LogsOptions{DB: db, Epoch: d.epoch, Shipper: c, Clock: d.clock, Log: fallback})
	logCtx, cancel := context.WithCancel(context.WithoutCancel(ctx))
	done := make(chan struct{})
	go func() {
		defer close(done)
		tee.Run(logCtx, logs.Store, fallback)
	}()
	return logs, held, func() { cancel(); <-done }
}

// orderTaker is the applier as the daemon sees it.
type orderTaker interface {
	Handle(ctx context.Context, orders []v1.WorkflowApplyOrder)
	Wait()
}

// takeApplies hands the orders of a heartbeat to ap, under the context of the
// group: when another component fails the applies in flight are cancelled with
// it, and the daemon does not wait for them forever.
func (d *daemon) takeApplies(gctx context.Context, g *errgroup.Group, ap orderTaker) func(v1.HeartbeatResponse) {
	g.Go(func() error { <-gctx.Done(); ap.Wait(); return nil })
	return func(r v1.HeartbeatResponse) { ap.Handle(gctx, r.WorkflowApplies) }
}

// applier is what applies the Workflows the Controller orders to the global
// configuration of the harnesses. It does not need a Runtime: a Worker that
// offers no harness to the Runs still keeps its configuration. Creating it
// recovers an apply a crash cut off, before any order is taken.
func (d *daemon) applier(ctx context.Context, o Options, h harness.Host, c *client.Client) *applier.Applier {
	if h == nil {
		var err error
		if h, err = d.host(); err != nil {
			d.log.WarnContext(ctx, "the host of the harnesses cannot be built: this Worker applies no Workflow", "err", err)
		}
	}
	adapters := o.Harnesses
	if adapters == nil {
		adapters = []harness.Adapter{codex.Adapter{}, claude.Adapter{}}
	}
	adapters = d.settings.Offered(adapters)
	if h == nil {
		adapters = nil
	}
	return applier.New(applier.Options{
		Client: c, Adapters: adapters, Host: h, DataDir: o.Paths.WorkerDataDir(), Secrets: d.secrets, Log: d.log, Clock: d.clock,
	})
}

// selfIdentity is how the daemon checks that a wrapper of this machine can
// identify itself; tests replace it.
var selfIdentity = procident.StartOf

// runtimeAndHarnesses are what this Worker can run Runs with: the Runtime and
// the adapters it may offer. A harness needs a Runtime; without one the
// Worker offers none.
func (d *daemon) runtimeAndHarnesses(ctx context.Context, o Options) (cfruntime.Runtime, []harness.Adapter) {
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
	adapters := o.Harnesses
	if adapters == nil {
		adapters = []harness.Adapter{command.Adapter{}, codex.Adapter{}, claude.Adapter{}}
	}
	return rt, d.settings.Offered(adapters)
}

// host is the machine the harnesses run on: this one, with the home and the
// environment of the daemon, without the tmux it may run in, as the harness
// itself has it.
func (d *daemon) host() (harness.Host, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil, err
	}
	return localhost.New(localhost.Options{Home: home, Env: envutil.WithoutTmux(os.Environ()), Clock: d.clock})
}

// announce detects the adapters and says which this Worker offers. The
// executor gets all of them, so that a Run it accepted before a restart runs
// with its adapter even if detecting it failed now; it withholds the rest
// from new assignments. The capabilities announce the offered ones with their
// modes.
func (d *daemon) announce(ctx context.Context, h harness.Host, adapters []harness.Adapter) (reg *harness.Registry, withheld, capabilities []string, err error) {
	if reg, err = harness.NewRegistry(adapters...); err != nil {
		return nil, nil, nil, err
	}
	ann := host.Announce(ctx, h, reg, runtimeModes)
	for _, found := range ann.Harnesses {
		attrs := []any{"harness", found.ID, "status", found.Status, "modes", found.Modes}
		switch found.Status {
		case host.Offered:
			d.log.InfoContext(ctx, "harness offered", append(attrs, "path", found.Path, "structured_input", found.StructuredInput)...)
			if found.InputErr != nil {
				d.log.WarnContext(ctx, "harness offered without structured input: its Runs that ask permissions are refused", "harness", found.ID, "err", found.InputErr)
			}
			continue
		case host.DetectFailed:
			d.log.WarnContext(ctx, "harness not offered: detecting it failed", append(attrs, "err", found.Err)...)
		default:
			d.log.InfoContext(ctx, "harness not offered", attrs...)
		}
		withheld = append(withheld, found.ID)
	}
	return reg, withheld, ann.Capabilities, nil
}

type daemon struct {
	// settings is the Worker config as this run of the daemon read it at start.
	settings doctor.Settings
	client   *client.Client
	dataDir  string
	epoch    string
	interval time.Duration
	version  string
	log      *slog.Logger
	measure  func(dir string) (sysinfo.Info, error)
	clock    clock.Clock

	// generation orders this run of the daemon among those of the Worker,
	// and seq its heartbeats (see the fence in app/workers).
	generation uint64
	seq        uint64
	stateDB    *state.DB
	secrets    secrets.Store
	health     secretHealth // the last Check of the keyring entries

	doctor      *doctor.Doctor // nil: the machine is not checked
	doctorEvery time.Duration
	doctorAt    time.Time
	components  []v1.ComponentHealth

	capabilities   []string
	sshTarget      string            // what clients attach through; "" announces none
	defaultSSH     bool              // announce <user>@<hostname>
	attachBinaries map[string]string // runtime name to the absolute path that attaches
	onResponse     func(v1.HeartbeatResponse)
	executorHealth func() v1.Health
	journalBacklog func() journal.Backlog // nil: the journal says nothing in the heartbeat

	lastMeasured    sysinfo.Info
	hasLastMeasured bool
}

func (d *daemon) loop(ctx context.Context) error {
	var (
		failures int
		state    domain.WorkerState
	)
	d.log.InfoContext(ctx, "worker daemon started", "epoch", d.epoch, "generation", d.generation,
		"version", d.version, "controller", d.client.Controller())
	for {
		wait := d.interval
		resp, err := d.beat(ctx)
		var again bool
		if err != nil && ctx.Err() == nil {
			again, err = d.answerFence(ctx, err)
		}
		switch {
		case ctx.Err() != nil:
			return nil
		case again:
			wait = d.backoff(1)
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
	req, err := d.heartbeat(ctx)
	if err != nil {
		return v1.HeartbeatResponse{}, err
	}
	return d.client.Heartbeat(ctx, req)
}

// heartbeat is the next heartbeat: what the machine has, the health of the
// daemon, and the metadata of the secrets it holds, under the next seq.
func (d *daemon) heartbeat(ctx context.Context) (v1.HeartbeatRequest, error) {
	info, health, err := d.measureMachine(ctx)
	if err != nil {
		return v1.HeartbeatRequest{}, err
	}
	if d.executorHealth != nil && health.Status == v1.HealthOK {
		health = d.executorHealth()
	}
	report, problem := d.secretsReport(ctx)
	if problem != "" && health.Status == v1.HealthOK {
		health = v1.Health{Status: v1.HealthDegraded, Message: problem}
	}
	health = d.withComponents(ctx, health)
	jr, health := d.journalStatus(ctx, health)
	return v1.HeartbeatRequest{
		Epoch: d.epoch, Generation: d.generation, DaemonVersion: d.version,
		Hostname: info.Hostname, OS: info.OS, Arch: info.Arch,
		Capabilities: d.capabilities,
		Total:        v1.Capacity{CPU: info.CPU, MemoryMB: info.MemoryMB, DiskMB: info.DiskMB},
		Health:       health,
		SSHTarget:    d.announcedSSHTarget(info.Hostname), AttachBinaries: d.attachBinaries,
		Secrets: &report, Journal: jr,
	}, nil
}

// journalStatus is what the journal tells in the heartbeat: its backlog and the
// truncations the Controller has not acknowledged. A journal over max_size also
// makes the "journal" component fail, which keeps new Runs off the Worker.
func (d *daemon) journalStatus(ctx context.Context, health v1.Health) (*v1.JournalReport, v1.Health) {
	if d.journalBacklog == nil || d.stateDB == nil {
		return nil, health
	}
	b := d.journalBacklog()
	report := &v1.JournalReport{BacklogBytes: b.Bytes}
	truncations, err := state.SealedTruncations(ctx, d.stateDB.Reader(), v1.MaxJournalTruncations)
	if err != nil {
		d.log.WarnContext(ctx, "the truncations of the journal cannot be read", "err", err)
	}
	for _, t := range truncations {
		report.Truncations = append(report.Truncations, v1.JournalTruncation{
			ID: t.ID, At: t.At, LogsFromSeq: t.LogsFrom, LogsToSeq: t.LogsTo, LogLines: t.LogLines, OutputBytes: t.OutputBytes, RunIDs: t.RunIDs,
		})
	}
	if b.Reason != "" {
		health.Components = append(slices.Clone(health.Components), doctor.CleanComponent(v1.ComponentHealth{
			Name: journalComponent, Status: v1.ComponentFail, Message: b.Reason,
			Action: "restore the connection to the Controller or free the disk of the Worker",
		}))
		if health.Status == v1.HealthOK {
			health.Status, health.Message = v1.HealthDegraded, b.Reason
		}
	}
	return report, health
}

// ackJournal forgets the truncations the Controller says it has.
func (d *daemon) ackJournal(ctx context.Context, upTo uint64) {
	if upTo == 0 || d.stateDB == nil {
		return
	}
	if err := d.stateDB.WithTx(ctx, func(tx *state.Tx) error { return state.AckTruncations(ctx, tx, upTo) }); err != nil && ctx.Err() == nil {
		d.log.WarnContext(ctx, "forgetting the truncations the Controller has failed", "err", err)
	}
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

// withOriginAskpass adds domain.OriginAskpassCapability to caps when the
// askpass script exists: only then can this Worker fetch the origin of a Run
// with its credential, and placement relies on the capability saying so.
func withOriginAskpass(caps []string, script string) []string {
	if script == "" {
		return caps
	}
	return slices.Sorted(slices.Values(append(slices.Clone(caps), domain.OriginAskpassCapability)))
}

// writeAskpassScript keeps the GIT_ASKPASS script of the Worker (AskpassScriptPath)
// up to date. Without it a Run whose origin has a credential cannot be prepared,
// which is said when that Run is, not by stopping the daemon.
func (d *daemon) writeAskpassScript(ctx context.Context, o Options) string {
	argv := o.AskpassArgv
	if len(argv) == 0 {
		self, err := os.Executable()
		if err != nil {
			d.log.WarnContext(ctx, "the askpass script cannot be written: this binary cannot be located", "err", err)
			return ""
		}
		argv = []string{self, "worker", "git-askpass"}
	}
	path, err := EnsureAskpassScript(o.Paths, argv)
	if err != nil {
		d.log.WarnContext(ctx, "the askpass script cannot be written", "err", err)
		return ""
	}
	return path
}

// newExecutor builds the executor of the daemon; a test replaces it to see the options.
var newExecutor = executor.New
