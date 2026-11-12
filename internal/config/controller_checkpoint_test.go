package config

import (
	"testing"
	"time"
)

func TestPreDestructiveWait(t *testing.T) {
	for _, tt := range []struct {
		set, want time.Duration
	}{
		{0, DefaultPreDestructiveTimeout},
		{-time.Second, DefaultPreDestructiveTimeout},
		{5 * time.Second, 5 * time.Second},
		{20 * time.Minute, MaxPreDestructiveTimeout},
	} {
		if got := (Checkpoint{PreDestructiveTimeout: Duration(tt.set)}).PreDestructiveWait(); got != tt.want {
			t.Errorf("PreDestructiveWait(%s) = %s, want %s", tt.set, got, tt.want)
		}
	}
}

// The wait of a forced drain covers the grace a Worker gives a harness to end
// (30s by default) and the time to publish, which a client timeout used to cut.
func TestPreDestructiveWait_CoversTheCheckpointGrace(t *testing.T) {
	const grace = 30 * time.Second
	if got := (Checkpoint{}).PreDestructiveWait(); got < grace+15*time.Second {
		t.Errorf("the default wait %s does not cover checkpoint.grace (%s) and a margin", got, grace)
	}
}
