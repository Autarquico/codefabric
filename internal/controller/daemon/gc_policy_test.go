package daemon_test

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/Autarquico/codefabric/internal/config"
	"github.com/Autarquico/codefabric/internal/controller/daemon"
)

func TestGCPolicy_TakesTheRetentionOfControllerJSON(t *testing.T) {
	var cfg config.Controller
	raw := `{"retention":{"relay_branches":"45d","events":"90d","run_logs":"10d"},"gc":{"batch_size":7,"blob_grace":"2h"},"max_storage":1024}`
	if err := json.Unmarshal([]byte(raw), &cfg); err != nil {
		t.Fatal(err)
	}
	p := daemon.GCPolicy(cfg)
	const day = 24 * time.Hour
	if p.RelayBranches != 45*day || p.Events != 90*day || p.RunLogs != 10*day || p.BatchSize != 7 || p.BlobGrace != 2*time.Hour || p.MaxStorage != 1024 {
		t.Fatalf("policy = %+v", p)
	}
}
