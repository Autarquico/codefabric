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
		{10 * time.Minute, MaxPreDestructiveTimeout},
	} {
		if got := (Checkpoint{PreDestructiveTimeout: Duration(tt.set)}).PreDestructiveWait(); got != tt.want {
			t.Errorf("PreDestructiveWait(%s) = %s, want %s", tt.set, got, tt.want)
		}
	}
}
