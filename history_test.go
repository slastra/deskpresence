package main

import (
	"path/filepath"
	"slices"
	"testing"
	"time"
)

func newTestHistory(t *testing.T) *history {
	return newHistory(filepath.Join(t.TempDir(), "history.json"), 500*time.Millisecond, 120)
}

func frameWith(e byte) Frame { return Frame{MovingGates: []byte{e, 0}} }

// Frames at 10 Hz make one bin per 500 ms, each holding its max, and the
// file is written as each bin closes, not before.
func TestHistoryBinsByFrames(t *testing.T) {
	h := newTestHistory(t)
	t0 := time.UnixMilli(1_000_000)
	for i := range 10 { // 1 s of frames: bins at t0 and t0+500ms
		if h.observe(frameWith(byte(10+i)), 2, true, 40, t0.Add(time.Duration(i)*100*time.Millisecond)) {
			if i != 5 {
				t.Fatalf("wrote at frame %d, want only when the first bin closes (frame 5)", i)
			}
		}
	}
	h.observe(frameWith(1), 2, false, 40, t0.Add(time.Second))
	if want := []int{14, 19}; !slices.Equal(h.near, want) {
		t.Fatalf("near = %v, want %v", h.near, want)
	}
	if want := []bool{true, true}; !slices.Equal(h.present, want) {
		t.Fatalf("present = %v, want %v", h.present, want)
	}
}

// A sensor gap shows as empty bins, so the axis stays true; a long one is
// capped at the history length.
func TestHistoryFillsGaps(t *testing.T) {
	h := newTestHistory(t)
	t0 := time.UnixMilli(1_000_000)
	h.observe(frameWith(70), 2, true, 40, t0)
	h.observe(frameWith(60), 2, true, 40, t0.Add(1500*time.Millisecond))
	if want := []int{70, 0, 0}; !slices.Equal(h.near, want) {
		t.Fatalf("near = %v, want %v", h.near, want)
	}
	h.observe(frameWith(50), 2, false, 40, t0.Add(time.Hour))
	if len(h.near) != 120 || len(h.present) != 120 {
		t.Fatalf("after a long gap: %d bins, want the cap of 120", len(h.near))
	}
}

// Frames without per-gate energy (basic mode, the ToF sensor) are ignored.
func TestHistoryIgnoresBasicFrames(t *testing.T) {
	h := newTestHistory(t)
	t0 := time.UnixMilli(1_000_000)
	h.observe(Frame{}, 2, true, 40, t0)
	h.observe(Frame{}, 2, true, 40, t0.Add(time.Second))
	if len(h.near) != 0 {
		t.Fatalf("near = %v, want nothing", h.near)
	}
}
