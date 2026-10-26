# Calibration

How to pick thresholds for your chair and room, and what was measured at this desk. The [README](../README.md#configuration) has the unit setup.

Thresholds depend on the chair, the distance and the room, so measure
rather than guess. With the live view open:

1. Sit still for a minute. Read the highest moving energy in the near gates
   (the rows above the dashed line). Breathing and small movements keep it
   well above zero.
2. Walk out of the room for 30 s. Read the highest value the same rows reach
   with nobody there.
3. Pick `-energy-min` between the two, closer to the empty-room number. It
   is the floor under the learned threshold, which will rise above it if
   the room is noisier.
4. Pick `-absence` for the longest gap a still reader leaves between
   frames above the threshold, times two. A seated person's moving energy
   spikes every few seconds, but a four-minute recording of reading with
   hands off the desk showed one gap of 10 s, so 10 s is not enough.
   `-fade` starts dimming before absence latches; one breath cancels it.

Measured at this desk, module about 1 m from the chair:

| | Gates 0 and 1 (0 to 150 cm) | Gate 2 (150 to 225 cm) |
|---|---|---|
| Typing | peaks 100 every few seconds, mean 55 to 73 | 50 to 100 |
| Reading, hands off | peaks 33 to 53 every 5 s, longest gap above 38 was 10 s | |
| Room empty | mostly under 30, single frames of 32 to 38 | blips of 35 to 86 |

Which gave `-near-gates 2 -energy-min 38 -absence 20s -fade 8s`. By
2026-09-27 the empty room reached 41 at the 99.5th percentile and threw a
frame over 38 every 10 to 20 s, so the verdict never went away. A
10-minute empty recording and a 15-minute still one gave the current rule:

| rule | empty room: presence refreshes in 575 s | seated: longest gap |
|---|---|---|
| any frame at 38 | 57 | 10 s |
| two frames at 45 within 4 s | 8, the longest quiet stretch 293 s | 17.6 s |

Hence the unit file's `-energy-min 40 -absence 30s -fade 8s`, with the
learned threshold (45 from that recording) on top. On 2026-10-18 the empty
room read a median of 26 and peaks of 44 in gate 1 (32 at the 99th
percentile the evening before), so the unit's minimum went to 46. That spike passed,
and the learner had been reading arrival and input-away body frames as empty room; with
those kept out (`floorTeaches`), the room learns about 33, and on 2026-10-21 the minimum
went back to 40, checked by two hands-off minutes seated (held) and a walk-out (absent
once the away timer ran, light off; light on 6 s after sitting back down). An away timer longer
than a minute would rarely expire in an empty room, so the daemon clamps it
at 60 s; long still stretches are what `hold` and input activity are for.
Static energy does not help here: the module reports none for gates 0 and
1, and gates 2 and 3 read 100 with the room empty.
The third gate was in the rule at first; it turned out to carry energy from
beyond the desk (a body in the next room reads through drywall), and one
walk-away test kept the TV on for a full minute because of it. Keep the
near gates to where the chair actually is and let the empty-room maximum
set the threshold.

If a gate beyond the chair picks up the room (a fan, a corridor), lower its
sensitivity in the module rather than widening the daemon's rule:

    deskpresence config sens <gate|all> <movingSens> <staticSens>
    deskpresence config gates <maxMovingGate> <maxStaticGate> <unmannedSeconds>
    deskpresence config factory

A sensitivity of 100 disables a gate. Settings persist in the module.
