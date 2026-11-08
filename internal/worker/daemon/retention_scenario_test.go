//go:build linux || darwin

package daemon_test

import (
	"context"
	"log/slog"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Autarquico/codefabric/internal/config"
	"github.com/Autarquico/codefabric/internal/worker/daemon"
	"github.com/Autarquico/codefabric/internal/worker/doctor"
	"github.com/Autarquico/codefabric/internal/worker/executor"
)

func TestRun_GivesTheExecutorTheWorktreeRetentionOfTheSettings(t *testing.T) {
	c := newController(t)
	paths := machine(t)
	c.join("box", paths)
	if err := doctor.SaveSettings(paths, doctor.Settings{Name: "box", Retention: doctor.Retention{Worktrees: config.Duration(36 * time.Hour)}}); err != nil {
		t.Fatal(err)
	}
	var got atomic.Int64
	daemon.WatchExecutorOptions(t, func(o executor.Options) { got.Store(int64(o.WorktreeRetention)) })

	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = daemon.Run(ctx, daemon.Options{Paths: paths, Interval: interval, Logger: slog.New(slog.DiscardHandler), Runtime: &recordingRuntime{}})
	}()
	t.Cleanup(func() { cancel(); <-done })

	deadline := time.Now().Add(10 * time.Second)
	for got.Load() == 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if time.Duration(got.Load()) != 36*time.Hour {
		t.Fatalf("WorktreeRetention = %v, want 36h from the settings", time.Duration(got.Load()))
	}
}
