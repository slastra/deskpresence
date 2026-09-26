package main

import (
	"testing"
	"time"
)

// TestStatusHeartbeat pins that an unchanged status is still rewritten every
// statusHeartbeat, so idle-guard.sh's 15 s staleness check only ever means
// the daemon is gone, never that the desk has been still.
func TestStatusHeartbeat(t *testing.T) {
	t0 := time.Date(2026, 9, 26, 4, 18, 0, 0, time.Local)
	if !statusDue("a", "", time.Time{}, t0) {
		t.Error("the first status must be written")
	}
	if !statusDue("b", "a", t0, t0.Add(time.Second)) {
		t.Error("a change must be written at once")
	}
	if statusDue("a", "a", t0, t0.Add(statusHeartbeat-time.Millisecond)) {
		t.Error("an unchanged status inside the heartbeat is not rewritten")
	}
	if !statusDue("a", "a", t0, t0.Add(statusHeartbeat)) {
		t.Error("an unchanged status is rewritten after the heartbeat")
	}
	if statusHeartbeat*3 > 15*time.Second {
		t.Error("the heartbeat must fit idle-guard's 15 s staleness window three times over")
	}
}
