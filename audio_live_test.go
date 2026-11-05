//go:build livepactl

package main

// Against the real PipeWire, touching only a test application's streams:
//
//	go test -tags livepactl -run Live -v .
//
// A stream fades, ends mid-fade, the fade comes back up: its saved volume
// must be the original again, so its next stream opens there.

import (
	"encoding/json"
	"os/exec"
	"testing"
	"time"
)

const liveApp = "fadetest-app"

func openLive(t *testing.T) (*exec.Cmd, int, float64) {
	t.Helper()
	cmd := exec.Command("pw-play", "-P", `{ "application.name" = "`+liveApp+`" }`, "--raw",
		"--format", "f32", "--rate", "48000", "--channels", "2", "/dev/zero")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	find := func() (sinkInput, bool) {
		out, _ := exec.Command("pactl", "-f", "json", "list", "sink-inputs").Output()
		var all []sinkInput
		_ = json.Unmarshal(out, &all)
		for _, s := range all {
			if s.Properties["application.name"] == liveApp && !isProxy(s) {
				return s, true
			}
		}
		return sinkInput{}, false
	}
	for i := 0; i < 50; i++ {
		if _, ok := find(); ok {
			// stream-restore applies the saved volume just after the node
			// appears; read it once that has happened
			time.Sleep(150 * time.Millisecond)
			s, _ := find()
			return cmd, s.Index, volumeOf(s)
		}
		time.Sleep(30 * time.Millisecond)
	}
	_ = cmd.Process.Kill()
	t.Fatal("the test stream never appeared")
	return nil, 0, 0
}

func TestLiveGoneStreamRepaired(t *testing.T) {
	a := newAudio("", "")
	a.subscribe = nil
	real := a.list
	a.list = func() ([]byte, error) { // only the test app (and its stand-ins)
		out, err := real()
		if err != nil {
			return nil, err
		}
		var all []map[string]any
		_ = json.Unmarshal(out, &all)
		var keep []map[string]any
		for _, s := range all {
			if p, _ := s["properties"].(map[string]any); p["application.name"] == liveApp {
				keep = append(keep, s)
			}
		}
		return json.Marshal(keep)
	}

	cmd, idx, _ := openLive(t)
	a.set(idx, 80)
	time.Sleep(200 * time.Millisecond)
	a.apply(0.5)
	_ = cmd.Process.Kill() // ends mid-fade, saved at 40%
	_ = cmd.Wait()
	time.Sleep(300 * time.Millisecond)
	a.apply(1)
	a.rampUp(200 * time.Millisecond)

	cmd, _, vol := openLive(t)
	defer func() { _ = cmd.Process.Kill(); _ = cmd.Wait() }()
	near(t, "next stream", vol, 0.8)
	if len(a.pending) != 0 {
		t.Errorf("pending %v", a.pending)
	}
}
