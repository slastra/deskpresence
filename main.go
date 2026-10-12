// deskpresence: turn the TV off when nobody is at the desk and back on the
// moment someone is, driven by an LD2410C mmWave sensor on a USB UART (or,
// with -sensor tof, a laptop's VL53L1 time-of-flight sensor; see tof.go).
//
// It replaces input-idle blanking (swayidle): idle inhibitors, key presses and
// video playback are irrelevant, only bodies count. The actuator is the
// existing tv-screen.sh, which serialises on its own lock and records the TV
// state it last achieved in a state file this daemon reads back.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/godbus/dbus/v5"
	"go.bug.st/serial"
	"golang.org/x/sys/unix"
)

type config struct {
	port, script, stateFile, statusFile, pauseFile string
	holdFile, audioFlag, absenceFile, inputFile    string
	hooks                                          string
	httpAddr                                       string
	baud                                           int
	absence, debounce, stale, alertAfter, fade     time.Duration
	blackout                                       time.Duration
	inputCap, confirmWindow                        time.Duration
	confirmFrames                                  int
	floorFile                                      string
	maxDistance                                    uint
	nearGates, energyMin                           int
	dryRun, verbose, noMpris, noAudioFade          bool
	audioExclude                                   string
	replay                                         string
	sensor, alertText                              string
}

func main() {
	home, _ := os.UserHomeDir()
	runtime := os.Getenv("XDG_RUNTIME_DIR")
	if runtime == "" {
		runtime = os.TempDir()
	}
	var c config
	flag.StringVar(&c.sensor, "sensor", "ld2410", "presence source: ld2410 (mmWave on a USB UART) or tof (the Lenovo VL53L1 on the sensor hub)")
	flag.StringVar(&c.port, "port", "/dev/serial/by-id/*CP210*", "serial device (glob ok)")
	flag.IntVar(&c.baud, "baud", 256000, "serial baud rate")
	flag.DurationVar(&c.absence, "absence", 60*time.Second, "how long the desk must be empty before the TV goes off")
	flag.DurationVar(&c.debounce, "debounce", 500*time.Millisecond, "how long presence must hold before the TV comes on")
	flag.DurationVar(&c.stale, "stale", 5*time.Second, "no frames for this long -> no opinion, no actions")
	flag.UintVar(&c.maxDistance, "max-distance", 250, "basic frames only: ignore targets farther than this many cm (0 = any)")
	flag.IntVar(&c.nearGates, "near-gates", 3, "engineering frames: gates (75 cm each) that count as the desk; 0 = use max-distance")
	flag.IntVar(&c.energyMin, "energy-min", 40, "engineering frames: the lowest the learned threshold may go (moving energy in a near gate)")
	flag.IntVar(&c.confirmFrames, "confirm-frames", 2, "frames at or over the threshold within -confirm-window that count as presence (1 = any frame)")
	flag.DurationVar(&c.confirmWindow, "confirm-window", 4*time.Second, "see -confirm-frames")
	flag.StringVar(&c.floorFile, "floor-file", filepath.Join(home, ".local/state/deskpresence/floor.json"), "learned empty-room noise (six hours of histograms)")
	flag.StringVar(&c.script, "script", filepath.Join(home, ".config/hypr/scripts/tv-screen.sh"), "actuator")
	flag.StringVar(&c.hooks, "hooks", "", "commands run when the decision turns on|off (arrive/leave, even while -blackout keeps the TV on), comma separated; {} is replaced by on|off, else it is appended (a light switch)")
	flag.StringVar(&c.stateFile, "state", filepath.Join(home, ".config/lgtv/state"), "TV state file written by the actuator")
	flag.StringVar(&c.statusFile, "status", filepath.Join(home, ".local/state/deskpresence/status.json"), "status output for bars/chips")
	flag.StringVar(&c.pauseFile, "pause-file", filepath.Join(runtime, "deskpresence.pause"), "while this exists, observe but never act")
	flag.StringVar(&c.holdFile, "hold-file", filepath.Join(runtime, "deskpresence.hold"), "unix seconds; until then, observe but never act (deskpresence hold 30m)")
	flag.StringVar(&c.absenceFile, "absence-file", filepath.Join(home, ".local/state/deskpresence/absence"), "overrides -absence while present (seconds or a duration; deskpresence absence 45s)")
	flag.StringVar(&c.inputFile, "input-file", filepath.Join(runtime, "deskpresence.input"), "input idle level hypridle writes (deskpresence input active|quiet|idle|away)")
	flag.DurationVar(&c.inputCap, "input-cap", time.Hour, "with no input signal (hypridle down), the radar alone keeps the TV on at most this long")
	flag.StringVar(&c.audioFlag, "audio-flag", filepath.Join(home, ".local/state/deskpresence/audio-follow"), "reads \"off\" -> leave players and volumes alone")
	flag.BoolVar(&c.dryRun, "dry-run", false, "log actions instead of running the actuator")
	flag.BoolVar(&c.verbose, "verbose", false, "log every frame")
	flag.DurationVar(&c.fade, "fade", 5*time.Second, "start dimming the screen this long before absence latches (0 = off)")
	flag.DurationVar(&c.blackout, "blackout", 0, "after the desk empties, keep the TV on behind a black screen this long before powering it off (0 = off at once)")
	flag.DurationVar(&c.alertAfter, "alert-after", 2*time.Minute, "sensor silent this long -> spoken/desktop alert (OLED is unguarded)")
	flag.StringVar(&c.alertText, "alert-text", "Presence sensor offline. The screen now follows keyboard and mouse idle.", "what that alert says")
	flag.StringVar(&c.httpAddr, "http", "127.0.0.1:7391", "serve the live sensor view here (empty = off)")
	flag.BoolVar(&c.noAudioFade, "no-audio-fade", false, "do not fade PipeWire streams with the screen")
	flag.StringVar(&c.audioExclude, "audio-exclude", "emotune,speech-dispatcher", "application.name substrings never faded")
	flag.BoolVar(&c.noMpris, "no-mpris", false, "do not pause/resume media players on leave/return")
	flag.StringVar(&c.replay, "replay", "", "synthesise frames instead of reading the port, e.g. present:5s,absent:70s,present:3s")
	if len(os.Args) > 1 && os.Args[1] == "config" {
		// flags after the subcommand: deskpresence config [-port X] show
		fs := flag.NewFlagSet("config", flag.ExitOnError)
		port := fs.String("port", "/dev/serial/by-id/*CP210*", "serial device (glob ok)")
		baud := fs.Int("baud", 256000, "serial baud rate")
		_ = fs.Parse(os.Args[2:])
		runConfig(fs.Args(), *port, *baud)
		return
	}
	if len(os.Args) > 1 && os.Args[1] == "absence" {
		// deskpresence absence 45s | off: the away timer, changeable live
		runAbsence(os.Args[2:], filepath.Join(home, ".local/state/deskpresence/absence"))
		return
	}
	if len(os.Args) > 1 && os.Args[1] == "input" {
		// deskpresence input active|quiet|idle|away: what hypridle's listeners run
		runInput(os.Args[2:], filepath.Join(runtime, "deskpresence.input"))
		return
	}
	if len(os.Args) > 1 && os.Args[1] == "hold" {
		// deskpresence hold 30m | off: a timed pause the daemon expires itself
		runHold(os.Args[2:], filepath.Join(runtime, "deskpresence.hold"))
		return
	}
	flag.Parse()
	log.SetFlags(0)

	pol := &Policy{
		Absence: c.absence, Debounce: c.debounce, MaxDistance: uint16(c.maxDistance),
		NearGates: c.nearGates, EnergyMin: c.energyMin, Fade: c.fade, Blackout: c.blackout,
		StaleAfter: c.stale, MinBackoff: 30 * time.Second, MaxBackoff: 10 * time.Minute,
		InputUnknownCap: c.inputCap,
		ConfirmFrames:   c.confirmFrames, ConfirmWindow: c.confirmWindow,
	}
	floor := newFloorLearner(c.floorFile, c.energyMin)
	if t, n, ok := floor.threshold(); ok {
		pol.EnergyMin = t
		log.Printf("floor: threshold %d from %d empty-room frames", t, n)
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	frames := make(chan Frame, 64)
	h := newHub()
	if c.httpAddr != "" {
		go h.serve(c.httpAddr)
	}
	// The ToF reader switches its sensor off on the way out; wait for that
	// before exiting, or a stopped daemon leaves the sensor running.
	readerDone := make(chan struct{})
	var gate *sensorGate // ToF only: off while input is active
	switch {
	case c.replay != "":
		go replayFrames(ctx, c.replay, frames)
	case c.sensor == "tof":
		pol.RangeCM = tofRangeCM
		gate = newSensorGate()
		go func() { readToF(ctx, frames, gate); close(readerDone) }()
	case c.sensor == "ld2410":
		go readSerial(ctx, c, frames, h)
	default:
		log.Fatalf("unknown -sensor %q (want ld2410 or tof)", c.sensor)
	}

	sleep := watchSleep(ctx) // nil channel when logind is unavailable

	act := &actuator{script: c.script, stateFile: c.stateFile, dryRun: c.dryRun}
	for _, h := range strings.Split(c.hooks, ",") {
		if h = strings.TrimSpace(h); h != "" {
			act.hooks = append(act.hooks, h)
		}
	}
	var media *mpris
	if !c.noMpris {
		media = newMpris()
	}
	var snd *audio
	if !c.noAudioFade {
		snd = newAudio(c.audioExclude)
	}
	var lastPresent, lastKnown bool
	// The hooks follow the decision, not the TV: during a blackout the desk
	// is empty and the light should say so while the TV waits. Seeded from
	// the TV so a restart does not replay a hello.
	hookWant := act.tvState()
	blackingOut := false
	done := make(chan string, 1)
	var (
		lastStatus  string
		lastWrite   time.Time
		lastFrame   Frame
		pausedNoted bool
		// the input layer (input.go)
		hypridle          = &processAlive{comm: "hypridle"}
		inputNoted        Input
		inputChecked      bool
		inputUnknownSince time.Time
		lastInputAlert    time.Time
		audioNoted        = true // audio follow, as of the last tick
		staleNoted        bool
		staleSince        time.Time
		lastAlert         time.Time
		everSeen          bool // never alert about a sensor that was never plugged in
		flipWindow        time.Time
		flips             int
		gateOpened        time.Time // the sensor gets -stale to report after waking
	)
	// The tick's period comes from tickPeriod; arrivals and input writes
	// tick at once whatever it is.
	tick := time.NewTicker(tickFast)
	defer tick.Stop()
	inputChanged := watchInput(ctx, c.inputFile)
	hist := newHistory(filepath.Join(filepath.Dir(c.statusFile), "history.json"), 500*time.Millisecond, 120)
	log.Printf("deskpresence up: sensor=%s absence=%s blackout=%s debounce=%s near-gates=%d energy-min=%d max=%dcm tv=%q dry-run=%v", c.sensor, c.absence, c.blackout, c.debounce, c.nearGates, c.energyMin, c.maxDistance, act.tvState(), c.dryRun)

	for {
		select {
		case <-ctx.Done():
			if c.sensor == "tof" && c.replay == "" {
				select {
				case <-readerDone:
				case <-time.After(2 * time.Second):
				}
			}
			return
		case f := <-frames:
			lastFrame = f
			now := time.Now()
			flipped := pol.Observe(f, now)
			h.publish("frame", toEvent(f, now))
			if pr, known := pol.Present(); known {
				hist.observe(f, c.nearGates, pr, pol.EnergyMin, now)
			}
			// Confidently empty: the radar's own away with the TV off, or an
			// hour without input. Only these teach the floor.
			if pr, known := pol.Present(); len(f.MovingGates) > 0 &&
				((known && !pr && (act.tvState() == "off" || blackingOut)) || inputNoted == InputAway) {
				floor.observe(nearEnergy(f, c.nearGates), now)
			}
			if c.verbose {
				log.Printf("frame: %s", f)
			} else if pr, _ := pol.Present(); flipped && !pr {
				// Raw edges while the verdict is "away" are the interesting
				// ones (arrivals, phantoms). While seated the energy rule
				// flips at frame rate and the live view/history carry that.
				flips++
				if now.Sub(flipWindow) > time.Minute {
					flipWindow, flips = now, 1
				}
				if flips <= 20 {
					log.Printf("raw: %s", f)
				} else if flips == 21 {
					log.Printf("raw: flapping, muting edges for a minute")
				}
			}
			staleNoted = false
			everSeen = true
			if pr, known := pol.Present(); flipped && (!known || !pr) {
				tick.Reset(time.Millisecond) // the tick resets the period itself
			}
		case <-inputChanged:
			tick.Reset(time.Millisecond) // read it now; the tick resets the period
		case a := <-done:
			act.busy = false
			log.Printf("actuator: %s finished, tv=%q", a, act.tvState())
			tick.Reset(time.Millisecond) // a decision held while it ran goes out now
		case s := <-sleep:
			if s.before {
				log.Printf("sleep: powering TV off before suspend")
				if !act.busy {
					act.busy = true
					act.fireHooks("off")
					hookWant = "off"
					act.run("off") // synchronous: logind waits on our inhibitor
					act.busy = false
				}
				s.release()
			} else {
				log.Printf("sleep: resumed, tv=%q", act.tvState())
				pol.Reset()
			}
		case now := <-tick.C:
			// A pause file or an unexpired hold both mean observe only: no
			// TV, no fade, no media. The hold is the "keep awake 30 min"
			// button; it expires itself so a forgotten one cannot leave the
			// OLED unguarded.
			// The away timer follows its override file while the daemon runs:
			// the bar's slider writes it, so a film night does not need a
			// restart. A missing or bad file means the -absence flag.
			if d, changed := absenceOverride.read(c.absenceFile, c.absence); changed {
				log.Printf("absence: %s", d)
				pol.Absence = d
			}
			if t, n, ok := floor.threshold(); ok && t != pol.EnergyMin {
				log.Printf("floor: threshold %d -> %d (99.5th percentile of %d empty-room frames + %d)", pol.EnergyMin, t, n, floor.margin)
				pol.EnergyMin = t
			}
			hold := holdUntil(c.holdFile, now)
			_, pauseErr := os.Stat(c.pauseFile)
			paused := pauseErr == nil || !hold.IsZero()
			// The input layer: hypridle's level, trusted only while hypridle
			// runs. Unknown input is timed; Want caps the radar alone.
			in := readInput(c.inputFile, hypridle.get(now))
			if in != inputNoted || !inputChecked {
				log.Printf("input: %s", map[bool]string{true: "unknown (hypridle not running or silent)", false: string(in)}[in == InputUnknown])
				inputNoted, inputChecked = in, true
			}
			if in == InputUnknown {
				if inputUnknownSince.IsZero() {
					inputUnknownSince = now
				}
				if now.Sub(inputUnknownSince) > 5*time.Minute && now.Sub(lastInputAlert) > 6*time.Hour {
					lastInputAlert = now
					alert("Idle guard degraded: hypridle is not running. The radar alone is keeping the screen on, for at most " + c.inputCap.String() + ".")
				}
			} else {
				inputUnknownSince = time.Time{}
			}
			// While input is active Want says "on" without the sensor, so it
			// sleeps; it wakes when input goes quiet, ahead of any fade.
			if gate != nil && gate.set(in != InputActive) {
				if gate.on.Load() {
					gateOpened = now
					log.Printf("tof: input %s, sensor on", in)
				} else {
					log.Printf("tof: input active, sensor off")
				}
			}
			want, why := pol.Want(now, in, map[bool]time.Duration{true: now.Sub(inputUnknownSince), false: 0}[!inputUnknownSince.IsZero()])
			if paused != pausedNoted {
				if paused && !hold.IsZero() {
					log.Printf("hold: observing only until %s", hold.Format(time.Kitchen))
				} else if paused {
					log.Printf("paused: %s exists, observing only", c.pauseFile)
				} else {
					log.Printf("unpaused")
				}
				pausedNoted = paused
			}
			// Audio follow can be switched off from the bar. Switching it
			// off mid-fade or while away hands the volumes straight back.
			audioOn := !flagOff(c.audioFlag)
			if audioOn != audioNoted {
				log.Printf("audio follow: %v", audioOn)
				if !audioOn && snd.Level() > 0 {
					go snd.rampUp(1500 * time.Millisecond)
				}
				audioNoted = audioOn
			}
			// Media follows the debounced verdict, not the TV: pause the
			// moment "away" latches, resume the moment "present" does.
			if pr, known := pol.Present(); known {
				if lastKnown && pr != lastPresent && audioOn && !paused {
					if pr {
						media.resume()
						go snd.rampUp(1500 * time.Millisecond)
					} else {
						snd.apply(1) // silence before the pause so nothing pops
						media.pauseAll()
					}
				}
				lastPresent, lastKnown = pr, true
			}
			// Audio follows the same fade as the screen. While away it holds
			// at zero (the verdict flip above already applied 1); rampUp owns
			// the way back, so skip apply until it has cleared the snapshot.
			if pr, known := pol.Present(); known && pr && !paused && in != InputActive && audioOn && snd != nil {
				lvl := pol.FadeLevel(now)
				if lvl > 0 && lvl < 1 || lvl == 0 && snd.Level() > 0 && snd.Level() < 1 {
					snd.apply(lvl) // rising, or cancelled mid-fade (restore)
				}
			}
			// A hold is the explicit, self-expiring "keep it on" and beats
			// everything. A pause only stops the radar acting: an hour
			// without input still turns the panel off, so a forgotten
			// pause cannot leave the OLED lit overnight.
			if paused && !(hold.IsZero() && in == InputAway) {
				want = ""
			}
			if want != "" && want != hookWant {
				act.fireHooks(want)
				hookWant = want
			}
			blackingOut = pol.InBlackout(want, why, now)
			want, why = pol.HoldBlack(want, why, now)
			{
				gated := gate != nil && (!gate.on.Load() || now.Sub(gateOpened) < c.stale)
				if pol.SensorStale(now) && !gated {
					if !staleNoted {
						log.Printf("sensor: no frames for %s, input idle decides", c.stale)
						staleNoted = true
						staleSince = now
					}
					// A dead sensor means nothing blanks the OLED. Be loud,
					// once, then hourly.
					if everSeen && now.Sub(staleSince) > c.alertAfter && now.Sub(lastAlert) > time.Hour {
						lastAlert = now
						alert(c.alertText)
					}
				}
				if a := pol.Decide(want, act.tvState(), now, act.busy); a != "" {
					pr, _ := pol.Present()
					log.Printf("decide: %s (%s; present=%v input=%s, last %s)", a, why, pr, map[bool]string{true: "unknown", false: string(in)}[in == InputUnknown], lastFrame)
					act.busy = true
					go func() { act.run(a); done <- a }()
				}
			}
			if s := status(pol, act, floor, lastFrame, now, paused, hold, audioOn, in, want); statusDue(s, lastStatus, lastWrite, now) {
				writeStatus(c.statusFile, s)
				if s != lastStatus {
					h.publish("status", json.RawMessage(s))
				}
				lastStatus, lastWrite = s, now
			}
			tick.Reset(tickPeriod(pol.FadeLevel(now), snd.Level()))
		}
	}
}

// --- actuator -------------------------------------------------------------

type actuator struct {
	script, stateFile string
	// hooks run with the decision's on|off, in parallel with each other and
	// apart from the script, so a light switch answers at once instead of
	// after the TV's wake (or a blackout). They are fire and forget:
	// presence is the truth, and a hook that fails is logged, never retried.
	hooks  []string
	dryRun bool
	busy   bool
}

func (a *actuator) tvState() string {
	b, err := os.ReadFile(a.stateFile)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(b))
}

func (a *actuator) fireHooks(action string) {
	if a.dryRun {
		log.Printf("dry-run: would run hooks %v %s", a.hooks, action)
		return
	}
	for _, h := range a.hooks {
		go a.hook(h, action)
	}
}

func (a *actuator) run(action string) {
	if a.dryRun {
		log.Printf("dry-run: would run %s %s", a.script, action)
		_ = os.WriteFile(a.stateFile, []byte(action+"\n"), 0o644)
		return
	}
	t0 := time.Now()
	// Bounded: the script's own lock wait and back-off fit well inside
	// this, and a hung script must not leave the daemon busy for good.
	ctx, cancel := context.WithTimeout(context.Background(), actuatorTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, a.script, action)
	cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
	err := cmd.Run()
	took := time.Since(t0).Round(time.Second)
	code := 0
	if ee, ok := err.(*exec.ExitError); ok {
		code = ee.ExitCode()
	} else if err != nil {
		log.Printf("actuator: %s %s did not run to the end after %s: %v", filepath.Base(a.script), action, took, err)
		return
	}
	log.Printf("actuator: %s %s in %s", action, actuatorOutcome(code), took)
}

// actuatorTimeout bounds one tv-screen.sh run.
const actuatorTimeout = 7 * time.Minute

// actuatorOutcome names tv-screen.sh's exit codes. The state file stays
// the daemon's record of the TV; these say why it does or does not match.
func actuatorOutcome(code int) string {
	switch code {
	case 0:
		return "done"
	case 1:
		return "FAILED (the TV may be lit; retrying with backoff)"
	case 2:
		return "abandoned (the decision changed while it ran)"
	case 3:
		return "unconfirmed (TV unreachable; state unknown, retrying with backoff)"
	}
	return fmt.Sprintf("exited %d", code)
}

// hook runs one extra command through sh with a bound, so a device that
// never answers cannot pile up goroutines behind it.
func (a *actuator) hook(h, action string) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	t0 := time.Now()
	// "{}" in the command stands for the action; without it the action is
	// appended (kasactl wants the verb before the address).
	script := h + ` "$0"`
	if strings.Contains(h, "{}") {
		script = strings.ReplaceAll(h, "{}", `"$0"`)
	}
	cmd := exec.CommandContext(ctx, "sh", "-c", script, action)
	cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
	if err := cmd.Run(); err != nil {
		log.Printf("hook: %s %s failed after %s: %v", h, action, time.Since(t0).Round(time.Second), err)
		return
	}
	log.Printf("hook: %s %s ok in %s", h, action, time.Since(t0).Round(time.Millisecond))
}

// --- serial ---------------------------------------------------------------

// basicFrameLen is a basic-mode frame on the wire (header, length, 13
// payload bytes, footer), the shortest the radar sends.
const basicFrameLen = 23

// setReadMin sets VMIN on the tty behind f, leaving the rest of the line
// setup (the library's 256000-baud termios2) as it is. Through SyscallConn:
// f.Fd() would put the descriptor back into blocking mode.
func setReadMin(f *os.File, n byte) error {
	rc, err := f.SyscallConn()
	if err != nil {
		return err
	}
	var ierr error
	if err := rc.Control(func(fd uintptr) {
		var t *unix.Termios
		if t, ierr = unix.IoctlGetTermios(int(fd), unix.TCGETS2); ierr != nil {
			return
		}
		t.Cc[unix.VMIN], t.Cc[unix.VTIME] = n, 0
		ierr = unix.IoctlSetTermios(int(fd), unix.TCSETS2, t)
	}); err != nil {
		return err
	}
	return ierr
}

func readSerial(ctx context.Context, c config, out chan<- Frame, h *hub) {
	var lastErr string
	for ctx.Err() == nil {
		dev := resolvePort(c.port)
		if dev == "" {
			warnOnce(&lastErr, "serial: no device matches "+c.port)
			sleepCtx(ctx, 5*time.Second)
			continue
		}
		// Frames are read through our own non-blocking handle, so a read
		// parks on Go's netpoller instead of the library's select() holding
		// an OS thread. It has to be opened before serial.Open, which takes
		// the tty exclusively (TIOCEXCL refuses later opens). The library
		// still owns the line setup: termios belongs to the tty, so its
		// raw 256000-baud mode applies to this handle too.
		rf, err := os.OpenFile(dev, os.O_RDONLY|syscall.O_NOCTTY|syscall.O_NONBLOCK, 0)
		if err != nil {
			warnOnce(&lastErr, fmt.Sprintf("serial: open %s: %v", dev, err))
			sleepCtx(ctx, 5*time.Second)
			continue
		}
		port, err := serial.Open(dev, &serial.Mode{BaudRate: c.baud})
		if err != nil {
			rf.Close()
			warnOnce(&lastErr, fmt.Sprintf("serial: open %s: %v", dev, err))
			sleepCtx(ctx, 5*time.Second)
			continue
		}
		lastErr = ""
		_ = port.SetReadTimeout(300 * time.Millisecond)
		if prm, err := enterEngineering(port); err != nil {
			log.Printf("serial: engineering mode: %v (basic frames only)", err)
		} else {
			h.setParams(prm)
			log.Printf("serial: engineering mode on, static thresholds %v, moving %v", prm.StaticSens, prm.MovingSens)
		}
		log.Printf("serial: reading %s at %d", dev, c.baud)
		// The CP2102 hands a frame over in USB-sized pieces, ~8 per 100 ms
		// frame. With VTIME 0, n_tty reports the tty readable to epoll only
		// once VMIN bytes are queued, so VMIN is kept at what the next frame
		// still needs: one wakeup per frame, and a read that ends mid-frame
		// realigns on the next instead of leaving every frame a frame late.
		vmin, vminOK := 0, true
		stop := context.AfterFunc(ctx, func() { rf.Close() }) // unblocks the read
		var p Parser
		buf := make([]byte, 512)
		for {
			// An unplug hangs the tty up: EOF or EIO, never a silent block.
			n, err := rf.Read(buf)
			if err != nil {
				if ctx.Err() == nil {
					log.Printf("serial: read: %v (reconnecting)", err)
				}
				break
			}
			for _, f := range p.Feed(buf[:n]) {
				select {
				case out <- f:
				default: // consumer is behind; drop, the next frame is 100ms away
				}
			}
			if need := p.Need(); vminOK && need != vmin {
				if err := setReadMin(rf, byte(need)); err != nil {
					log.Printf("serial: VMIN: %v (waking per USB chunk)", err)
					vminOK = false
				}
				vmin = need
			}
		}
		stop()
		rf.Close()
		port.Close()
		sleepCtx(ctx, 2*time.Second)
	}
}

func resolvePort(pattern string) string {
	if !strings.ContainsAny(pattern, "*?[") {
		return pattern
	}
	m, _ := filepath.Glob(pattern)
	if len(m) == 0 {
		return ""
	}
	return m[0]
}

func warnOnce(last *string, msg string) {
	if *last != msg {
		log.Print(msg)
		*last = msg
	}
}

func sleepCtx(ctx context.Context, d time.Duration) {
	select {
	case <-ctx.Done():
	case <-time.After(d):
	}
}

// replayFrames synthesises a scenario at the sensor's 10 Hz for dry runs.
func replayFrames(ctx context.Context, scenario string, out chan<- Frame) {
	var p Parser
	for _, step := range strings.Split(scenario, ",") {
		kind, durS, ok := strings.Cut(strings.TrimSpace(step), ":")
		if !ok {
			log.Fatalf("replay: bad step %q (want present:5s)", step)
		}
		dur, err := time.ParseDuration(durS)
		if err != nil {
			log.Fatalf("replay: %v", err)
		}
		raw := basicFrame(0, 0, 0, 0, 0)
		if kind == "present" {
			raw = basicFrame(2, 0, 110, 0, 70)
		}
		for end := time.Now().Add(dur); time.Now().Before(end) && ctx.Err() == nil; {
			for _, f := range p.Feed(raw) {
				out <- f
			}
			sleepCtx(ctx, 100*time.Millisecond)
		}
	}
	log.Printf("replay: scenario finished")
}

// --- logind ---------------------------------------------------------------

type sleepEvent struct {
	before  bool
	release func()
}

// watchSleep delivers PrepareForSleep events, holding a logind delay inhibitor
// so the TV-off actually lands before the machine suspends. Returns nil (a
// channel that never fires) when the system bus is unavailable.
func watchSleep(ctx context.Context) <-chan sleepEvent {
	conn, err := dbus.ConnectSystemBus()
	if err != nil {
		log.Printf("logind: %v (suspend handling disabled)", err)
		return nil
	}
	mgr := conn.Object("org.freedesktop.login1", "/org/freedesktop/login1")
	inhibit := func() *os.File {
		var fd dbus.UnixFD
		if err := mgr.Call("org.freedesktop.login1.Manager.Inhibit", 0, "sleep", "deskpresence", "powering the TV off", "delay").Store(&fd); err != nil {
			log.Printf("logind: inhibit: %v", err)
			return nil
		}
		return os.NewFile(uintptr(fd), "inhibit")
	}
	lock := inhibit()
	if err := conn.AddMatchSignal(dbus.WithMatchInterface("org.freedesktop.login1.Manager"), dbus.WithMatchMember("PrepareForSleep")); err != nil {
		log.Printf("logind: match: %v (suspend handling disabled)", err)
		return nil
	}
	sig := make(chan *dbus.Signal, 8)
	conn.Signal(sig)
	out := make(chan sleepEvent)
	go func() {
		for {
			select {
			case <-ctx.Done():
				return
			case s := <-sig:
				if len(s.Body) != 1 {
					continue
				}
				before, _ := s.Body[0].(bool)
				if before {
					l := lock
					lock = nil
					out <- sleepEvent{before: true, release: func() {
						if l != nil {
							l.Close()
						}
					}}
				} else {
					lock = inhibit()
					out <- sleepEvent{before: false, release: func() {}}
				}
			}
		}
	}()
	return out
}

func alert(msg string) {
	log.Printf("ALERT: %s", msg)
	home, _ := os.UserHomeDir()
	_ = exec.Command(filepath.Join(home, ".claude/bin/claude-speak"), "--kind", "error", msg).Run()
	_ = exec.Command("notify-send", "-u", "critical", "deskpresence", msg).Run()
}

// --- status ---------------------------------------------------------------

func status(p *Policy, a *actuator, fl *floorLearner, f Frame, now time.Time, paused bool, hold time.Time, audioOn bool, in Input, want string) string {
	pr, known := p.Present()
	_, floorFrames, learned := fl.threshold()
	s := map[string]any{
		"rule": map[string]any{"near_gates": p.NearGates, "energy_min": p.EnergyMin,
			"absence_ms": p.Absence.Milliseconds(), "fade_ms": p.Fade.Milliseconds(), "blackout_ms": p.Blackout.Milliseconds(), "max_distance": p.MaxDistance,
			"confirm_frames": p.ConfirmFrames, "confirm_window_ms": p.ConfirmWindow.Milliseconds(),
			"floor_learned": learned, "floor_frames": floorFrames, "range_cm": p.RangeCM},
		"present": pr,
		"known":   known,
		"since":   p.PresentSince().UnixMilli(),
		// No dim while input is active: the TV cannot go off then, and a
		// dim that never ends in off reads as a fault.
		"fade":         map[bool]float64{true: 0, false: p.FadeLevel(now)}[paused || in == InputActive],
		"sensor_ok":    !p.SensorStale(now),
		"tv":           a.tvState(),
		"busy":         a.busy,
		"paused":       paused,
		"hold_until":   map[bool]int64{true: 0, false: hold.UnixMilli()}[hold.IsZero()],
		"audio_follow": audioOn,
		"input":        map[bool]string{true: "unknown", false: string(in)}[in == InputUnknown],
		"want":         want,
		"state":        f.State,
		"distance":     f.Distance(),
	}
	b, _ := json.Marshal(s)
	return string(b)
}

// holdUntil reads the hold file (unix seconds). An expired or unreadable
// hold is removed and reported as zero.
func holdUntil(path string, now time.Time) time.Time {
	b, err := os.ReadFile(path)
	if err != nil {
		return time.Time{}
	}
	secs, err := strconv.ParseInt(strings.TrimSpace(string(b)), 10, 64)
	if err != nil || secs <= now.Unix() {
		_ = os.Remove(path)
		return time.Time{}
	}
	return time.Unix(secs, 0)
}

// absenceOverride tracks the -absence-file: it is stat'ed every tick and
// re-read only when its mtime moves, so the slider costs nothing at rest.
var absenceOverride fileDuration

type fileDuration struct {
	mtime time.Time
	seen  bool
	value time.Duration
}

// read returns the current absence and whether it changed since last call.
func (a *fileDuration) read(path string, fallback time.Duration) (time.Duration, bool) {
	st, err := os.Stat(path)
	var d time.Duration
	var mt time.Time
	if err == nil {
		mt = st.ModTime()
		if a.seen && mt.Equal(a.mtime) {
			return a.value, false
		}
		d = parseAbsence(path, fallback)
	} else {
		d = fallback
	}
	changed := !a.seen || d != a.value
	a.seen, a.mtime, a.value = true, mt, d
	return d, changed
}

// parseAbsence reads "45", "45s" or "1m30s", clamped to 5 s..30 min.
func parseAbsence(path string, fallback time.Duration) time.Duration {
	b, err := os.ReadFile(path)
	if err != nil {
		return fallback
	}
	t := strings.TrimSpace(string(b))
	d, err := time.ParseDuration(t)
	if err != nil {
		if secs, err2 := strconv.Atoi(t); err2 == nil {
			d = time.Duration(secs) * time.Second
		} else {
			return fallback
		}
	}
	// 10-60 s. Longer used to be how a film night was kept lit, but the
	// radar's noise refreshes presence every minute or so even with the
	// confirm rule, so a longer timer never expires in an empty room;
	// input activity and `hold` cover long still stretches now.
	if d < 10*time.Second {
		d = 10 * time.Second
	}
	if d > 60*time.Second {
		d = 60 * time.Second
	}
	return d
}

// runAbsence is the `absence` subcommand: `absence 45s` writes the
// override, `absence off` removes it (back to the -absence flag), no
// argument prints the override.
func runAbsence(args []string, path string) {
	if len(args) == 0 {
		if b, err := os.ReadFile(path); err == nil {
			fmt.Printf("absence override: %s\n", strings.TrimSpace(string(b)))
		} else {
			fmt.Println("no override (using -absence)")
		}
		return
	}
	if args[0] == "off" {
		_ = os.Remove(path)
		return
	}
	if d := parseAbsenceText(args[0]); d == 0 {
		fmt.Fprintf(os.Stderr, "absence: want seconds or a duration like 45s, got %q\n", args[0])
		os.Exit(2)
	}
	_ = os.MkdirAll(filepath.Dir(path), 0o755)
	if err := os.WriteFile(path, []byte(args[0]+"\n"), 0o644); err != nil {
		fmt.Fprintln(os.Stderr, "absence:", err)
		os.Exit(1)
	}
}

func parseAbsenceText(t string) time.Duration {
	if d, err := time.ParseDuration(t); err == nil && d > 0 {
		return d
	}
	if secs, err := strconv.Atoi(t); err == nil && secs > 0 {
		return time.Duration(secs) * time.Second
	}
	return 0
}

// flagOff is true when a flag file reads "off"; missing means on.
func flagOff(path string) bool {
	b, err := os.ReadFile(path)
	return err == nil && strings.TrimSpace(string(b)) == "off"
}

// runHold is the `hold` subcommand: `hold 30m` writes the expiry, `hold off`
// removes it, no argument prints what is left.
func runHold(args []string, path string) {
	if len(args) == 0 {
		if h := holdUntil(path, time.Now()); !h.IsZero() {
			fmt.Printf("held until %s (%s left)\n", h.Format(time.Kitchen), time.Until(h).Round(time.Second))
		} else {
			fmt.Println("no hold")
		}
		return
	}
	if args[0] == "off" {
		_ = os.Remove(path)
		return
	}
	d, err := time.ParseDuration(args[0])
	if err != nil || d <= 0 {
		fmt.Fprintf(os.Stderr, "hold: want a duration like 30m or off, got %q\n", args[0])
		os.Exit(2)
	}
	until := time.Now().Add(d)
	if err := os.WriteFile(path, []byte(strconv.FormatInt(until.Unix(), 10)+"\n"), 0o644); err != nil {
		fmt.Fprintln(os.Stderr, "hold:", err)
		os.Exit(1)
	}
	fmt.Printf("held until %s\n", until.Format(time.Kitchen))
}

// statusHeartbeat is the longest status.json goes unwritten. idle-guard.sh
// reads a file older than 15 s as a dead daemon; writing only on change let
// a still, seated evening look like one, and the guard blanked the screen
// under someone deskpresence could see (2026-09-26 04:18 and 04:26).
const statusHeartbeat = 5 * time.Second

// statusDue reports whether the status file should be written: on any
// change, and at least every statusHeartbeat so its age means "alive".
// The main loop ticks at tickFast while something is in motion and at
// tickSlow otherwise: each tick reads a handful of files, and at a fixed
// 4 Hz that kept the laptop's CPU waking ~100 times a second.
const tickFast, tickSlow = 250 * time.Millisecond, time.Second

// tickPeriod is the tick the loop needs now: fast only while the screen
// fade or the audio fade runs (or a cancelled audio fade is being restored,
// 0 < audio < 1), since those step on the tick. Not during the blackout or
// the away state, where both sit at 1, nor while an action runs: nothing
// changes there until an arrival, an input write or the action's finish,
// and each of those ticks at once.
func tickPeriod(fade, audio float64) time.Duration {
	if fade > 0 && fade < 1 || audio > 0 && audio < 1 {
		return tickFast
	}
	return tickSlow
}

func statusDue(s, last string, lastWrite, now time.Time) bool {
	return s != last || now.Sub(lastWrite) >= statusHeartbeat
}

func writeStatus(path, s string) {
	_ = os.MkdirAll(filepath.Dir(path), 0o755)
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, []byte(s+"\n"), 0o644); err == nil {
		_ = os.Rename(tmp, path)
	}
}
