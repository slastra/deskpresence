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
	if a := decideQ(p, "off", t0, false); a != "on" {
		t.Fatalf("seated at start, tv off: want on, got %q", a)
	}
	// user leaves; nothing for 59s
	for s := 1; s <= 59; s++ {
		p.Observe(gone, t0.Add(time.Duration(s)*time.Second))
	}
	if a := decideQ(p, "on", t0.Add(59*time.Second), false); a != "" {
		t.Fatalf("59s absent: want no action, got %q", a)
	}
	p.Observe(gone, t0.Add(61*time.Second))
	if a := decideQ(p, "on", t0.Add(61*time.Second), false); a != "off" {
		t.Fatalf("61s absent: want off, got %q", a)
	}
	// returns: present within debounce
	t1 := t0.Add(5 * time.Minute)
	p.Observe(here, t1)
	p.Observe(here, t1.Add(200*time.Millisecond))
	if a := decideQ(p, "off", t1.Add(200*time.Millisecond), false); a != "" {
		t.Fatalf("200ms present: want no action yet, got %q", a)
	}
	p.Observe(here, t1.Add(600*time.Millisecond))
	if a := decideQ(p, "off", t1.Add(600*time.Millisecond), false); a != "on" {
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

// decideQ is Decide as the radar-driven tests mean it: input quiet, so a
// healthy radar decides.
func decideQ(p *Policy, tv string, now time.Time, busy bool) string {
	w, _ := p.Want(now, InputQuiet, 0)
	return p.Decide(w, tv, now, busy)
}

func TestBackoffAndStale(t *testing.T) {
	p := newTestPolicy()
	// an empty room has to earn its first verdict: Absence of quiet frames
	t0 := time.Unix(1000, 0).Add(-p.Absence)
	p.Observe(Frame{}, t0)
	t0 = t0.Add(p.Absence)
	p.Observe(Frame{}, t0)
	if a := decideQ(p, "on", t0, false); a != "off" {
		t.Fatalf("want off, got %q", a)
	}
	// actuator failed to record "off"; immediate retry must be suppressed
	if a := decideQ(p, "on", t0.Add(time.Second), false); a != "" {
		t.Fatalf("want backoff, got %q", a)
	}
	p.Observe(Frame{}, t0.Add(31*time.Second))
	if a := decideQ(p, "on", t0.Add(31*time.Second), false); a != "off" {
		t.Fatalf("after 30s backoff want off, got %q", a)
	}
	// busy actuator never gets a second request
	if a := decideQ(p, "on", t0.Add(2*time.Minute), true); a != "" {
		t.Fatalf("busy: want none, got %q", a)
	}
	// sensor silent for 6s: no longer a hold. Recent input keeps the
	// panel on, idle or no input signal turns it off (fail safe).
	stale := t0.Add(37 * time.Second)
	for in, want := range map[Input]string{InputQuiet: "on", InputIdle: "off", InputUnknown: "off"} {
		if w, _ := p.Want(stale, in, 0); w != want {
			t.Errorf("stale sensor, input %q: want %q, got %q", in, want, w)
		}
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
		if a := decideQ(p, "on", t0.Add(time.Duration(i)*100*time.Millisecond), false); a != "" {
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
	if a := decideQ(q, "on", t0.Add(61*time.Second), false); a != "off" {
		t.Fatalf("empty room after Absence should turn the TV off, got %q", a)
	}
}

// TestWantTable pins the whole decision, row by row: input away beats the
// radar, input active beats a radar that misses you, a healthy radar
// decides otherwise (capped when there is no input signal), and with the
// radar out the input timer decides. Fail safe throughout.
func TestWantTable(t *testing.T) {
	present := func() *Policy {
		p := newTestPolicy()
		p.InputUnknownCap = time.Hour
		t0 := time.Unix(1000, 0)
		for i := 0; i <= 6; i++ {
			p.Observe(Frame{State: 1, MovingCM: 90}, t0.Add(time.Duration(i)*100*time.Millisecond))
		}
		return p
	}
	absent := func() *Policy {
		p := newTestPolicy()
		t0 := time.Unix(1000, 0).Add(-p.Absence)
		p.Observe(Frame{}, t0)
		p.Observe(Frame{}, t0.Add(p.Absence))
		return p
	}
	now := time.Unix(1000, 0).Add(700 * time.Millisecond)
	for _, tc := range []struct {
		name       string
		p          *Policy
		in         Input
		unknownFor time.Duration
		at         time.Time
		want       string
	}{
		{"away beats a present radar", present(), InputAway, 0, now, "off"},
		{"active beats an absent radar", absent(), InputActive, 0, now, "on"},
		{"quiet: radar present", present(), InputQuiet, 0, now, "on"},
		{"idle: radar present keeps it on", present(), InputIdle, 0, now, "on"},
		{"quiet: radar absent", absent(), InputQuiet, 0, now, "off"},
		{"no input signal, radar present, under the cap", present(), InputUnknown, 59 * time.Minute, now, "on"},
		{"no input signal, radar present, past the cap", present(), InputUnknown, time.Hour, now, "off"},
		{"radar stale, input quiet", present(), InputQuiet, 0, now.Add(time.Minute), "on"},
		{"radar stale, input idle", present(), InputIdle, 0, now.Add(time.Minute), "off"},
		{"radar stale, no input signal", present(), InputUnknown, 0, now.Add(time.Minute), "off"},
	} {
		if got, why := tc.p.Want(tc.at, tc.in, tc.unknownFor); got != tc.want {
			t.Errorf("%s: want %q, got %q (%s)", tc.name, tc.want, got, why)
		}
	}
	// A radar still earning its first verdict, with no input signal, has
	// no opinion yet rather than blanking a restart.
	fresh := newTestPolicy()
	fresh.Observe(Frame{}, time.Unix(1000, 0))
	if got, _ := fresh.Want(time.Unix(1000, 0), InputUnknown, 0); got != "" {
		t.Errorf("waiting for a verdict: want no opinion, got %q", got)
	}
}

// TestBlackout: once the desk empties, the radar's "off" is held for
// Blackout (the TV stays on behind black), then goes through; a return
// inside the window wants "on" with the TV never having left; a stale radar
// is not held.
func TestBlackout(t *testing.T) {
	p := newTestPolicy()
	p.Blackout = 10 * time.Minute
	t0 := time.Unix(1000, 0)
	here := Frame{State: 2, StaticCM: 100}
	p.Observe(here, t0)
	// Empty from t0; frames every second keep the radar healthy.
	at := t0
	feed := func(f Frame, until time.Time) {
		for at.Before(until) {
			at = at.Add(time.Second)
			p.Observe(f, at)
		}
	}
	away := t0.Add(p.Absence)
	feed(Frame{}, away.Add(5*time.Minute))
	want, why := p.Want(at, InputIdle, 0)
	if want != "off" || why != whyRadarAway {
		t.Fatalf("5 min away: Want should be the radar's off, got %q (%s)", want, why)
	}
	if got, why := p.HoldBlack(want, why, at); got != "" {
		t.Fatalf("5 min away: blackout should hold the TV, got %q (%s)", got, why)
	}
	if !p.InBlackout(want, why, at) {
		t.Fatal("5 min away: InBlackout should say so")
	}
	feed(Frame{}, away.Add(10*time.Minute))
	want, why = p.Want(at, InputIdle, 0)
	if got, _ := p.HoldBlack(want, why, at); got != "off" {
		t.Fatalf("10 min away: blackout over, want off, got %q", got)
	}

	// A return inside the window: present again, nothing to switch.
	p2 := newTestPolicy()
	p2.Blackout = 10 * time.Minute
	p, at = p2, t0
	p.Observe(here, t0)
	feed(Frame{}, away.Add(2*time.Minute))
	feed(here, at.Add(2*time.Second))
	want, why = p.Want(at, InputIdle, 0)
	if got, _ := p.HoldBlack(want, why, at); got != "on" {
		t.Fatalf("back inside the blackout: want on, got %q", got)
	}
	if a := p.Decide("on", "on", at, false); a != "" {
		t.Fatalf("back inside the blackout: TV already on, want no action, got %q", a)
	}

	// A stale radar is not the radar's away: off at once.
	p3 := newTestPolicy()
	p3.Blackout = 10 * time.Minute
	p, at = p3, t0
	p.Observe(here, t0)
	feed(Frame{}, away.Add(time.Minute))
	stale := at.Add(time.Minute)
	want, why = p.Want(stale, InputIdle, 0)
	if got, _ := p.HoldBlack(want, why, stale); got != "off" {
		t.Fatalf("stale radar during blackout: want off, got %q (%s)", got, why)
	}
}

// hookFire is one hook action, at its offset from the run's start.
type hookFire struct {
	at   time.Duration
	what string
}

// hookRun feeds a policy frames at 10 Hz and gives the hook gate each
// frame's Want, as the loop's tick does; it returns what fired, and when.
func hookRun(p *Policy, g *hookGate, t0 time.Time, n int, in func(i int) Input, frame func(i int) Frame) []hookFire {
	var out []hookFire
	for i := range n {
		now := t0.Add(time.Duration(i) * 100 * time.Millisecond)
		p.Observe(frame(i), now)
		w, why := p.Want(now, in(i), 0)
		if h := g.next(w, why, p.PresentSince(), p.ConfirmedAt(), now, 2*time.Minute, 5*time.Second); h != "" {
			out = append(out, hookFire{now.Sub(t0), h})
		}
	}
	return out
}

// newHookPolicy is a seated desk with the gate rule, the hooks told "on".
// It returns the policy and gate once the desk has emptied and "off" has
// fired, and the time of the next frame.
func newHookPolicy(t *testing.T) (*Policy, *hookGate, time.Time) {
	p := newTestPolicy()
	p.NearGates, p.EnergyMin, p.ConfirmFrames, p.ConfirmWindow = 2, 45, 2, 4*time.Second
	g := &hookGate{fired: "on"}
	t0 := time.Unix(1000, 0)
	quiet := func(int) Input { return InputQuiet }
	hot := func(i int) Frame { return engFrame(map[bool]byte{true: 60, false: 20}[i < 20]) }
	fired := hookRun(p, g, t0, 700, quiet, hot) // 2 s seated, then 68 s empty
	if len(fired) != 1 || fired[0].what != "off" {
		t.Fatalf("leaving: want one off, got %v", fired)
	}
	return p, g, t0.Add(70 * time.Second)
}

func TestHookGate(t *testing.T) {
	quiet := func(int) Input { return InputQuiet }

	// A phantom 30 s after leaving: two frames over the threshold, then a
	// minute of nothing. The radar says present for a while; the light
	// stays off.
	p, g, t1 := newHookPolicy(t)
	sawOn := false
	phantom := func(i int) Frame {
		if pr, _ := p.Present(); pr {
			sawOn = true
		}
		return engFrame(map[bool]byte{true: 60, false: 20}[i == 300 || i == 303])
	}
	if fired := hookRun(p, g, t1, 1200, quiet, phantom); len(fired) != 0 {
		t.Fatalf("phantom: want no hooks, got %v", fired)
	}
	if !sawOn {
		t.Fatal("phantom: the radar never said present, the test proves nothing")
	}

	// A real return 30 s after leaving: continuous frames. "on" once
	// presence has been renewed for the dwell.
	p, g, t1 = newHookPolicy(t)
	back := func(i int) Frame { return engFrame(map[bool]byte{true: 60, false: 20}[i >= 300]) }
	fired := hookRun(p, g, t1, 400, quiet, back)
	if len(fired) != 1 || fired[0].what != "on" {
		t.Fatalf("real return: want one on, got %v", fired)
	}
	if late := fired[0].at - 30*time.Second; late > 5*time.Second+2400*time.Millisecond {
		t.Fatalf("real return: on came %s after the first frame", late)
	}

	// Input wakes the desk the radar calls empty, then goes quiet: both
	// fire at once, quick or not.
	p, g, t1 = newHookPolicy(t)
	in := func(i int) Input { return map[bool]Input{true: InputActive, false: InputQuiet}[i >= 10 && i < 210] }
	fired = hookRun(p, g, t1, 400, in, func(int) Frame { return engFrame(20) })
	if len(fired) != 2 || fired[0] != (hookFire{time.Second, "on"}) || fired[1].what != "off" || fired[1].at != 21*time.Second {
		t.Fatalf("input active then quiet: want on at 1s and off at 21s, got %v", fired)
	}

	// An arrival long after the last "off" fires the moment the verdict
	// does.
	p, g, t1 = newHookPolicy(t)
	late := func(i int) Frame { return engFrame(map[bool]byte{true: 60, false: 20}[i >= 3000]) }
	fired = hookRun(p, g, t1, 3010, quiet, late)
	if len(fired) != 1 || fired[0].what != "on" || fired[0].at != 300*time.Second+600*time.Millisecond {
		t.Fatalf("first arrival: want on at 300.6s, got %v", fired)
	}
}
