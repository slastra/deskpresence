package main

import (
	"encoding/json"
	"fmt"
	"log"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"time"
)

// audio fades the playing PipeWire streams with the screen: down to silence
// over the absence warning, held at zero while away (the players are paused
// underneath), and back up to their original levels when someone returns.
// Streams are addressed by sink-input index and restored to the exact
// volume they had; a stream that vanished meanwhile is simply skipped.
// Volume is set through pactl rather than MPRIS because Firefox's MPRIS
// ignores its Volume property.
type audio struct {
	mu      sync.Mutex
	exclude []string        // application.name substrings left alone (UI cues)
	orig    map[int]float64 // index -> original volume fraction, while faded
	level   float64         // last applied fade level
}

func newAudio(exclude string) *audio {
	var ex []string
	for _, e := range strings.Split(exclude, ",") {
		if e = strings.TrimSpace(e); e != "" {
			ex = append(ex, strings.ToLower(e))
		}
	}
	return &audio{exclude: ex, orig: map[int]float64{}}
}

type sinkInput struct {
	Index      int `json:"index"`
	Corked     bool
	Volume     map[string]struct{ Value float64 } `json:"volume"`
	Properties map[string]string                  `json:"properties"`
}

func (a *audio) streams() []sinkInput {
	out, err := exec.Command("pactl", "-f", "json", "list", "sink-inputs").Output()
	if err != nil {
		return nil
	}
	var all []sinkInput
	if json.Unmarshal(out, &all) != nil {
		return nil
	}
	var keep []sinkInput
	for _, s := range all {
		name := strings.ToLower(s.Properties["application.name"])
		skip := s.Corked || len(s.Volume) == 0
		for _, e := range a.exclude {
			if strings.Contains(name, e) {
				skip = true
			}
		}
		if !skip {
			keep = append(keep, s)
		}
	}
	return keep
}

func volumeOf(s sinkInput) float64 {
	for _, ch := range s.Volume {
		return ch.Value / 65536.0
	}
	return 1
}

func (a *audio) set(index int, frac float64) {
	pct := int(frac*100 + 0.5)
	_ = exec.Command("pactl", "set-sink-input-volume", strconv.Itoa(index), fmt.Sprintf("%d%%", pct)).Run()
}

// apply moves every faded stream to orig*(1-level). The first non-zero
// level snapshots the playing streams; level 0 restores them and forgets.
func (a *audio) apply(level float64) {
	if a == nil {
		return
	}
	if !a.mu.TryLock() { // a ramp is in progress; it owns the streams
		return
	}
	defer a.mu.Unlock()
	if level == a.level {
		return
	}
	if level > 0 && len(a.orig) == 0 {
		for _, s := range a.streams() {
			a.orig[s.Index] = volumeOf(s)
		}
		if len(a.orig) > 0 {
			log.Printf("audio: fading %d stream(s)", len(a.orig))
		}
	}
	for idx, v := range a.orig {
		a.set(idx, v*(1-level))
	}
	a.level = level
	if level == 0 {
		if len(a.orig) > 0 {
			log.Printf("audio: restored")
		}
		a.orig = map[int]float64{}
	}
}

// rampUp brings the faded streams back over `d` in steps, then forgets
// them. Called after the players have been resumed.
func (a *audio) rampUp(d time.Duration) {
	if a == nil {
		return
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if len(a.orig) == 0 {
		a.level = 0
		return
	}
	const steps = 8
	for i := 1; i <= steps; i++ {
		for idx, v := range a.orig {
			a.set(idx, v*float64(i)/steps)
		}
		time.Sleep(d / steps)
	}
	log.Printf("audio: ramped %d stream(s) back up", len(a.orig))
	a.orig = map[int]float64{}
	a.level = 0
}
