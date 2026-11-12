package daemon

import (
	"cmp"
	"context"
	"crypto/rand"
	"crypto/tls"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"sync"
	"time"

	v1 "github.com/Autarquico/codefabric/internal/api/v1"
	"github.com/Autarquico/codefabric/internal/app"
	"github.com/Autarquico/codefabric/internal/app/checkpoints"
	"github.com/Autarquico/codefabric/internal/app/gc"
	"github.com/Autarquico/codefabric/internal/app/moves"
	"github.com/Autarquico/codefabric/internal/app/runs"
	"github.com/Autarquico/codefabric/internal/app/workers"
	"github.com/Autarquico/codefabric/internal/app/workflows"
	"github.com/Autarquico/codefabric/internal/buildinfo"
	"github.com/Autarquico/codefabric/internal/config"
	"github.com/Autarquico/codefabric/internal/controller/api"
	"github.com/Autarquico/codefabric/internal/controller/blob"
	"github.com/Autarquico/codefabric/internal/controller/pki"
	"github.com/Autarquico/codefabric/internal/controller/relay"
	"github.com/Autarquico/codefabric/internal/controller/store"
	"github.com/Autarquico/codefabric/internal/domain"
	"github.com/Autarquico/codefabric/internal/support/clock"
	"github.com/Autarquico/codefabric/internal/support/slogtee"
)

// Options parameterise Start.
type Options struct {
	Paths  config.Paths
	Clock  clock.Clock  // defaults to clock.Real
	Logger *slog.Logger // defaults to slog.Default()
	// ServerCertCheckEvery is how often the server certificate is checked for
	// renewal; zero means every 12 hours.
	ServerCertCheckEvery time.Duration
	// WorkerOfflineTTL overrides controller.json's worker_offline_ttl; zero
	// takes that, and failing that the default of Appendix A.
	WorkerOfflineTTL time.Duration
	// RunLostTimeout overrides controller.json's run_lost_timeout; zero takes
	// that, and failing that the default of Appendix A.
	RunLostTimeout time.Duration
	// SchedulerTick is how often the scheduler looks at the queue when no
	// event has woken it; zero means scheduler.DefaultTick.
	SchedulerTick time.Duration
	// RelayHookArgv replaces `<this binary> relay hook pre-receive`, for tests.
	RelayHookArgv []string
}

const serverCertCheckEvery = 12 * time.Hour

// Controller is a running Controller process.
type Controller struct {
	addr net.Addr
	done chan struct{}
	err  error
}

// Addr is the address the API listens on.
func (c *Controller) Addr() net.Addr { return c.addr }

// Wait blocks until the Controller has stopped and releases its resources.
func (c *Controller) Wait() error {
	<-c.done
	return c.err
}

func newAuthority(db *store.DB, ca *pki.CA, clk clock.Clock, host, port string) *pki.Authority {
	return pki.NewAuthority(pki.AuthorityConfig{
		DB: db, CA: ca, Clock: clk, IDs: domain.NewIDGenerator(clk.Now, rand.Reader),
		CertTTL: domain.Defaults().IdentityCertTTL, Host: host, Port: port,
		EnrollWorker: workers.Enroll,
	})
}

// openStore is store.Open; a test sees through it what the store is given.
var openStore = store.Open

// Start opens the database, loads the PKI and serves the API until ctx ends.
func Start(ctx context.Context, o Options) (*Controller, error) {
	if o.Clock == nil {
		o.Clock = clock.Real{}
	}
	if o.ServerCertCheckEvery == 0 {
		o.ServerCertCheckEvery = serverCertCheckEvery
	}
	if o.Logger == nil {
		o.Logger = slog.Default()
	}
	// The store and the writer of the structured logs log through fallback, which
	// is not teed: what they say about storing a line is not another line to store.
	fallback := o.Logger
	tee := slogtee.New(slogtee.Options{MaxMessageBytes: v1.LogMessageMaxBytes, MaxAttrsBytes: v1.LogAttrsMaxBytes, Clock: o.Clock})
	o.Logger = slog.New(tee.Wrap(fallback.Handler()))
	p := o.Paths
	cfg, err := config.LoadController(p)
	if err != nil {
		return nil, err
	}
	host, port, err := net.SplitHostPort(cfg.Advertise)
	if err != nil {
		return nil, fmt.Errorf("controller.json: advertise: %w", err)
	}
	ca, err := pki.LoadCA(p.PKIDir())
	if err != nil {
		return nil, err
	}
	serverCert, err := ca.LoadServerCert(p.PKIDir(), host, o.Clock.Now())
	if err != nil {
		return nil, err
	}
	blobs, err := blob.New(p.DataDir, o.Clock)
	if err != nil {
		return nil, fmt.Errorf("opening the blob store: %w", err)
	}
	db, err := openStore(ctx, p.DatabaseFile(), store.Options{Clock: o.Clock, Logger: fallback, Blobs: blobs})
	if err != nil {
		return nil, err
	}
	ln, err := new(net.ListenConfig).Listen(ctx, "tcp", cfg.Listen)
	if err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("listening on %s: %w", cfg.Listen, err)
	}

	// A machine without a usable git still runs the Controller: the relay is off.
	var (
		gitRelay *relay.Relay
		runOpts  []runs.Option
	)
	if gitRelay, err = relay.Open(ctx, relay.Config{
		Dir: p.RelayDir(), HookArgv: o.RelayHookArgv, MaxPushBytes: cfg.RelayMaxPushBytes, Log: o.Logger,
	}); err != nil {
		o.Logger.ErrorContext(ctx, "the git relay is disabled: Workers cannot publish results", "err", err)
		gitRelay = nil
	} else {
		runOpts = append(runOpts, runs.WithRelay(gitRelay))
	}

	build := buildinfo.Get()
	offlineTTL := cmp.Or(o.WorkerOfflineTTL, time.Duration(cfg.WorkerOfflineTTL))
	startedAt := o.Clock.Now()
	services := app.New(db, o.Clock, domain.NewIDGenerator(o.Clock.Now, rand.Reader), app.Config{
		Workers:        workers.Config{ControllerVersion: build.Version, OfflineTTL: offlineTTL, StartedAt: startedAt},
		RunLostTimeout: cmp.Or(o.RunLostTimeout, time.Duration(cfg.RunLostTimeout)),
		Blobs:          blobs, MaxBlobBytes: workflows.DefaultMaxBlobBytes,
		Relay: checkpointRelay(gitRelay), History: checkpointHistory(gitRelay), CheckpointExclude: cfg.Checkpoint.Excluded(),
	}, runOpts...)
	services.GC = gc.New(db, o.Clock, blobs, gcPolicy(cfg),
		gcOptions(p, gitRelay, services.Moves)...)
	stopLogs := runLogs(ctx, tee, services, fallback)
	// §27.1: the stops that are owed are resumed before anything is served.
	if err := services.Reconciliation.Startup(ctx, o.Logger); err != nil {
		_ = ln.Close()
		stopLogs()
		_ = db.Close()
		return nil, err
	}
	srv, err := api.New(api.Config{
		Authority: newAuthority(db, ca, o.Clock, host, port), CA: ca, ServerCert: serverCert,
		Store: db, Work: services, Relay: gitRelay,
		Clock: o.Clock, StartedAt: startedAt, Build: build, Logger: o.Logger,
	})
	if err != nil {
		_ = ln.Close()
		stopLogs()
		_ = db.Close()
		return nil, err
	}

	c := &Controller{addr: ln.Addr(), done: make(chan struct{})}
	go func() {
		defer close(c.done)
		runCtx, stop := context.WithCancel(ctx)
		var background sync.WaitGroup
		background.Go(func() {
			renewServerCert(runCtx, o.Clock, o.ServerCertCheckEvery, func() (tls.Certificate, error) {
				return ca.LoadServerCert(p.PKIDir(), host, o.Clock.Now())
			}, srv.SetServerCert, o.Logger)
		})
		background.Go(func() { services.Workers.RunSweeper(runCtx, o.Logger) })
		background.Go(func() { services.GC.RunLoop(runCtx, o.Logger) })
		background.Go(func() { services.Reconciliation.RunGate(runCtx, o.Logger) })
		background.Go(func() { services.Reconciliation.RunLostSweeper(runCtx, o.Logger) })
		background.Go(func() { services.Moves.Run(runCtx, services.Reconciliation.Ready(), o.Logger) })
		background.Go(func() {
			services.Scheduler.RunAfter(runCtx, services.Reconciliation.Ready(), o.SchedulerTick, o.Logger)
		})
		serveErr := srv.Serve(ctx, ln)
		stop()
		background.Wait()
		stopLogs()
		c.err = errors.Join(serveErr, db.Close())
	}()
	return c, nil
}

// gcPolicy is what controller.json says about retention and the GC; what it
// leaves unset takes the defaults of the spec.
func gcPolicy(cfg config.Controller) gc.Policy {
	return gc.Policy{
		System: time.Duration(cfg.Retention.System), Events: time.Duration(cfg.Retention.Events),
		RunLogs: time.Duration(cfg.Retention.RunLogs), RawOutput: time.Duration(cfg.Retention.RawOutput),
		BlobGrace: time.Duration(cfg.GC.BlobGrace), BatchSize: cfg.GC.BatchSize,
		Interval: time.Duration(cfg.GC.Interval), MaxStorage: cfg.MaxStorage,
		RelayBranches:   time.Duration(cfg.Retention.RelayBranches),
		AutoCheckpoints: cfg.GC.CheckpointIntervalKeep, CheckpointReservation: time.Duration(cfg.GC.CheckpointReservationTTL),
	}
}

// gcOptions wires the GC to the disk and, when there is one, to the relay: with
// no relay (no usable git) the pass leaves refs alone.
func gcOptions(p config.Paths, gitRelay *relay.Relay, moves *moves.Service) []gc.Option {
	// A move that is not over keeps the checkpoint it continues from.
	opts := []gc.Option{
		gc.WithLockFile(p.GCLockFile()), gc.WithDisk(p.DatabaseFile(), p.RelayDir()), gc.WithCheckpointRoots(moves.Root),
	}
	if gitRelay != nil {
		opts = append(opts, gc.WithRelay(gitRelay))
	}
	return opts
}

// runLogs writes the structured logs of the Controller into its store until
// the returned stop is called, which writes what is left and waits: it is
// called before the store is closed, after everything that logs has stopped.
func runLogs(ctx context.Context, tee *slogtee.Tee, services *app.Services, fallback *slog.Logger) (stop func()) {
	logCtx, cancel := context.WithCancel(context.WithoutCancel(ctx))
	done := make(chan struct{})
	go func() {
		defer close(done)
		tee.Run(logCtx, services.Logs.AppendController, fallback)
	}()
	return func() { cancel(); <-done }
}

// renewServerCert asks load for the server certificate every interval and
// publishes it. load reissues the certificate once it has under 30 days left
// (pki.CA.LoadServerCert), so a long-running Controller never serves an
// expired one. A failure is logged and retried at the next tick.
func renewServerCert(ctx context.Context, clk clock.Clock, every time.Duration, load func() (tls.Certificate, error), publish func(tls.Certificate), log *slog.Logger) {
	ticker := clk.NewTicker(every)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C():
			cert, err := load()
			if err != nil {
				log.ErrorContext(ctx, "renewing the server certificate failed", "err", err)
				continue
			}
			publish(cert)
		}
	}
}

// checkpointHistory is the relay as the summary of a checkpoint reads commits
// from it: nil, not a typed nil, when the daemon has none.
func checkpointHistory(r *relay.Relay) checkpoints.History {
	if r == nil {
		return nil
	}
	return relayHistory{r}
}

type relayHistory struct{ r *relay.Relay }

func (h relayHistory) Log(ctx context.Context, project domain.ID, base, tip string, limit int) ([]checkpoints.Commit, bool, error) {
	commits, more, err := h.r.Log(ctx, project, base, tip, limit)
	out := make([]checkpoints.Commit, len(commits))
	for i, c := range commits {
		out[i] = checkpoints.Commit(c)
	}
	return out, more, err
}

// checkpointRelay is the relay as the capture sees it: nil, not a typed nil,
// when the daemon has none.
func checkpointRelay(r *relay.Relay) checkpoints.Relay {
	if r == nil {
		return nil
	}
	return r
}
