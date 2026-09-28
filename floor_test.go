package main

import (
	"path/filepath"
	"testing"
	"time"
)

func fill(f *floorLearner, now time.Time, counts map[int]int) {
	for e, n := range counts {
		for range n {
			f.observe(e, now)
		}
	}
}

func TestFloorThreshold(t *testing.T) {
	now := time.Now()
	f := newFloorLearner(filepath.Join(t.TempDir(), "floor.json"), 40)
	fill(f, now, map[int]int{20: 2000})
	if th, n, ok := f.threshold(); ok || th != 40 || n != 2000 {
		t.Fatalf("under minFrames: got %d/%d/%v, want the minimum and no opinion", th, n, ok)
	}
	// 99.4% quiet, 0.6% at 50: the 99.5th percentile lands on 50.
	fill(f, now, map[int]int{20: 2970, 50: 30})
	if th, _, ok := f.threshold(); !ok || th != 54 {
		t.Fatalf("got %d/%v, want 54", th, ok)
	}

	lo := newFloorLearner(filepath.Join(t.TempDir(), "floor.json"), 40)
	fill(lo, now, map[int]int{5: 4000})
	if th, _, _ := lo.threshold(); th != 40 {
		t.Fatalf("a quiet room never goes under the minimum: got %d", th)
	}
	hi := newFloorLearner(filepath.Join(t.TempDir(), "floor.json"), 40)
	fill(hi, now, map[int]int{95: 4000})
	if th, _, _ := hi.threshold(); th != 70 {
		t.Fatalf("clamped at the maximum: got %d", th)
	}
}

func TestFloorSaveLoadAndAge(t *testing.T) {
	path := filepath.Join(t.TempDir(), "floor.json")
	now := time.Now()
	f := newFloorLearner(path, 40)
	fill(f, now.Add(-20*time.Minute), map[int]int{20: 2970, 50: 30})
	f.observe(20, now) // a new slot saves what is held
	g := newFloorLearner(path, 40)
	if th, n, ok := g.threshold(); !ok || th != 54 || n < 3000 {
		t.Fatalf("reloaded: got %d/%d/%v", th, n, ok)
	}
	// Seven hours on, the old slots have aged out.
	g.observe(20, now.Add(7*time.Hour))
	if _, n, ok := g.threshold(); ok || n != 1 {
		t.Fatalf("aged: got %d frames ok=%v, want 1 and no opinion", n, ok)
	}
}

// engFrame is an engineering frame with the given energy in gate 0.
func engFrame(e byte) Frame {
	return Frame{State: 1, MovingCM: 60, MovingGates: []byte{e, 10, 10, 5, 5, 5, 5, 5, 5}}
}

// TestConfirmFrames: sparse single noise frames over the threshold no longer
// keep an empty room "present", while a still reader's pairs of frames do.
func TestConfirmFrames(t *testing.T) {
	run := func(confirm int, hot func(i int) bool) bool {
		p := &Policy{Absence: 30 * time.Second, Debounce: 500 * time.Millisecond,
			NearGates: 2, EnergyMin: 45, ConfirmFrames: confirm, ConfirmWindow: 4 * time.Second,
			StaleAfter: 5 * time.Second, MinBackoff: 30 * time.Second, MaxBackoff: 10 * time.Minute}
		t0 := time.Unix(1000, 0)
		for i := range 1200 { // two minutes at 10 frames/s
			e := byte(20)
			if i < 3 || hot(i) {
				e = 60
			}
			p.Observe(engFrame(e), t0.Add(time.Duration(i)*100*time.Millisecond))
		}
		pr, known := p.Present()
		return pr && known
	}
	noise := func(i int) bool { return i%150 == 0 }                // one hot frame every 15 s
	reader := func(i int) bool { return i%150 == 0 || i%150 == 5 } // a pair every 15 s
	if !run(1, noise) {
		t.Fatalf("control: with single-frame evidence, noise every 15 s keeps it present")
	}
	if run(2, noise) {
		t.Fatalf("single noise frames kept an empty room present")
	}
	if !run(2, reader) {
		t.Fatalf("a still reader's frame pairs lost presence")
	}
}
