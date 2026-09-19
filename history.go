package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"time"
)

// history keeps the last minute of the presence signal in 500 ms bins and
// writes it as a small JSON file for the bar's popout graph. Each bin holds
// the max near-gate moving energy seen in it and the daemon's verdict.
type history struct {
	path      string
	binMs     int64
	n         int
	near      []int
	present   []bool
	curBin    int64
	curMax    int
	haveFrame bool
}

func newHistory(path string, bin time.Duration, n int) *history {
	return &history{path: path, binMs: bin.Milliseconds(), n: n}
}

func (h *history) observe(f Frame, near int, now time.Time) {
	if len(f.MovingGates) == 0 {
		return
	}
	e := 0
	for _, v := range f.MovingGates[:min(near, len(f.MovingGates))] {
		e = max(e, int(v))
	}
	h.curMax = max(h.curMax, e)
	h.haveFrame = true
}

// tick closes the current bin when its time is up; returns true when the
// file was rewritten.
func (h *history) tick(now time.Time, present bool, threshold int) bool {
	bin := now.UnixMilli() / h.binMs
	if h.curBin == 0 {
		h.curBin = bin
		return false
	}
	if bin == h.curBin || !h.haveFrame {
		return false
	}
	h.near = append(h.near, h.curMax)
	h.present = append(h.present, present)
	if len(h.near) > h.n {
		h.near = h.near[len(h.near)-h.n:]
		h.present = h.present[len(h.present)-h.n:]
	}
	h.curBin, h.curMax = bin, 0
	b, _ := json.Marshal(map[string]any{
		"bin_ms": h.binMs, "threshold": threshold, "near": h.near, "present": h.present,
	})
	_ = os.MkdirAll(filepath.Dir(h.path), 0o755)
	tmp := h.path + ".tmp"
	if err := os.WriteFile(tmp, append(b, '\n'), 0o644); err != nil {
		return false
	}
	return os.Rename(tmp, h.path) == nil
}
