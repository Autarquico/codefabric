package daemon

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"github.com/oklog/ulid/v2"

	v1 "github.com/Autarquico/codefabric/internal/api/v1"
	"github.com/Autarquico/codefabric/internal/buildinfo"
	"github.com/Autarquico/codefabric/internal/client"
	"github.com/Autarquico/codefabric/internal/config"
	"github.com/Autarquico/codefabric/internal/domain"
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
}

// Run sends a heartbeat now and then every Interval until ctx ends, and returns
// nil when it does. Each Run is a new instance of the daemon: it has an epoch
// of its own, which tells the Controller that the daemon restarted.
//
// A Controller that cannot be reached is retried with a backoff that never
// exceeds the interval, so the Worker is back as soon as the Controller is.
// Anything else (credentials revoked or expired, a Controller that refuses the
// heartbeat) ends Run with that error: retrying would not change it.
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
		interval: o.Interval, version: o.Version, log: o.Logger,
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
	return d.loop(ctx)
}

type daemon struct {
	client   *client.Client
	dataDir  string
	epoch    string
	interval time.Duration
	version  string
	log      *slog.Logger
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
		if !sleep(ctx, wait) {
			return nil
		}
	}
}

func (d *daemon) beat(ctx context.Context) (v1.HeartbeatResponse, error) {
	info, err := sysinfo.Collect(d.dataDir)
	if err != nil {
		return v1.HeartbeatResponse{}, err
	}
	return d.client.Heartbeat(ctx, v1.HeartbeatRequest{
		Epoch: d.epoch, DaemonVersion: d.version, Hostname: info.Hostname, OS: info.OS, Arch: info.Arch,
		Total:  v1.Capacity{CPU: info.CPU, MemoryMB: info.MemoryMB, DiskMB: info.DiskMB},
		Health: v1.Health{Status: v1.HealthOK},
	})
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

// sleep waits for d and reports false if ctx ended first.
func sleep(ctx context.Context, d time.Duration) bool {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}
