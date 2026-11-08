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
//  3. What was given up is sealed in a truncation, which the next heartbeat
//     carries to the Controller as JournalTruncated: when a pass gave something
//     up, and at hard_max (at most once every sealEvery).
//
// No report is ever given up: all of them are the state of a Run (the kinds of
// internal/api/v1/reports.go are Run lifecycle). What bounds the journal at
// hard_max is the Worker, which takes no Runs while it is degraded; the reports
// of the Runs it already has are the only growth left. At hard_max the Worker
// says so in its log (error) and in its degraded reason.
type Degrader struct {
	o        DegradeOptions
	mu       sync.Mutex
	last     Backlog
	reason   string
	hard     bool // the reason is of a journal at hard_max
	sealedAt time.Time
	hardAt   time.Time
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

	var shed uint64
	if total >= limits.MaxSize {
		var err error
		if shed, err = d.shed(ctx, total-limits.MaxSize+1); err != nil {
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
	hard := total >= limits.HardMax
	if err := d.seal(ctx); err != nil {
		return err
	}
	if hard {
		d.warnHardMax(ctx, total)
	}
	d.publish(ctx, total, total >= limits.MaxSize, hard)
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
	var (
		open   state.Truncation
		sealed bool
	)
	err := d.o.DB.WithTx(ctx, func(tx *state.Tx) (err error) {
		if open, sealed, err = state.OpenTruncation(ctx, tx); err != nil || !sealed {
			return err
		}
		_, err = state.SealTruncation(ctx, tx)
		return err
	})
	if err == nil && sealed {
		d.mu.Lock()
		d.sealedAt = now
		d.mu.Unlock()
		d.o.Log.WarnContext(ctx, "the journal discarded logs and output the Controller has not confirmed: it is reported to the Controller",
			"log_lines", open.LogLines, "output_bytes", open.OutputBytes)
	}
	return err
}

// warnHardMax says, at most once every sealEvery, that the reports alone have
// filled the journal: none is discarded, so only the Worker taking no Runs bounds it.
func (d *Degrader) warnHardMax(ctx context.Context, total uint64) {
	now := d.o.Clock.Now()
	d.mu.Lock()
	wait := !d.hardAt.IsZero() && now.Sub(d.hardAt) < sealEvery
	if !wait {
		d.hardAt = now
	}
	d.mu.Unlock()
	if !wait {
		d.o.Log.ErrorContext(ctx, "the journal is at hard_max: reports are never discarded, so it grows only with the Runs this Worker already has; no more are taken",
			"bytes", total, "hard_max", d.o.Limits.HardMax)
	}
}

func (d *Degrader) publish(ctx context.Context, total uint64, degraded, hard bool) {
	reason := ""
	switch {
	case hard:
		reason = fmt.Sprintf("the journal holds %d bytes the Controller has not confirmed (hard_max %d): reports are not discarded, so the Worker takes no Runs until they are confirmed", total, d.o.Limits.HardMax)
	case degraded:
		reason = fmt.Sprintf("the journal holds %d bytes the Controller has not confirmed (max_size %d): logs and output are discarded", total, d.o.Limits.MaxSize)
	}
	d.mu.Lock()
	d.last = Backlog{Bytes: total, Reason: reason}
	changed := (reason == "") != (d.reason == "") || hard != d.hard
	d.reason, d.hard = reason, hard
	d.mu.Unlock()
	if changed && d.o.OnDegraded != nil {
		if degraded {
			d.o.Log.WarnContext(ctx, "the journal is over max_size: this Worker is degraded and takes no Runs")
		}
		d.o.OnDegraded(reason)
	}
}
