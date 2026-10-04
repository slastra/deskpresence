package main

import (
	"math"
	"time"
)

// Policy turns a stream of sensor frames into on/off decisions.
//
// The invariant is simple: the TV should be on exactly when someone is at the
// desk. "At the desk" is the debounced sensor reading; "the TV is on" is the
// last state tv-screen.sh reported. Whenever the two disagree the policy asks
// for an action, with backoff so a failing actuator is retried without being
// hammered. Idle inhibitors, input activity and the like are deliberately not
// consulted: absence is absence.
type Policy struct {
	Absence     time.Duration // raw-absent this long -> absent
	Debounce    time.Duration // raw-present this long -> present
	MaxDistance uint16        // cm; 0 = any (basic frames only, see NearGates)
	// Engineering frames carry per-gate energies, and those are the honest
	// signal: someone at the desk is *moving* energy in the near gates
	// (typing, breathing, shifting), while the room's phantom is *static*
	// energy in the far ones. When a frame has gate data, raw presence is
	// "max moving energy over gates [0, NearGates) >= EnergyMin"; the
	// module's own summary distance smears 80 cm past the body and is
	// ignored. NearGates 0 disables this and falls back to MaxDistance.
	NearGates int
	EnergyMin int
	// Fade is how long before Absence the screen should start going dark:
	// a visible warning that the desk reads as empty, cheap to cancel with
	// one breath. 0 disables.
	Fade time.Duration
	// Blackout holds the TV on for this long after the desk empties, behind
	// a fully black screen (the fade sits at 1 while away), before powering
	// it off. An OLED showing black is dark pixels, nearly free, and a
	// return inside the window is instant. Past a few minutes in standby
	// the TV falls into a deeper sleep and a wake is a 17-30 s boot
	// (measured 2026-10-03: 21 of 21 wakes after 15+ min off were slow, 0
	// of 99 under 2 min). Only the radar's own away waits; an hour without
	// input, or the radar going stale, still turns it off at once. 0 = off.
	Blackout time.Duration
	// InputUnknownCap bounds how long the radar alone may keep the panel
	// lit when there is no input signal (hypridle down): past it, off.
	InputUnknownCap time.Duration
	// ConfirmFrames within ConfirmWindow at or over EnergyMin make one
	// raw-present reading. A still person is over threshold every couple
	// of seconds; room noise, since 2026-09-27, once every 10-20 s. One
	// frame used to be enough, and noise alone then held "present" forever
	// once the away timer was longer than its spacing.
	ConfirmFrames int
	ConfirmWindow time.Duration
	recent        []time.Time
	StaleAfter    time.Duration // no frames this long -> sensor unknown, no actions
	MinBackoff    time.Duration
	MaxBackoff    time.Duration

	lastFrame    time.Time
	rawSince     time.Time // start of the current raw-present run
	raw          bool
	confirmedAt  time.Time // last time a raw-present run reached Debounce
	present      bool
	presentAt    time.Time // when the debounced verdict last flipped
	presentKnown bool
	absentSince  time.Time // start of the raw-absent run before the first verdict
	lastDecision string    // last action requested
	lastAttempt  time.Time
	attempts     int // consecutive attempts of lastDecision
}

// Observe feeds one frame; it reports whether the raw (undebounced) reading
// flipped, so the caller can log what the sensor saw at the edge.
func (p *Policy) Observe(f Frame, now time.Time) (flipped bool) {
	p.lastFrame = now
	raw := p.rawPresent(f, now)
	flipped = raw != p.raw
	if raw && !p.raw {
		p.rawSince = now
	}
	p.raw = raw
	if !p.presentKnown {
		// A first reading above threshold seeds "present" at once, so a
		// fresh start with someone in the chair does not wait out the
		// absence timer. A reading below it proves nothing: a still reader
		// spends most frames under threshold (measured 2026-09-20), and
		// seeding "absent" from one of them turned every restart into an
		// off/on cycle. Absence has to be earned the usual way.
		if raw {
			p.present = true
			p.presentAt = now
			p.confirmedAt = now
			p.presentKnown = true
			p.absentSince = time.Time{}
			return true
		}
		if p.absentSince.IsZero() {
			p.absentSince = now
		}
		if now.Sub(p.absentSince) >= p.Absence {
			p.present = false
			p.presentAt = now
			p.presentKnown = true
		}
		return flipped
	}
	// Arrival needs a raw-present run of Debounce (a body walking in is
	// continuous energy, so this costs nothing real). Once present, ANY frame
	// above threshold keeps the absence clock fresh: a seated, motionless
	// person shows up as isolated single frames of moving energy every
	// couple of seconds, never as sustained runs (measured 2026-09-18: zero
	// 500 ms runs in 20 s of sitting still, but no gap longer than 2.4 s).
	// The gate rule has no empty-room blips to guard against; the old
	// "blips must not restart the clock" defence belonged to the summary-
	// distance rule and would blank the screen on a still reader here.
	if raw {
		if p.present {
			p.confirmedAt = now
		} else if now.Sub(p.rawSince) >= p.Debounce {
			p.confirmedAt = now
			p.presentAt = now
			p.present = true
		}
	}
	if !raw && p.present && now.Sub(p.confirmedAt) >= p.Absence {
		p.present = false
		p.presentAt = now
	}
	return flipped
}

func (p *Policy) rawPresent(f Frame, now time.Time) bool {
	if p.NearGates > 0 && len(f.MovingGates) > 0 {
		if nearEnergy(f, p.NearGates) < p.EnergyMin {
			return p.ConfirmFrames > 1 && p.confirmed(now)
		}
		if p.ConfirmFrames <= 1 {
			return true
		}
		p.recent = append(p.recent, now)
		return p.confirmed(now)
	}
	return f.Present() && (p.MaxDistance == 0 || f.Distance() <= p.MaxDistance)
}

// confirmed drops over-threshold frames older than ConfirmWindow and says
// whether ConfirmFrames remain.
func (p *Policy) confirmed(now time.Time) bool {
	i := 0
	for i < len(p.recent) && now.Sub(p.recent[i]) > p.ConfirmWindow {
		i++
	}
	p.recent = p.recent[i:]
	return len(p.recent) >= p.ConfirmFrames
}

// nearEnergy is the highest moving energy over the near gates.
func nearEnergy(f Frame, gates int) int {
	e := 0
	for _, v := range f.MovingGates[:min(gates, len(f.MovingGates))] {
		e = max(e, int(v))
	}
	return e
}

// Present reports the debounced state and whether it is known at all.
func (p *Policy) Present() (present, known bool) {
	return p.present, p.presentKnown
}

// FadeLevel is 0 while the desk is fresh, rising to 1 as the absence timer
// runs out, and 1 once absent. It is what the screen dims by.
func (p *Policy) FadeLevel(now time.Time) float64 {
	if !p.presentKnown || p.SensorStale(now) || p.Fade <= 0 {
		return 0
	}
	if !p.present {
		return 1
	}
	if p.raw {
		return 0
	}
	idle := now.Sub(p.confirmedAt)
	start := p.Absence - p.Fade
	if idle <= start {
		return 0
	}
	f := float64(idle-start) / float64(p.Fade)
	return math.Round(math.Min(1, f)*20) / 20 // 0.05 steps: few status rewrites
}

// PresentSince is when the debounced verdict last changed.
func (p *Policy) PresentSince() time.Time { return p.presentAt }

func (p *Policy) SensorStale(now time.Time) bool {
	return p.lastFrame.IsZero() || now.Sub(p.lastFrame) > p.StaleAfter
}

// Want is the whole decision: whether the TV should be on, and why. The
// panel is an OLED and the rule is fail safe: when in doubt, off.
//
//	input away (1 h, inhibitors ignored)  off, whatever the radar says
//	input active (< 20 s)                 on, even if the radar misses you
//	radar healthy                         the radar's verdict; with no input
//	                                      signal at all, only for
//	                                      InputUnknownCap, then off
//	radar out, input quiet (< 10 min)     on
//	radar out, input idle                 off
//	radar out, no input signal            off once the radar is stale
//
// A radar that is merely waiting for its first verdict, with no input
// signal either, has no opinion yet ("").
func (p *Policy) Want(now time.Time, in Input, unknownFor time.Duration) (want, why string) {
	switch in {
	case InputAway:
		return "off", "no input for an hour"
	case InputActive:
		return "on", "input"
	}
	if p.presentKnown && !p.SensorStale(now) {
		if in == InputUnknown && p.InputUnknownCap > 0 && unknownFor >= p.InputUnknownCap {
			return "off", "no input signal for " + p.InputUnknownCap.String()
		}
		if p.present {
			return "on", "radar present"
		}
		return "off", whyRadarAway
	}
	switch in {
	case InputQuiet:
		return "on", "radar out, input recent"
	case InputIdle:
		return "off", "radar out, input idle"
	}
	if p.SensorStale(now) {
		return "off", "radar out, no input signal"
	}
	return "", "waiting for a verdict"
}

const whyRadarAway = "radar away"

// HoldBlack applies Blackout to Want's answer: a radar "off" within
// Blackout of the desk emptying becomes no opinion, so the TV stays as it
// is (on, black) and a return costs nothing. Anything else passes through.
func (p *Policy) HoldBlack(want, why string, now time.Time) (string, string) {
	if p.InBlackout(want, why, now) {
		return "", "radar away, screen black until the blackout ends"
	}
	return want, why
}

// InBlackout says whether HoldBlack would hold this answer.
func (p *Policy) InBlackout(want, why string, now time.Time) bool {
	return want == "off" && why == whyRadarAway && p.Blackout > 0 &&
		!p.present && now.Sub(p.presentAt) < p.Blackout
}

// Decide turns Want's answer into an action given the TV state tv-screen.sh
// last recorded ("on", "off", or anything else for not confirmed either
// way), with backoff between repeated attempts. "" means nothing to do.
func (p *Policy) Decide(want, tv string, now time.Time, busy bool) string {
	if busy || want == "" {
		return ""
	}
	if tv == want {
		p.attempts = 0
		return ""
	}
	if want != p.lastDecision {
		p.attempts = 0
	}
	if p.attempts > 0 {
		wait := p.MinBackoff << (p.attempts - 1)
		if wait > p.MaxBackoff || wait <= 0 {
			wait = p.MaxBackoff
		}
		if now.Sub(p.lastAttempt) < wait {
			return ""
		}
	}
	p.lastDecision = want
	p.lastAttempt = now
	p.attempts++
	return want
}

// Reset forgets timing state (used after resume from suspend, when the last
// reading is arbitrarily old).
func (p *Policy) Reset() {
	p.lastFrame = time.Time{}
	p.rawSince = time.Time{}
	p.raw = false
	p.presentKnown = false
	p.absentSince = time.Time{}
	p.attempts = 0
}
