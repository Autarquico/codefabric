package domain

import "testing"

func TestLevelAtLeast(t *testing.T) {
	for _, tc := range []struct {
		level, min string
		want       bool
	}{
		{LevelDebug, LevelDebug, true},
		{LevelDebug, LevelInfo, false},
		{LevelInfo, LevelDebug, true},
		{LevelWarn, LevelInfo, true},
		{LevelWarn, LevelError, false},
		{LevelError, LevelError, true},
		{"loud", LevelDebug, false},
		{LevelInfo, "loud", false},
	} {
		if got := LevelAtLeast(tc.level, tc.min); got != tc.want {
			t.Errorf("LevelAtLeast(%q, %q) = %v, want %v", tc.level, tc.min, got, tc.want)
		}
	}
	if !ValidLevel(LevelWarn) || ValidLevel("") {
		t.Error("ValidLevel")
	}
}
