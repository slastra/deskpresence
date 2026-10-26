# Flags

Every flag the daemon takes. The unit files in `systemd/` show the values in use.

| Flag | Default | Purpose |
|---|---|---|
| `-sensor` | `ld2410` | presence source: `ld2410` (mmWave on a USB UART) or `tof` (the Lenovo VL53L1 on the sensor hub) |
| `-port` | `/dev/serial/by-id/*CP210*` | serial device, glob allowed |
| `-baud` | `256000` | the module default |
| `-near-gates` | `3` | gates (75 cm each) that count as the desk; 0 uses `-max-distance` |
| `-energy-min` | `40` | the lowest the learned threshold may go (moving energy in a near gate) |
| `-confirm-frames` | `2` | frames at or over the threshold within `-confirm-window` that count as presence; 1 = any frame |
| `-confirm-window` | `4s` | see `-confirm-frames` |
| `-floor-file` | `~/.local/state/deskpresence/floor.json` | learned empty-room noise, six hours of 10-minute histograms |
| `-max-distance` | `250` | basic frames only: ignore targets farther than this (cm) |
| `-debounce` | `500ms` | presence must hold this long before the TV comes on |
| `-absence` | `60s` | desk must be empty this long before the TV goes off |
| `-stale` | `5s` | no frames for this long means the radar is out and input idle decides |
| `-input-file` | `$XDG_RUNTIME_DIR/deskpresence.input` | input level hypridle writes (`deskpresence input active\|quiet\|idle\|away`) |
| `-input-cap` | `1h` | with no input signal (hypridle down), the radar alone keeps the TV on at most this long |
| `-fade` | `5s` | start dimming this long before absence latches; 0 disables |
| `-blackout` | `0` | after the desk empties, keep the TV on behind a black screen this long before powering it off; 0 = off at once |
| `-alert-after` | `2m` | sensor silent this long triggers the spoken and desktop alert |
| `-alert-text` | `Presence sensor offline. The screen now follows keyboard and mouse idle.` | what that alert says |
| `-script` | `~/.config/hypr/scripts/tv-screen.sh` | actuator, called with `on` or `off` |
| `-hooks` | none | commands run when the decision turns on or off (arrive and leave, even while `-blackout` holds the TV), comma separated; `{}` becomes `on` or `off`, otherwise the verb is appended |
| `-state` | `~/.config/lgtv/state` | TV state file the actuator writes |
| `-status` | `~/.local/state/deskpresence/status.json` | status output |
| `-pause-file` | `$XDG_RUNTIME_DIR/deskpresence.pause` | while present, observe but never act, except that an hour without input still turns the TV off |
| `-hold-file` | `$XDG_RUNTIME_DIR/deskpresence.hold` | unix seconds; observe only until then, then removed (`deskpresence hold 30m`); the one thing that beats an hour without input |
| `-absence-file` | `~/.local/state/deskpresence/absence` | overrides `-absence` while the daemon runs, seconds or a duration, 10 to 60 s (`deskpresence absence 45s`) |
| `-audio-flag` | `~/.local/state/deskpresence/audio-follow` | reads `off` to leave players and volumes alone |
| `-http` | `127.0.0.1:7391` | live view address; empty disables |
| `-no-audio-fade` | | leave PipeWire streams alone |
| `-audio-exclude` | `emotune,speech-dispatcher` | application names never faded |
| `-no-mpris` | | do not pause or resume players |
| `-dry-run` | | log actions instead of running the actuator |
| `-replay` | | synthesise frames, e.g. `present:5s,absent:70s` |
| `-verbose` | | log every frame |
