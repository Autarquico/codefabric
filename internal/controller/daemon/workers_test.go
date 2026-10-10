package daemon_test

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	v1 "github.com/Autarquico/codefabric/internal/api/v1"
	"github.com/Autarquico/codefabric/internal/client"
	"github.com/Autarquico/codefabric/internal/config"
	"github.com/Autarquico/codefabric/internal/controller/daemon"
	"github.com/Autarquico/codefabric/internal/domain"
	"github.com/Autarquico/codefabric/internal/support/clock"
)

func heartbeat() v1.HeartbeatRequest {
	return v1.HeartbeatRequest{
		Epoch: "01JA0000000000000000000000", DaemonVersion: "dev", Hostname: "h", OS: "linux", Arch: "amd64",
		Total:  v1.Capacity{CPU: 2, MemoryMB: 2048, DiskMB: 10000},
		Health: v1.Health{Status: v1.HealthOK},
	}
}

// The Controller sweeps its Workers on its own: a Worker that falls silent for
// longer than worker_offline_ttl of controller.json goes offline without anyone
// asking, and comes back with its next heartbeat.
func TestStart_SweepsSilentWorkersWithTheTTLOfTheConfiguration(t *testing.T) {
	p := newPaths(t)
	initController(t, p)
	cfg, err := config.LoadController(p)
	if err != nil {
		t.Fatal(err)
	}
	cfg.WorkerOfflineTTL = config.Duration(time.Second) // the shortest TTL: sweeps every third of a second of the fake clock
	if err := config.SaveController(p, cfg); err != nil {
		t.Fatal(err)
	}
	clk := clock.NewFake(time.Now())
	ctx, cancel := context.WithCancel(t.Context())
	c, err := daemon.Start(ctx, daemon.Options{Paths: p, Clock: clk})
	if err != nil {
		cancel()
		t.Fatal(err)
	}
	t.Cleanup(func() {
		cancel()
		if err := c.Wait(); err != nil {
			t.Errorf("Wait: %v", err)
		}
	})

	owner, err := client.Open(p.CredentialsDir())
	if err != nil {
		t.Fatal(err)
	}
	invite, err := owner.CreateToken(t.Context(), domain.KindWorker, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	workerDir := filepath.Join(t.TempDir(), "credentials")
	if _, err := client.Connect(t.Context(), invite.Invite, "box", workerDir, client.AsWorker(v1.WorkerSettings{})); err != nil {
		t.Fatal(err)
	}
	worker, err := client.Open(workerDir)
	if err != nil {
		t.Fatal(err)
	}
	if resp, err := worker.Heartbeat(t.Context(), heartbeat()); err != nil || resp.State != domain.WorkerOnline {
		t.Fatalf("heartbeat: %+v, %v", resp, err)
	}

	// The sweeper and the certificate renewal are waiting on the clock.
	waitCtx, cancelWait := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancelWait()
	if err := clk.BlockUntil(waitCtx, 2); err != nil {
		t.Fatal(err)
	}
	clk.Advance(2 * time.Second)
	deadline := time.Now().Add(5 * time.Second)
	for {
		w, err := owner.Worker(t.Context(), "box")
		if err != nil {
			t.Fatal(err)
		}
		if w.State == domain.WorkerOffline {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the Controller never swept the silent Worker: %+v", w)
		}
		time.Sleep(10 * time.Millisecond)
	}
	if resp, err := worker.Heartbeat(t.Context(), heartbeat()); err != nil || resp.State != domain.WorkerOnline {
		t.Errorf("heartbeat after the silence: %+v, %v", resp, err)
	}
}
