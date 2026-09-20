package main

import (
	"testing"
	"time"
)

func newTestPolicy() *Policy {
	return &Policy{Absence: 60 * time.Second, Debounce: 500 * time.Millisecond, MaxDistance: 250,
		StaleAfter: 5 * time.Second, MinBackoff: 30 * time.Second, MaxBackoff: 10 * time.Minute}
}

func TestLeaveAndReturn(t *testing.T) {
	p := newTestPolicy()
	t0 := time.Unix(1000, 0)
	here := Frame{State: 2, StaticCM: 100}
	gone := Frame{}
	p.Observe(here, t0)
	if a := p.Decide("off", t0, false); a != "on" {
		t.Fatalf("seated at start, tv off: want on, got %q", a)
	}
	// user leaves; nothing for 59s
	for s := 1; s <= 59; s++ {
		p.Observe(gone, t0.Add(time.Duration(s)*time.Second))
	}
	if a := p.Decide("on", t0.Add(59*time.Second), false); a != "" {
		t.Fatalf("59s absent: want no action, got %q", a)
	}
	p.Observe(gone, t0.Add(61*time.Second))
	if a := p.Decide("on", t0.Add(61*time.Second), false); a != "off" {
		t.Fatalf("61s absent: want off, got %q", a)
	}
	// returns: present within debounce
	t1 := t0.Add(5 * time.Minute)
	p.Observe(here, t1)
	p.Observe(here, t1.Add(200*time.Millisecond))
	if a := p.Decide("off", t1.Add(200*time.Millisecond), false); a != "" {
		t.Fatalf("200ms present: want no action yet, got %q", a)
	}
	p.Observe(here, t1.Add(600*time.Millisecond))
	if a := p.Decide("off", t1.Add(600*time.Millisecond), false); a != "on" {
		t.Fatalf("600ms present: want on, got %q", a)
	}
}

func TestStillSitterStaysPresent(t *testing.T) {
	// A motionless person is isolated single frames above threshold every
	// ~2 s; each must refresh the absence clock even though none lasts
	// Debounce. And once absence has latched, a lone frame must not flip
	// back to present.
	p := newTestPolicy()
	p.Absence = 10 * time.Second
	t0 := time.Unix(1000, 0)
	here := Frame{State: 2, StaticCM: 100}
	p.Observe(here, t0)
	for i := 1; i <= 300; i++ { // 30 s at 10 Hz, one live frame every 2 s
		f := Frame{}
		if i%20 == 0 {
			f = here
		}
		p.Observe(f, t0.Add(time.Duration(i)*100*time.Millisecond))
	}
	if pr, _ := p.Present(); !pr {
		t.Fatal("still sitter went absent")
	}
	for i := 301; i <= 420; i++ { // 12 s of nothing -> absent
		p.Observe(Frame{}, t0.Add(time.Duration(i)*100*time.Millisecond))
	}
	if pr, _ := p.Present(); pr {
		t.Fatal("did not latch absent after 12 s")
	}
	p.Observe(here, t0.Add(43*time.Second)) // one frame
	p.Observe(Frame{}, t0.Add(43*time.Second+100*time.Millisecond))
	if pr, _ := p.Present(); pr {
		t.Fatal("a lone frame flipped absent -> present")
	}
}

func TestFarTargetIgnored(t *testing.T) {
	p := newTestPolicy()
	t0 := time.Unix(1000, 0)
	p.Observe(Frame{State: 1, MovingCM: 400}, t0)
	if pr, _ := p.Present(); pr {
		t.Fatal("target at 400cm counted as present with MaxDistance 250")
	}
}

func TestGateRule(t *testing.T) {
	p := newTestPolicy()
	p.NearGates, p.EnergyMin = 3, 40
	t0 := time.Unix(1000, 0)
	// engineering frame: summary says "static at 300 cm" (the phantom) but
	// gate 1 carries moving energy 55 -> present
	p.Observe(Frame{State: 2, StaticCM: 300, MovingGates: []byte{10, 55, 20, 0, 0, 0, 0, 0, 0}}, t0)
	if pr, _ := p.Present(); !pr {
		t.Fatal("near-gate energy 55 should be present regardless of summary distance")
	}
	// energy only in gate 3+ (beyond the desk) with the summary claiming a
	// moving target at 120 cm -> not present
	q := newTestPolicy()
	q.NearGates, q.EnergyMin = 3, 40
	q.Observe(Frame{State: 1, MovingCM: 120, MovingGates: []byte{5, 8, 12, 90, 60, 0, 0, 0, 0}}, t0)
	if pr, _ := q.Present(); pr {
		t.Fatal("energy only beyond the near gates must not count")
	}
	// basic frame (no gate data) falls back to the distance rule
	r := newTestPolicy()
	r.NearGates, r.EnergyMin = 3, 40
	r.Observe(Frame{State: 1, MovingCM: 120}, t0)
	if pr, _ := r.Present(); !pr {
		t.Fatal("basic frame should fall back to MaxDistance")
	}
}

func TestBackoffAndStale(t *testing.T) {
	p := newTestPolicy()
	// an empty room has to earn its first verdict: Absence of quiet frames
	t0 := time.Unix(1000, 0).Add(-p.Absence)
	p.Observe(Frame{}, t0)
	t0 = t0.Add(p.Absence)
	p.Observe(Frame{}, t0)
	if a := p.Decide("on", t0, false); a != "off" {
		t.Fatalf("want off, got %q", a)
	}
	// actuator failed to record "off"; immediate retry must be suppressed
	if a := p.Decide("on", t0.Add(time.Second), false); a != "" {
		t.Fatalf("want backoff, got %q", a)
	}
	p.Observe(Frame{}, t0.Add(31*time.Second))
	if a := p.Decide("on", t0.Add(31*time.Second), false); a != "off" {
		t.Fatalf("after 30s backoff want off, got %q", a)
	}
	// busy actuator never gets a second request
	if a := p.Decide("on", t0.Add(2*time.Minute), true); a != "" {
		t.Fatalf("busy: want none, got %q", a)
	}
	// sensor silent for 6s: no opinion
	if a := p.Decide("on", t0.Add(37*time.Second), false); a != "" {
		t.Fatalf("stale sensor: want none, got %q", a)
	}
}

func TestFadeLevel(t *testing.T) {
	p := newTestPolicy()
	p.Absence, p.Fade = 10*time.Second, 5*time.Second
	t0 := time.Unix(1000, 0)
	p.Observe(Frame{State: 1, MovingCM: 90}, t0)
	if f := p.FadeLevel(t0); f != 0 {
		t.Fatalf("fresh: want 0, got %v", f)
	}
	feedTo := func(sec float64) time.Time {
		var now time.Time
		for i := 1; i <= int(sec*10); i++ {
			now = t0.Add(time.Duration(i) * 100 * time.Millisecond)
			p.Observe(Frame{}, now)
		}
		return now
	}
	if f := p.FadeLevel(feedTo(4)); f != 0 {
		t.Fatalf("4s idle: want 0, got %v", f)
	}
	if f := p.FadeLevel(feedTo(7.5)); f != 0.5 {
		t.Fatalf("7.5s idle: want 0.5, got %v", f)
	}
	// one breath mid-fade cancels it outright
	p.Observe(Frame{State: 1, MovingCM: 90}, t0.Add(7600*time.Millisecond))
	if f := p.FadeLevel(t0.Add(7600 * time.Millisecond)); f != 0 {
		t.Fatalf("one breath mid-fade: want 0, got %v", f)
	}
	// let it run out: absent latches and the fade holds at 1
	p = newTestPolicy()
	p.Absence, p.Fade = 10*time.Second, 5*time.Second
	p.Observe(Frame{State: 1, MovingCM: 90}, t0)
	if f := p.FadeLevel(feedTo(10)); f != 1 {
		t.Fatalf("10s idle: want 1, got %v", f)
	}
	// coming back needs the arrival debounce (500 ms), then the shade lifts
	for i := 0; i <= 6; i++ {
		p.Observe(Frame{State: 1, MovingCM: 90}, t0.Add(11*time.Second+time.Duration(i)*100*time.Millisecond))
	}
	if f := p.FadeLevel(t0.Add(11600 * time.Millisecond)); f != 0 {
		t.Fatalf("back for 600ms: want 0, got %v", f)
	}
}

func TestStartupWaitsForAVerdict(t *testing.T) {
	p := newTestPolicy()
	p.NearGates, p.EnergyMin = 2, 38
	t0 := time.Unix(1000, 0)
	quiet := Frame{State: 2, StaticCM: 145, MovingGates: []byte{30, 25, 20, 0, 0, 0, 0, 0, 0}}
	// a still reader: 5 s of frames under threshold must not become "off"
	for i := 0; i < 50; i++ {
		p.Observe(quiet, t0.Add(time.Duration(i)*100*time.Millisecond))
		if a := p.Decide("on", t0.Add(time.Duration(i)*100*time.Millisecond), false); a != "" {
			t.Fatalf("frame %d: decided %q before any verdict", i, a)
		}
	}
	if _, known := p.Present(); known {
		t.Fatal("presence must stay unknown until earned")
	}
	// one breath seeds present
	p.Observe(Frame{State: 1, MovingGates: []byte{20, 45, 0, 0, 0, 0, 0, 0, 0}}, t0.Add(5*time.Second))
	if pr, known := p.Present(); !pr || !known {
		t.Fatal("a frame above threshold should seed present")
	}
	// an empty room at start: absent after Absence, and only then
	q := newTestPolicy()
	q.NearGates, q.EnergyMin = 2, 38
	for i := 0; i <= 600; i++ {
		now := t0.Add(time.Duration(i) * 100 * time.Millisecond)
		q.Observe(quiet, now)
		_, known := q.Present()
		if want := now.Sub(t0) >= q.Absence; known != want {
			t.Fatalf("at %s known=%v want %v", now.Sub(t0), known, want)
		}
	}
	if a := q.Decide("on", t0.Add(61*time.Second), false); a != "off" {
		t.Fatalf("empty room after Absence should turn the TV off, got %q", a)
	}
}
