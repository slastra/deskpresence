package main

import (
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
	MaxDistance uint16        // cm; 0 = any
	StaleAfter  time.Duration // no frames this long -> sensor unknown, no actions
	MinBackoff  time.Duration
	MaxBackoff  time.Duration

	lastFrame    time.Time
	rawSince     time.Time // start of the current raw-present run
	raw          bool
	confirmedAt  time.Time // last time a raw-present run reached Debounce
	present      bool
	presentKnown bool
	lastDecision string // last action requested
	lastAttempt  time.Time
	attempts     int // consecutive attempts of lastDecision
}

func (p *Policy) Observe(f Frame, now time.Time) {
	p.lastFrame = now
	raw := f.Present() && (p.MaxDistance == 0 || f.Distance() <= p.MaxDistance)
	if raw && !p.raw {
		p.rawSince = now
	}
	p.raw = raw
	if !p.presentKnown {
		// First reading seeds the debounced state immediately so a fresh start
		// with someone in the chair does not wait out the absence timer.
		p.present = raw
		p.presentKnown = true
		if raw {
			p.confirmedAt = now
		}
		return
	}
	// Only a raw-present run that lasts Debounce counts as presence. A single
	// spurious frame therefore neither flips us to present nor restarts the
	// absence clock, which runs from the last *confirmed* presence.
	if raw && now.Sub(p.rawSince) >= p.Debounce {
		p.confirmedAt = now
		p.present = true
	}
	if !raw && p.present && now.Sub(p.confirmedAt) >= p.Absence {
		p.present = false
	}
}

// Present reports the debounced state and whether it is known at all.
func (p *Policy) Present() (present, known bool) {
	return p.present, p.presentKnown
}

func (p *Policy) SensorStale(now time.Time) bool {
	return p.lastFrame.IsZero() || now.Sub(p.lastFrame) > p.StaleAfter
}

// Decide returns "on", "off" or "" given the TV state tv-screen.sh last
// recorded ("on", "off" or "" for unknown).
func (p *Policy) Decide(tv string, now time.Time, busy bool) string {
	if busy || !p.presentKnown || p.SensorStale(now) {
		return ""
	}
	want := "off"
	if p.present {
		want = "on"
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
	p.attempts = 0
}
