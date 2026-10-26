# How it works

How the daemon reads the LD2410C, decides, and drives the TV. The [README](../README.md#how-it-works) has the short version.

The LD2410C reports ten times a second. In engineering mode each report
carries a moving-energy and a static-energy value (0 to 100) for each of nine
range gates, 75 cm apart. The daemon reads only the moving energy of the
first few gates, the ones that cover the chair:

- **Present** when moving energy in any of the first `-near-gates` gates
  reaches the threshold in `-confirm-frames` (2) frames within
  `-confirm-window` (4 s), held for `-debounce` (500 ms) before the TV comes
  on. A single noisy frame neither counts as presence nor restarts the
  absence clock.
- **Absent** when that has not been true for `-absence` (10 to 60 s).
- **The threshold learns the room.** The empty-room noise drifts (a 99.5th
  percentile of 27 after the module swap, 41 a week later), so the daemon
  keeps six hours of near-gate energy from moments it is sure the room is
  empty: its own "away" with the TV off (frames under the threshold only),
  or an hour without input (every frame, so the threshold can rise). The
  threshold is that noise's 99.5th percentile plus 4, never under
  `-energy-min` and never over 70, once 3000 frames (five minutes) are
  held. It is saved in `-floor-file` and every change is logged. When a
  long sit at the desk ages those frames out, the last learned threshold
  stands until fresh ones replace it, rather than dropping to the minimum.
- **Radar out** when the sensor has been silent for `-stale` (5 s). Input idle
  decides: on while the keyboard or mouse was used in the last 10 minutes,
  off after that. It no longer holds the TV in whatever state it was.

The whole decision (`Policy.Want`):

| input | radar | TV |
|---|---|---|
| away (1 h without input) | anything | off, until input returns |
| active (< 20 s) | anything | on |
| quiet or idle | healthy | the radar's verdict |
| quiet (20 s to 10 min) | out | on |
| idle | out | off |
| unknown (hypridle not running) | healthy | the radar's verdict for at most `-input-cap` (1 h), then off, with an alert |
| unknown | out | off |

Why not the module's own presence output or its summary distance? The
summary distance smears about 80 cm past a seated body, the static channel
is blind inside 150 cm and saturated by the room beyond it, and the module's
built-in verdict flickers. Per-gate moving energy at the chair is the one
signal that separates a person sitting still from an empty room. Basic
frames (no gate data) fall back to "target within `-max-distance`".

The actuator is any script that takes `on` or `off`. The daemon compares the
desk state with the TV state file every 250 ms and acts whenever they
disagree, with a 30 s to 10 min backoff for a failing actuator. It is
level-triggered: if something else turns the TV off while you are reading,
it comes back on within a few seconds. The actuator's exit code says how it
went (0 done, 1 failed, 2 abandoned because the decision changed, 3 TV
unreachable, state unknown); the state file stays the record.

Around the edges:

- `-fade` (5 s) dims the screen and PipeWire streams before absence latches,
  so the cut is not a surprise. Streams listed in `-audio-exclude` are left
  alone. Volumes are remembered by application and restored on return.
- `-blackout` (off by default; the unit uses 10 min) keeps the TV on behind
  the faded-out black screen for that long after the desk empties, and only
  then powers it off. A black OLED is dark pixels, so this costs almost
  nothing, and a return inside the window is instant. It exists because this
  LG falls into a deeper standby after a few minutes off, and a wake from
  there is a 17 to 30 s boot (2026-10-03: 21 of 21 wakes after 15+ min off
  were slow, 0 of 99 under 2 min). Only the radar's own away waits: an hour
  without input or a dead sensor still turns the TV off at once. The hooks
  follow the decision, not the TV, so the light still goes off when you
  leave.
- MPRIS players are paused on leave and resumed on return (`-no-mpris` to
  disable).
- A logind delay inhibitor turns the TV off before suspend. After resume the
  policy resets and the sensor decides again.
- A dead sensor is the dangerous case on an OLED, because nothing blanks it.
  After `-alert-after` (2 min) of silence the daemon speaks and posts a
  critical notification, then repeats hourly. A sensor that has never been
  seen since start does not alert, so the unit can sit enabled before the
  hardware is plugged in.
