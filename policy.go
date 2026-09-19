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
	MaxDistance uint16        // cm; 0 = any (basic frames only, see NearGates)
	// Engineering frames carry per-gate energies, and those are the honest
	// signal: someone at the desk is *moving* energy in the near gates
	// (typing, breathing, shifting), while the room's phantom is *static*
	// energy in the far ones. When a frame has gate data, raw presence is
	// "max moving energy over gates [0, NearGates) >= EnergyMin"; the
	// module's own summary distance smears 80 cm past the body and is
	// ignored. NearGates 0 disables this and falls back to MaxDistance.
	NearGates  int
	EnergyMin  int
	StaleAfter time.Duration // no frames this long -> sensor unknown, no actions
	MinBackoff time.Duration
	MaxBackoff time.Duration

	lastFrame    time.Time
	rawSince     time.Time // start of the current raw-present run
	raw          bool
	confirmedAt  time.Time // last time a raw-present run reached Debounce
	present      bool
	presentAt    time.Time // when the debounced verdict last flipped
	presentKnown bool
	lastDecision string // last action requested
	lastAttempt  time.Time
	attempts     int // consecutive attempts of lastDecision
}

// Observe feeds one frame; it reports whether the raw (undebounced) reading
// flipped, so the caller can log what the sensor saw at the edge.
func (p *Policy) Observe(f Frame, now time.Time) (flipped bool) {
	p.lastFrame = now
	raw := p.rawPresent(f)
	flipped = raw != p.raw
	if raw && !p.raw {
		p.rawSince = now
	}
	p.raw = raw
	if !p.presentKnown {
		// First reading seeds the debounced state immediately so a fresh start
		// with someone in the chair does not wait out the absence timer.
		p.present = raw
		p.presentAt = now
		p.presentKnown = true
		if raw {
			p.confirmedAt = now
		}
		return true
	}
	// Only a raw-present run that lasts Debounce counts as presence. A single
	// spurious frame therefore neither flips us to present nor restarts the
	// absence clock, which runs from the last *confirmed* presence.
	if raw && now.Sub(p.rawSince) >= p.Debounce {
		p.confirmedAt = now
		if !p.present {
			p.presentAt = now
		}
		p.present = true
	}
	if !raw && p.present && now.Sub(p.confirmedAt) >= p.Absence {
		p.present = false
		p.presentAt = now
	}
	return flipped
}

func (p *Policy) rawPresent(f Frame) bool {
	if p.NearGates > 0 && len(f.MovingGates) > 0 {
		n := min(p.NearGates, len(f.MovingGates))
		for _, e := range f.MovingGates[:n] {
			if int(e) >= p.EnergyMin {
				return true
			}
		}
		return false
	}
	return f.Present() && (p.MaxDistance == 0 || f.Distance() <= p.MaxDistance)
}

// Present reports the debounced state and whether it is known at all.
func (p *Policy) Present() (present, known bool) {
	return p.present, p.presentKnown
}

// PresentSince is when the debounced verdict last changed.
func (p *Policy) PresentSince() time.Time { return p.presentAt }

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
