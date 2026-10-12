package main

import "testing"

// The tick is fast only while a fade steps on it. Away (audio held silent
// at 1) and the blackout (fade at 1) must idle at the slow tick: the away
// case alone used to keep it at 4 Hz overnight.
func TestTickPeriod(t *testing.T) {
	for _, tc := range []struct {
		name        string
		fade, audio float64
		want        string
	}{
		{"present, still", 0, 0, "slow"},
		{"screen fading", 0.4, 0.4, "fast"},
		{"audio restoring after a cancelled fade", 0, 0.4, "fast"},
		{"blackout", 1, 1, "slow"},
		{"away, audio held silent", 0, 1, "slow"},
	} {
		got := map[bool]string{true: "fast", false: "slow"}[tickPeriod(tc.fade, tc.audio) == tickFast]
		if got != tc.want {
			t.Errorf("%s: %s, want %s", tc.name, got, tc.want)
		}
	}
}
