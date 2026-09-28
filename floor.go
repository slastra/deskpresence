package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"time"
)

// floorLearner learns the presence threshold from the room's own noise.
//
// The empty-room noise on this sensor drifts: 27 at the most after the
// module swap (2026-09-19), a median of 20 and a 99.5th percentile of 41 by
// 2026-09-27, when single noise frames over the fixed threshold of 38 kept
// the verdict "present" all day. It learns only from frames the daemon is
// confident are an empty room, the radar's own "away" with the TV off, or an
// hour without keyboard or mouse (a radar stuck on "present" has no away
// frames to learn from, so input is the second source). The threshold is the
// 99.5th percentile of the last six hours of those frames plus a margin,
// clamped: it can rise with the noise and fall back when the noise does, but
// never below the configured minimum.
type floorLearner struct {
	path      string
	slot      time.Duration
	slots     int
	minFrames int     // no opinion until this many empty frames are held
	quantile  float64 // 0.995
	margin    int
	min, max  int
	hist      map[int64]*[101]int
	lastSlot  int64
}

func newFloorLearner(path string, min int) *floorLearner {
	f := &floorLearner{path: path, slot: 10 * time.Minute, slots: 36, minFrames: 3000,
		quantile: 0.995, margin: 4, min: min, max: 70, hist: map[int64]*[101]int{}}
	f.load()
	return f
}

// observe counts one near-gate energy reading from an empty room.
func (f *floorLearner) observe(e int, now time.Time) {
	s := now.UnixMilli() / f.slot.Milliseconds()
	h := f.hist[s]
	if h == nil {
		h = &[101]int{}
		f.hist[s] = h
	}
	h[max(0, min(100, e))]++
	if s != f.lastSlot {
		// A new slot: drop what has aged out, and save what is held.
		for k := range f.hist {
			if k <= s-int64(f.slots) {
				delete(f.hist, k)
			}
		}
		if f.lastSlot != 0 {
			f.save()
		}
		f.lastSlot = s
	}
}

// threshold is the learned threshold and how many frames it rests on; ok is
// false until there are enough.
func (f *floorLearner) threshold() (t, frames int, ok bool) {
	var sum [101]int
	for _, h := range f.hist {
		for i, n := range h {
			sum[i] += n
			frames += n
		}
	}
	if frames < f.minFrames {
		return f.min, frames, false
	}
	need := int(float64(frames) * f.quantile)
	acc := 0
	for i, n := range sum {
		acc += n
		if acc >= need {
			return max(f.min, min(f.max, i+f.margin)), frames, true
		}
	}
	return f.max, frames, true
}

func (f *floorLearner) save() {
	out := map[string][101]int{}
	for k, h := range f.hist {
		out[strconv.FormatInt(k, 10)] = *h
	}
	b, _ := json.Marshal(map[string]any{"slot_ms": f.slot.Milliseconds(), "slots": out})
	_ = os.MkdirAll(filepath.Dir(f.path), 0o755)
	tmp := f.path + ".tmp"
	if os.WriteFile(tmp, b, 0o644) == nil {
		_ = os.Rename(tmp, f.path)
	}
}

func (f *floorLearner) load() {
	b, err := os.ReadFile(f.path)
	if err != nil {
		return
	}
	var in struct {
		SlotMs int64            `json:"slot_ms"`
		Slots  map[string][]int `json:"slots"`
	}
	if json.Unmarshal(b, &in) != nil || in.SlotMs != f.slot.Milliseconds() {
		return
	}
	now := time.Now().UnixMilli() / f.slot.Milliseconds()
	for k, v := range in.Slots {
		s, err := strconv.ParseInt(k, 10, 64)
		if err != nil || s <= now-int64(f.slots) || len(v) != 101 {
			continue
		}
		var h [101]int
		copy(h[:], v)
		f.hist[s] = &h
	}
}
