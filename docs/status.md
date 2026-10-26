# Status files

What `status.json` and `history.json` hold, for a bar, a chip or a script.

`status.json` is rewritten on every change and is what a bar or chip reads:

    rule       near_gates, energy_min (the live threshold), absence_ms,
               max_distance, confirm_frames, confirm_window_ms,
               floor_learned, floor_frames
    present    the daemon's verdict
    known      false until the first frame
    since      ms timestamp of the last verdict change
    fade       0 to 1, how far into the pre-absence dim
    sensor_ok  false when frames have stopped
    tv         on, off, or "" when the state file is missing
    busy       actuator running
    paused     pause file present
    state      the sensor's own target state (0 none, 1 moving, 2 static, 3 both)
    distance   the sensor's summary distance in cm

`history.json` beside it holds the last 60 s at 2 Hz: `near[]` (max
near-gate moving energy per bin), `present[]`, `threshold` and `bin_ms`.
