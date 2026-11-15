package client

import (
	"testing"
	"time"
)

func TestPollSeconds_NeverAsksForNoWait(t *testing.T) {
	for wait, want := range map[time.Duration]int64{
		time.Nanosecond: 1, 500 * time.Millisecond: 1, time.Second: 1, 1500 * time.Millisecond: 2, 20 * time.Second: 20,
	} {
		if got := pollSeconds(wait); got != want {
			t.Errorf("pollSeconds(%s) = %d, want %d", wait, got, want)
		}
	}
}
