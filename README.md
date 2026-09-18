# deskpresence

Turns the TV off when nobody is at the desk and back on the moment someone
is. Driven by an LD2410C 24 GHz mmWave presence sensor on a CP2102 USB UART.
Replaces swayidle blanking: idle inhibitors, keyboard activity and playing
video are irrelevant, only bodies count.

## Invariant

    TV on  <=>  someone within max-distance of the sensor

"Someone" is the sensor's target state (moving or stationary) debounced by
`-debounce` (default 500 ms) on the way in and `-absence` (default 60 s) on
the way out. Single-frame blips neither count as presence nor restart the
absence clock. When the sensor is silent for `-stale` (5 s) the daemon holds
and takes no action.

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

## Cutover from swayidle

Once a day of `journalctl --user -u deskpresence` shows clean on/off pairs,
drop `~/.config/hypr/idle.sh` from `hyprland.lua` and kill the running
swayidle. This daemon already covers before-sleep/after-resume. Until then
both run; the state file makes the double `off` harmless.
