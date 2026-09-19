// deskpresence: turn the TV off when nobody is at the desk and back on the
// moment someone is, driven by an LD2410C mmWave sensor on a USB UART.
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
	"strings"
	"syscall"
	"time"

	"github.com/godbus/dbus/v5"
	"go.bug.st/serial"
)

type config struct {
	port, script, stateFile, statusFile, pauseFile string
	httpAddr                                       string
	baud                                           int
	absence, debounce, stale, alertAfter, fade     time.Duration
	maxDistance                                    uint
	nearGates, energyMin                           int
	dryRun, verbose, noMpris                       bool
	replay                                         string
}

func main() {
	home, _ := os.UserHomeDir()
	runtime := os.Getenv("XDG_RUNTIME_DIR")
	if runtime == "" {
		runtime = os.TempDir()
	}
	var c config
	flag.StringVar(&c.port, "port", "/dev/serial/by-id/*CP210*", "serial device (glob ok)")
	flag.IntVar(&c.baud, "baud", 256000, "serial baud rate")
	flag.DurationVar(&c.absence, "absence", 60*time.Second, "how long the desk must be empty before the TV goes off")
	flag.DurationVar(&c.debounce, "debounce", 500*time.Millisecond, "how long presence must hold before the TV comes on")
	flag.DurationVar(&c.stale, "stale", 5*time.Second, "no frames for this long -> no opinion, no actions")
	flag.UintVar(&c.maxDistance, "max-distance", 250, "basic frames only: ignore targets farther than this many cm (0 = any)")
	flag.IntVar(&c.nearGates, "near-gates", 3, "engineering frames: gates (75 cm each) that count as the desk; 0 = use max-distance")
	flag.IntVar(&c.energyMin, "energy-min", 40, "engineering frames: moving energy in a near gate that counts as presence")
	flag.StringVar(&c.script, "script", filepath.Join(home, ".config/hypr/scripts/tv-screen.sh"), "actuator")
	flag.StringVar(&c.stateFile, "state", filepath.Join(home, ".config/lgtv/state"), "TV state file written by the actuator")
	flag.StringVar(&c.statusFile, "status", filepath.Join(home, ".local/state/deskpresence/status.json"), "status output for bars/chips")
	flag.StringVar(&c.pauseFile, "pause-file", filepath.Join(runtime, "deskpresence.pause"), "while this exists, observe but never act")
	flag.BoolVar(&c.dryRun, "dry-run", false, "log actions instead of running the actuator")
	flag.BoolVar(&c.verbose, "verbose", false, "log every frame")
	flag.DurationVar(&c.fade, "fade", 5*time.Second, "start dimming the screen this long before absence latches (0 = off)")
	flag.DurationVar(&c.alertAfter, "alert-after", 2*time.Minute, "sensor silent this long -> spoken/desktop alert (OLED is unguarded)")
	flag.StringVar(&c.httpAddr, "http", "127.0.0.1:7391", "serve the live sensor view here (empty = off)")
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
	flag.Parse()
	log.SetFlags(0)

	pol := &Policy{
		Absence: c.absence, Debounce: c.debounce, MaxDistance: uint16(c.maxDistance),
		NearGates: c.nearGates, EnergyMin: c.energyMin, Fade: c.fade,
		StaleAfter: c.stale, MinBackoff: 30 * time.Second, MaxBackoff: 10 * time.Minute,
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	frames := make(chan Frame, 64)
	h := newHub()
	if c.httpAddr != "" {
		go h.serve(c.httpAddr)
	}
	if c.replay != "" {
		go replayFrames(ctx, c.replay, frames)
	} else {
		go readSerial(ctx, c, frames, h)
	}

	sleep := watchSleep(ctx) // nil channel when logind is unavailable

	act := &actuator{script: c.script, stateFile: c.stateFile, dryRun: c.dryRun}
	var media *mpris
	if !c.noMpris {
		media = newMpris()
	}
	var lastPresent, lastKnown bool
	done := make(chan string, 1)
	var (
		lastStatus  string
		lastFrame   Frame
		pausedNoted bool
		staleNoted  bool
		staleSince  time.Time
		lastAlert   time.Time
		everSeen    bool // never alert about a sensor that was never plugged in
		flipWindow  time.Time
		flips       int
	)
	tick := time.NewTicker(250 * time.Millisecond)
	defer tick.Stop()
	hist := newHistory(filepath.Join(filepath.Dir(c.statusFile), "history.json"), 500*time.Millisecond, 120)
	log.Printf("deskpresence up: absence=%s debounce=%s near-gates=%d energy-min=%d max=%dcm tv=%q dry-run=%v", c.absence, c.debounce, c.nearGates, c.energyMin, c.maxDistance, act.tvState(), c.dryRun)

	for {
		select {
		case <-ctx.Done():
			return
		case f := <-frames:
			lastFrame = f
			now := time.Now()
			flipped := pol.Observe(f, now)
			h.publish("frame", toEvent(f, now))
			hist.observe(f, c.nearGates, now)
			if c.verbose {
				log.Printf("frame: %s", f)
			} else if flipped {
				// Raw edges are the calibration evidence: what the sensor saw
				// the instant it decided the desk was empty or occupied.
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
		case a := <-done:
			act.busy = false
			log.Printf("actuator: %s finished, tv=%q", a, act.tvState())
		case s := <-sleep:
			if s.before {
				log.Printf("sleep: powering TV off before suspend")
				if !act.busy {
					act.busy = true
					act.run("off") // synchronous: logind waits on our inhibitor
					act.busy = false
				}
				s.release()
			} else {
				log.Printf("sleep: resumed, tv=%q", act.tvState())
				pol.Reset()
			}
		case now := <-tick.C:
			// Media follows the debounced verdict, not the TV: pause the
			// moment "away" latches, resume the moment "present" does.
			if pr, known := pol.Present(); known {
				if lastKnown && pr != lastPresent {
					if pr {
						media.resume()
					} else {
						media.pauseAll()
					}
				}
				lastPresent, lastKnown = pr, true
			}
			if _, err := os.Stat(c.pauseFile); err == nil {
				if !pausedNoted {
					log.Printf("paused: %s exists, observing only", c.pauseFile)
					pausedNoted = true
				}
			} else {
				if pausedNoted {
					log.Printf("unpaused")
					pausedNoted = false
				}
				if pol.SensorStale(now) {
					if !staleNoted {
						log.Printf("sensor: no frames for %s, holding", c.stale)
						staleNoted = true
						staleSince = now
					}
					// A dead sensor means nothing blanks the OLED. Be loud,
					// once, then hourly.
					if everSeen && now.Sub(staleSince) > c.alertAfter && now.Sub(lastAlert) > time.Hour {
						lastAlert = now
						alert("Presence sensor offline. The OLED is not being blanked.")
					}
				}
				if a := pol.Decide(act.tvState(), now, act.busy); a != "" {
					pr, _ := pol.Present()
					log.Printf("decide: %s (present=%v, last %s)", a, pr, lastFrame)
					act.busy = true
					go func() { act.run(a); done <- a }()
				}
			}
			if pr, known := pol.Present(); known {
				hist.tick(now, pr, c.energyMin)
			}
			if s := status(pol, act, lastFrame, now, pausedNoted); s != lastStatus {
				writeStatus(c.statusFile, s)
				h.publish("status", json.RawMessage(s))
				lastStatus = s
			}
		}
	}
}

// --- actuator -------------------------------------------------------------

type actuator struct {
	script, stateFile string
	dryRun            bool
	busy              bool
}

func (a *actuator) tvState() string {
	b, err := os.ReadFile(a.stateFile)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(b))
}

func (a *actuator) run(action string) {
	if a.dryRun {
		log.Printf("dry-run: would run %s %s", a.script, action)
		_ = os.WriteFile(a.stateFile, []byte(action+"\n"), 0o644)
		return
	}
	t0 := time.Now()
	cmd := exec.Command(a.script, action)
	cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
	if err := cmd.Run(); err != nil {
		log.Printf("actuator: %s %s failed after %s: %v", filepath.Base(a.script), action, time.Since(t0).Round(time.Second), err)
		return
	}
	log.Printf("actuator: %s ok in %s", action, time.Since(t0).Round(time.Second))
}

// --- serial ---------------------------------------------------------------

func readSerial(ctx context.Context, c config, out chan<- Frame, h *hub) {
	var lastErr string
	for ctx.Err() == nil {
		dev := resolvePort(c.port)
		if dev == "" {
			warnOnce(&lastErr, "serial: no device matches "+c.port)
			sleepCtx(ctx, 5*time.Second)
			continue
		}
		port, err := serial.Open(dev, &serial.Mode{BaudRate: c.baud})
		if err != nil {
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
		_ = port.SetReadTimeout(time.Second)
		var p Parser
		buf := make([]byte, 512)
		for ctx.Err() == nil {
			n, err := port.Read(buf)
			if err != nil {
				log.Printf("serial: read: %v (reconnecting)", err)
				break
			}
			for _, f := range p.Feed(buf[:n]) {
				select {
				case out <- f:
				default: // consumer is behind; drop, the next frame is 100ms away
				}
			}
		}
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

func status(p *Policy, a *actuator, f Frame, now time.Time, paused bool) string {
	pr, known := p.Present()
	s := map[string]any{
		"rule": map[string]any{"near_gates": p.NearGates, "energy_min": p.EnergyMin,
			"absence_ms": p.Absence.Milliseconds(), "max_distance": p.MaxDistance},
		"present":   pr,
		"known":     known,
		"since":     p.PresentSince().UnixMilli(),
		"fade":      map[bool]float64{true: 0, false: p.FadeLevel(now)}[paused],
		"sensor_ok": !p.SensorStale(now),
		"tv":        a.tvState(),
		"busy":      a.busy,
		"paused":    paused,
		"state":     f.State,
		"distance":  f.Distance(),
	}
	b, _ := json.Marshal(s)
	return string(b)
}

func writeStatus(path, s string) {
	_ = os.MkdirAll(filepath.Dir(path), 0o755)
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, []byte(s+"\n"), 0o644); err == nil {
		_ = os.Rename(tmp, path)
	}
}
