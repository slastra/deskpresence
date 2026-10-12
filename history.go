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
// Bins are closed by the frames themselves, not the main loop's tick, so
// the graph's time axis never depends on how fast the loop runs.
type history struct {
	path       string
	binMs      int64
	n          int
	near       []int
	present    []bool
	curBin     int64
	curMax     int
	curPresent bool
}

func newHistory(path string, bin time.Duration, n int) *history {
	return &history{path: path, binMs: bin.Milliseconds(), n: n}
}

// observe folds one frame into the current bin. A frame in a later bin
// closes the current one first, and any bins skipped in between (the sensor
// went quiet) are recorded as empty. Returns true when the file was
// rewritten.
func (h *history) observe(f Frame, near int, present bool, threshold int, now time.Time) bool {
	if len(f.MovingGates) == 0 {
		return false
	}
	e := 0
	for _, v := range f.MovingGates[:min(near, len(f.MovingGates))] {
		e = max(e, int(v))
	}
	bin := now.UnixMilli() / h.binMs
	wrote := false
	if h.curBin != 0 && bin != h.curBin {
		h.near = append(h.near, h.curMax)
		h.present = append(h.present, h.curPresent)
		for range min(bin-h.curBin-1, int64(h.n)) {
			h.near = append(h.near, 0)
			h.present = append(h.present, present)
		}
		if len(h.near) > h.n {
			h.near = h.near[len(h.near)-h.n:]
			h.present = h.present[len(h.present)-h.n:]
		}
		h.curMax = 0
		wrote = h.write(threshold)
	}
	h.curBin = bin
	h.curMax = max(h.curMax, e)
	h.curPresent = present
	return wrote
}

func (h *history) write(threshold int) bool {
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
