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
	baud                                           int
	absence, debounce, stale, alertAfter           time.Duration
	maxDistance                                    uint
	dryRun, verbose                                bool
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
	flag.UintVar(&c.maxDistance, "max-distance", 250, "ignore targets farther than this many cm (0 = any)")
	flag.StringVar(&c.script, "script", filepath.Join(home, ".config/hypr/scripts/tv-screen.sh"), "actuator")
	flag.StringVar(&c.stateFile, "state", filepath.Join(home, ".config/lgtv/state"), "TV state file written by the actuator")
	flag.StringVar(&c.statusFile, "status", filepath.Join(home, ".local/state/deskpresence/status.json"), "status output for bars/chips")
	flag.StringVar(&c.pauseFile, "pause-file", filepath.Join(runtime, "deskpresence.pause"), "while this exists, observe but never act")
	flag.BoolVar(&c.dryRun, "dry-run", false, "log actions instead of running the actuator")
	flag.BoolVar(&c.verbose, "verbose", false, "log every frame")
	flag.DurationVar(&c.alertAfter, "alert-after", 2*time.Minute, "sensor silent this long -> spoken/desktop alert (OLED is unguarded)")
	flag.StringVar(&c.replay, "replay", "", "synthesise frames instead of reading the port, e.g. present:5s,absent:70s,present:3s")
	flag.Parse()
	log.SetFlags(0)

	pol := &Policy{
		Absence: c.absence, Debounce: c.debounce, MaxDistance: uint16(c.maxDistance),
		StaleAfter: c.stale, MinBackoff: 30 * time.Second, MaxBackoff: 10 * time.Minute,
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	frames := make(chan Frame, 64)
	if c.replay != "" {
		go replayFrames(ctx, c.replay, frames)
	} else {
		go readSerial(ctx, c, frames)
	}

	sleep := watchSleep(ctx) // nil channel when logind is unavailable

	act := &actuator{script: c.script, stateFile: c.stateFile, dryRun: c.dryRun}
	done := make(chan string, 1)
	var (
		lastStatus  string
		lastFrame   Frame
		pausedNoted bool
		staleNoted  bool
		staleSince  time.Time
		lastAlert   time.Time
		everSeen    bool // never alert about a sensor that was never plugged in
	)
	tick := time.NewTicker(250 * time.Millisecond)
	defer tick.Stop()
	log.Printf("deskpresence up: absence=%s debounce=%s max=%dcm tv=%q dry-run=%v", c.absence, c.debounce, c.maxDistance, act.tvState(), c.dryRun)

	for {
		select {
		case <-ctx.Done():
			return
		case f := <-frames:
			lastFrame = f
			pol.Observe(f, time.Now())
			if c.verbose {
				log.Printf("frame: %s", f)
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
			if s := status(pol, act, lastFrame, now, pausedNoted); s != lastStatus {
				writeStatus(c.statusFile, s)
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

func readSerial(ctx context.Context, c config, out chan<- Frame) {
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
		log.Printf("serial: reading %s at %d", dev, c.baud)
		lastErr = ""
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
		"present":   pr,
		"known":     known,
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
