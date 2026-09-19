# deskpresence

Turns the TV off when nobody is at the desk and back on the moment someone
is. Driven by an LD2410C 24 GHz mmWave presence sensor on a CP2102 USB UART.
Replaces swayidle blanking: idle inhibitors, keyboard activity and playing
video are irrelevant, only bodies count.

## Invariant

    TV on  <=>  someone within max-distance of the sensor

"Someone" is, in engineering mode, moving energy of at least `-energy-min`
in any of the first `-near-gates` gates (75 cm each). The module's own
summary distance smears ~80 cm past a seated body and its stationary channel
is blind inside 150 cm and saturated by the room beyond it, so neither is
used. Basic frames (no gate data) fall back to target-within-`-max-distance`.
Debounced by `-debounce` (500 ms) on the way in and `-absence` on the way
out. Single-frame blips neither count as presence nor restart the absence
clock. When the sensor is silent for `-stale` (5 s) the daemon holds.

Measured at this desk (2026-09-18): sitting still, near-gate moving energy
peaks above 35 at least every 2.4 s; with the room empty it never exceeds 26.
The unit therefore runs `-energy-min 35 -absence 10s`.

The actuator is `~/.config/hypr/scripts/tv-screen.sh on|off`, which
serialises on its own lock and writes the state it achieved to
`~/.config/lgtv/state`. The daemon compares desk and TV state every 250 ms
and acts whenever they disagree, with 30 s → 10 min backoff for a failing
actuator.

Suspend: a logind delay inhibitor is held; on `PrepareForSleep` the TV is
powered off before the machine sleeps, and after resume the policy resets and
lets the sensor decide.

## Wiring

    CP2102 5V  -> LD2410C VCC
    CP2102 GND -> LD2410C GND
    CP2102 TXD -> LD2410C RX
    CP2102 RXD -> LD2410C TX

256000 baud 8N1, the module default. Antenna patches face the chair.

## Run

    go install .
    systemctl --user enable --now deskpresence

    deskpresence -verbose                     # watch frames
    deskpresence -dry-run -replay present:5s,absent:70s,present:3s -absence 60s
    touch $XDG_RUNTIME_DIR/deskpresence.pause # observe only, never act
    cat ~/.local/state/deskpresence/status.json

## Sensor failure

The point of this is the OLED. A dead sensor (unplugged, wedged) means
nothing blanks it, so after `-alert-after` (2 min) of silence the daemon
speaks and posts a critical notification, then repeats hourly. A sensor
that has never been seen since the daemon started does not alert, so the
unit can sit enabled before the hardware arrives.

## Cutover from swayidle

Once a day of `journalctl --user -u deskpresence` shows clean on/off pairs,
swayidle stops being the primary. Recommended end state is not to delete it
but to demote it: change the timeout in `~/.config/hypr/idle.sh` from 600 to
1800 so it is a last-resort burn-in guard if this daemon or the sensor dies.
If it fires while someone is reading, this daemon turns the TV back on within
a few seconds (level-triggered, not edge). Before-sleep/after-resume hooks can
stay, they never fire on a machine that does not suspend. Until the cutover
both run at full strength; the state file makes the double `off` harmless.
