package journal

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/Autarquico/codefabric/internal/support/clock"
	"github.com/Autarquico/codefabric/internal/worker/state"
)

// Output is the output.log side of the journal: what the Controller has not
// confirmed of it, and the means to give some of it up.
type Output interface {
	// Unsent is how many bytes of output wait to be sent.
	Unsent(ctx context.Context) (uint64, error)
	// Shed skips at least need bytes of what waits (or all of it, if it is less),
	// leaving a gap in the output, and answers how many it skipped.
	Shed(ctx context.Context, need uint64) (uint64, error)
}

// Limits are the sizes of §1.3: journal.max_size and journal.hard_max.
type Limits struct {
	MaxSize uint64
	HardMax uint64
}

// sealEvery bounds how often a truncation is sealed, hence reported: a journal
// that stays over hard_max would otherwise make one event per pass.
const sealEvery = time.Minute

// DegradeOptions parameterise NewDegrader.
type DegradeOptions struct {
	DB     *state.DB
	Output Output
	Clock  clock.Clock
	Log    *slog.Logger
	Limits Limits
	Tick   time.Duration // 0 means a second
	// OnDegraded is told, each time it changes, the reason the journal has the
	// Worker degraded for; "" says it no longer does.
	OnDegraded func(reason string)
	// Compact is where usage events are compacted (stage 2). It answers the
	// bytes it freed. Nil until something in the journal can be compacted:
	// usage-attribution (Phase 6) is what writes usage.
	Compact func(ctx context.Context) (uint64, error)
}

// Backlog is what a pass found, as the heartbeat shows it.
type Backlog struct {
	Bytes uint64 // not yet confirmed by the Controller: reports, logs and output
	// Reason is why the journal has the Worker degraded; "" when it does not.
	Reason string
}

// Degrader keeps the journal within the sizes of §1.3, in three stages by how
// much of it the Controller has not confirmed (the backlog; bytes sent and
// confirmed are not counted):
//
//  1. At max_size, raw output is given up first (its cursors move to the end of
//     the file, leaving a gap), then the oldest log lines.
//  2. If, with no output and no logs left, the reports alone are still at
//     max_size, usage is compacted and the Worker is degraded.
//  3. At hard_max what was given up is sealed in a truncation, which the next
//     heartbeat carries to the Controller as JournalTruncated.
//
// No report is ever given up: each one is the state of a Run, or closes
// something the Controller waits for.
type Degrader struct {
	o        DegradeOptions
	mu       sync.Mutex
	last     Backlog
	reason   string
	sealedAt time.Time
}

// NewDegrader makes the policy.
func NewDegrader(o DegradeOptions) *Degrader {
	if o.Tick <= 0 {
		o.Tick = defaultTick
	}
	return &Degrader{o: o}
}

// Backlog is the outcome of the latest pass.
func (d *Degrader) Backlog() Backlog {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.last
}

// Run applies the policy until ctx ends.
func (d *Degrader) Run(ctx context.Context) error {
	ticker := d.o.Clock.NewTicker(d.o.Tick)
	defer ticker.Stop()
	for {
		if err := d.Once(ctx); err != nil && ctx.Err() == nil {
			d.o.Log.WarnContext(ctx, "keeping the journal within its size failed", "err", err)
		}
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C():
		}
	}
}

// Once is one pass.
func (d *Degrader) Once(ctx context.Context) error {
	backlog, err := state.JournalBacklog(ctx, d.o.DB.Reader())
	if err != nil {
		return err
	}
	output, err := d.o.Output.Unsent(ctx)
	if err != nil {
		return err
	}
	total := uint64(backlog.Reports) + uint64(backlog.Logs) + output //nolint:gosec // sums of lengths
	limits := d.o.Limits

	if total >= limits.MaxSize {
		shed, err := d.shed(ctx, total-limits.MaxSize+1)
		if err != nil {
			return err
		}
		total -= min(shed, total)
	}
	if total >= limits.MaxSize && d.o.Compact != nil {
		freed, err := d.o.Compact(ctx)
		if err != nil {
			return err
		}
		total -= min(freed, total)
	}
	if total >= limits.HardMax {
		if err := d.seal(ctx); err != nil {
			return err
		}
	}
	d.publish(ctx, total, total >= limits.MaxSize)
	return nil
}

// shed gives up at least need bytes: output first, then the oldest logs.
func (d *Degrader) shed(ctx context.Context, need uint64) (uint64, error) {
	shed, err := d.o.Output.Shed(ctx, need)
	if err != nil || shed >= need {
		return shed, err
	}
	var freed int64
	err = d.o.DB.WithTx(ctx, func(tx *state.Tx) (err error) {
		freed, err = state.ShedLogs(ctx, tx, int64(need-shed), d.o.Clock.Now()) //nolint:gosec // below the size of a journal
		return err
	})
	return shed + uint64(freed), err //nolint:gosec // freed is not negative
}

// seal closes the truncation that is open so that it is reported, at most once
// every sealEvery.
func (d *Degrader) seal(ctx context.Context) error {
	now := d.o.Clock.Now()
	d.mu.Lock()
	wait := !d.sealedAt.IsZero() && now.Sub(d.sealedAt) < sealEvery
	d.mu.Unlock()
	if wait {
		return nil
	}
	var sealed bool
	err := d.o.DB.WithTx(ctx, func(tx *state.Tx) (err error) {
		sealed, err = state.SealTruncation(ctx, tx)
		return err
	})
	if err == nil && sealed {
		d.mu.Lock()
		d.sealedAt = now
		d.mu.Unlock()
		d.o.Log.WarnContext(ctx, "the journal reached hard_max: what was given up of logs and output is reported to the Controller")
	}
	return err
}

func (d *Degrader) publish(ctx context.Context, total uint64, degraded bool) {
	reason := ""
	if degraded {
		reason = fmt.Sprintf("the journal holds %d bytes the Controller has not confirmed (max_size %d): logs and output are discarded", total, d.o.Limits.MaxSize)
	}
	d.mu.Lock()
	d.last = Backlog{Bytes: total, Reason: reason}
	changed := (reason == "") != (d.reason == "")
	d.reason = reason
	d.mu.Unlock()
	if changed && d.o.OnDegraded != nil {
		if degraded {
			d.o.Log.WarnContext(ctx, "the journal is over max_size: this Worker is degraded and takes no Runs")
		}
		d.o.OnDegraded(reason)
	}
}
