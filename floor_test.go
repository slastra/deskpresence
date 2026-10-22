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
	// Seven hours on, the old slots have aged out, but what they taught is
	// remembered, across a restart too, until fresh frames replace it.
	g.observe(20, now.Add(7*time.Hour))
	if th, n, ok := g.threshold(); !ok || th != 54 || n != 1 {
		t.Fatalf("aged: got %d/%d/%v, want 54 remembered from 1 frame", th, n, ok)
	}
	g.observe(20, now.Add(7*time.Hour+10*time.Minute)) // a new slot saves
	h := newFloorLearner(path, 40)
	if th, _, ok := h.threshold(); !ok || th != 54 {
		t.Fatalf("remembered after reload: got %d/%v", th, ok)
	}
	if th, _, _ := newFloorLearner(path, 60).threshold(); th != 60 {
		t.Fatalf("a remembered threshold under a raised minimum: got %d, want 60", th)
	}
	fill(h, now.Add(7*time.Hour+10*time.Minute), map[int]int{30: 3000})
	if th, _, ok := h.threshold(); !ok || th != 40 {
		t.Fatalf("fresh quiet frames replace it: got %d/%v, want 40", th, ok)
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

// TestFloorTeaches: the radar's away teaches only frames under the
// threshold with the TV off, so a body's frames at 100 no longer drag the
// threshold to the cap; an hour without input teaches every frame, so the
// threshold can still rise; the blackout, TV lit, teaches nothing.
func TestFloorTeaches(t *testing.T) {
	now := time.Now()
	feed := func(f *floorLearner, counts map[int]int, known, present bool, tv string, in Input) {
		for e, n := range counts {
			for range n {
				if floorTeaches(known, present, tv, in, e, 46) {
					f.observe(e, now)
				}
			}
		}
	}
	leak := map[int]int{20: 2970, 100: 30}

	all := newFloorLearner(filepath.Join(t.TempDir(), "floor.json"), 46)
	fill(all, now, leak)
	if th, _, _ := all.threshold(); th != 70 {
		t.Fatalf("control: every frame counted, got %d, want the 70 cap", th)
	}
	away := newFloorLearner(filepath.Join(t.TempDir(), "floor.json"), 46)
	feed(away, leak, true, false, "off", InputQuiet)
	feed(away, leak, true, false, "off", InputQuiet) // twice: past minFrames
	if th, n, ok := away.threshold(); !ok || th != 46 || n != 5940 {
		t.Fatalf("radar away: got %d/%d/%v, want the minimum from the 5940 quiet frames", th, n, ok)
	}

	noisy := newFloorLearner(filepath.Join(t.TempDir(), "floor.json"), 46)
	feed(noisy, map[int]int{20: 2970, 60: 30}, true, true, "on", InputAway)
	if th, _, ok := noisy.threshold(); !ok || th != 64 {
		t.Fatalf("input away, radar present: got %d/%v, want 64", th, ok)
	}

	for _, tc := range []struct {
		name           string
		known, present bool
		tv             string
		in             Input
		e              int
	}{
		{"blackout, TV on", true, false, "on", InputQuiet, 20},
		{"radar away, TV off, at the threshold", true, false, "off", InputQuiet, 46},
		{"radar present", true, true, "off", InputQuiet, 20},
		{"no verdict yet", false, false, "off", InputIdle, 20},
	} {
		if floorTeaches(tc.known, tc.present, tc.tv, tc.in, tc.e, 46) {
			t.Errorf("%s: taught the floor", tc.name)
		}
	}
}
