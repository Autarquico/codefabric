package daemon

import (
	"context"
	"os"
	"slices"
	"time"

	"github.com/Autarquico/codefabric/internal/adapters/harness"
	"github.com/Autarquico/codefabric/internal/adapters/harness/claude"
	"github.com/Autarquico/codefabric/internal/adapters/harness/codex"
	"github.com/Autarquico/codefabric/internal/adapters/harness/command"
	cfruntime "github.com/Autarquico/codefabric/internal/adapters/runtime"
	v1 "github.com/Autarquico/codefabric/internal/api/v1"
	"github.com/Autarquico/codefabric/internal/support/clock"
	"github.com/Autarquico/codefabric/internal/worker/doctor"
)

// daemonComponent is the component for a degradation that is not a check of the
// doctor (executor, secrets catalog, machine). Like the Controller's own
// "controller" and "version", it concerns every Run.
const daemonComponent = "daemon"

// doctorTimeout bounds one pass of the checks, which start processes.
const doctorTimeout = 15 * time.Second

// withDeadline is ctx cancelled once dur has passed on clk, so that the
// deadline follows the injected clock and not the system's.
func withDeadline(ctx context.Context, clk clock.Clock, dur time.Duration) (context.Context, context.CancelFunc) {
	ctx, cancel := context.WithCancel(ctx)
	t := clk.NewTimer(dur)
	go func() {
		defer t.Stop()
		select {
		case <-t.C():
			cancel()
		case <-ctx.Done():
		}
	}()
	return ctx, cancel
}

// runtimeCheck is the check of a Runtime that is not tmux: its own Check if it
// has one, and healthy if it has none.
func runtimeCheck(rt cfruntime.Runtime) func(context.Context) error {
	checker, ok := rt.(interface{ Check(context.Context) error })
	if !ok {
		return func(context.Context) error { return nil }
	}
	return checker.Check
}

// startDoctor builds the doctor of this machine. A host that cannot be built
// leaves the machine unchecked, as it leaves it without harnesses.
func (d *daemon) startDoctor(ctx context.Context, o Options) error {
	minFree, err := doctor.MinFree(o.Paths, o.DiskMinFree)
	if err != nil {
		return err
	}
	h, err := d.host()
	if err != nil {
		d.log.WarnContext(ctx, "the host cannot be built: this Worker does not check itself", "err", err)
		return nil
	}
	adapters := o.Harnesses
	if adapters == nil {
		adapters = []harness.Adapter{command.Adapter{}, codex.Adapter{}, claude.Adapter{}}
	}
	adapters = doctor.Offered(o.Paths, adapters)
	hostname, _ := os.Hostname()
	opts := doctor.Options{
		Host: h, Adapters: adapters, Worker: doctor.WorkerName(o.Paths, workerName(hostname)), DataDir: o.Paths.WorkerDataDir(),
		Secrets: d.secrets, MinFree: minFree, Git: o.GitPath,
		SetupPending: func() bool { return doctor.SetupPending(o.Paths) },
	}
	// RuntimeIsolated stays false: tmux and the process runtime run the harness on the host.
	if o.Runtime != nil { // the doctor checks the Runtime the daemon uses, not tmux
		opts.Runtime, opts.RuntimeName = runtimeCheck(o.Runtime), "el runtime del daemon"
	}
	d.doctor, d.doctorEvery = doctor.New(opts), o.DoctorEvery
	if d.doctorEvery <= 0 {
		d.doctorEvery = doctor.Every
	}
	return nil
}

// withComponents adds the verdict of the doctor to health, running it if
// doctorEvery has passed since the last time. Any failing component makes the
// Worker degraded, unless it already is for a reason of its own.
func (d *daemon) withComponents(ctx context.Context, health v1.Health) v1.Health {
	if d.doctor == nil {
		return health
	}
	if now := d.clock.Now(); d.components == nil || now.Sub(d.doctorAt) >= d.doctorEvery {
		cctx, cancel := withDeadline(ctx, d.clock, doctorTimeout)
		d.components, d.doctorAt = d.doctor.Check(cctx), now
		cancel()
	}
	health.Components = d.components
	if health.Status != v1.HealthOK {
		// The components must explain the whole degradation, or the scheduler
		// would take the failures of other harnesses for all there is to it.
		health.Components = append(slices.Clone(d.components), doctor.CleanComponent(v1.ComponentHealth{
			Name: daemonComponent, Status: v1.ComponentFail, Message: health.Message,
			Action: "revisa el log del daemon del Worker",
		}))
		return health
	}
	if summary := doctor.Health(d.components); summary.Status != v1.HealthOK {
		health.Status, health.Message = summary.Status, summary.Message
	}
	return health
}
