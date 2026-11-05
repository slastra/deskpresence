package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"log"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// audio fades the playing PipeWire streams with the screen: down to silence
// over the absence warning, held at zero while away (the players are paused
// underneath), and back up to their original levels when someone returns.
// Streams are remembered by application name, not sink-input index:
// Chrome (and others) tear the stream down on pause and create a fresh one
// on play, and PipeWire's stream-restore then hands the new stream the last
// volume written for that application, which after a fade is zero. So the
// original level is keyed by application and every write re-lists the live
// streams and applies to all of that application's current ones.
// Volume is set through pactl rather than MPRIS because Firefox's MPRIS
// ignores its Volume property.
//
// An application whose stream ENDED mid-fade has nothing live to restore
// when the fade comes back up, and stream-restore keeps the faded volume
// for it. deskvoice opens one short pw-play stream per utterance, so its
// next sentence, minutes or hours later, played at the faded level for good
// (2026-11-04: faded at 09:42, inaudible until reset that evening). So the
// fade remembers each application's stream-restore identity, and at the end
// writes a gone application's saved volume back through a silent stand-in
// stream (streamrestore.go). Paused streams are restored with the playing
// ones: a player paused during the fade must not resume faded.
//
// If the stand-in fails, the application stays pending: while any is,
// `pactl subscribe` watches for sink-input events, and a stream of a
// pending application showing the faded volume is put back to the
// original. One at any other volume was set by someone else and is left
// alone. Pending entries expire after a day.
//
// While faded, the originals are saved to stateFile, so a crash or a
// restart mid-fade is undone on the next start (restoreSaved).
type audio struct {
	mu      sync.Mutex
	exclude []string           // application.name substrings left alone (UI cues)
	orig    map[string]float64 // application.name -> original volume, while faded
	// last volume written per application during this fade, to know what
	// stream-restore holds for one whose stream is gone
	lastSet map[string]float64
	pending map[string]pend
	idents  map[string]ident // application -> its stream-restore identity, while faded
	watch   *exec.Cmd        // pactl subscribe, while anything is pending
	// the originals while faded, for restoreSaved; "" = none
	stateFile string
	// level is the last applied fade level, as float64 bits. Atomic so the
	// main loop can read it while rampUp holds mu for seconds.
	level atomic.Uint64

	// pactl, swappable for tests
	list         func() ([]byte, error)
	set          func(index, pct int)
	sleep        func(time.Duration)
	now          func() time.Time
	startStandIn func(props map[string]string, channels int) (standIn, error)
	// subscribe starts the event watch; nil in tests (they call fixPending)
	subscribe func(a *audio)
}

// pend is an application stream-restore still holds at a faded volume.
type pend struct {
	orig, faded float64
	until       time.Time
}

const (
	pendingFor = 24 * time.Hour
	sameVolume = 0.02 // volumes this close count as the one we wrote
)

func newAudio(exclude, stateFile string) *audio {
	var ex []string
	for _, e := range strings.Split(exclude, ",") {
		if e = strings.TrimSpace(e); e != "" {
			ex = append(ex, strings.ToLower(e))
		}
	}
	return &audio{
		exclude: ex, orig: map[string]float64{}, lastSet: map[string]float64{},
		pending: map[string]pend{}, idents: map[string]ident{}, stateFile: stateFile,
		list: func() ([]byte, error) {
			return exec.Command("pactl", "-f", "json", "list", "sink-inputs").Output()
		},
		set: func(index, pct int) {
			_ = exec.Command("pactl", "set-sink-input-volume", strconv.Itoa(index), fmt.Sprintf("%d%%", pct)).Run()
		},
		sleep:        time.Sleep,
		now:          time.Now,
		subscribe:    (*audio).subscribePactl,
		startStandIn: pwPlayStandIn,
	}
}

type sinkInput struct {
	Index      int `json:"index"`
	Corked     bool
	Volume     map[string]struct{ Value float64 } `json:"volume"`
	Properties map[string]string                  `json:"properties"`
}

// streams lists the sink-inputs the fade may touch: playing ones, plus
// paused ones when withCorked (restoring must reach a player paused mid-fade).
func (a *audio) streams(withCorked ...bool) []sinkInput {
	corked := len(withCorked) > 0 && withCorked[0]
	out, err := a.list()
	if err != nil {
		return nil
	}
	var all []sinkInput
	if json.Unmarshal(out, &all) != nil {
		return nil
	}
	var keep []sinkInput
	for _, s := range all {
		name := strings.ToLower(s.Properties["application.name"])
		skip := (s.Corked && !corked) || len(s.Volume) == 0 || name == "" || isProxy(s)
		for _, e := range a.exclude {
			if strings.Contains(name, e) {
				skip = true
			}
		}
		if !skip {
			keep = append(keep, s)
		}
	}
	return keep
}

func volumeOf(s sinkInput) float64 {
	for _, ch := range s.Volume {
		return ch.Value / 65536.0
	}
	return 1
}

func appOf(s sinkInput) string { return strings.ToLower(s.Properties["application.name"]) }

// setAll writes frac*orig to every live stream of each remembered
// application, and returns the applications it found a live stream for.
func (a *audio) setAll(frac float64) map[string]bool {
	wrote := map[string]bool{}
	for _, s := range a.streams(true) {
		if v, ok := a.orig[appOf(s)]; ok {
			pct := int(v*frac*100 + 0.5)
			a.set(s.Index, pct)
			a.lastSet[appOf(s)] = float64(pct) / 100
			wrote[appOf(s)] = true
		}
	}
	return wrote
}

// snapshot records the playing streams' volumes as the originals. A stream
// of a pending application still at its faded volume is not an original:
// it would make the faded level permanent. It counts as its true original.
func (a *audio) snapshot() {
	for _, s := range a.streams() {
		app := appOf(s)
		if _, seen := a.orig[app]; seen {
			continue
		}
		v := volumeOf(s)
		if p, ok := a.pending[app]; ok && math.Abs(v-p.faded) <= sameVolume {
			v = p.orig
		}
		a.orig[app] = v
		if id, ok := identityOf(s); ok {
			a.idents[app] = id
		}
	}
	a.save()
}

type savedFade struct {
	Orig   map[string]float64 `json:"orig"`
	Idents map[string]ident   `json:"idents"`
	TS     int64              `json:"ts"`
}

func (a *audio) save() {
	if a.stateFile == "" || len(a.orig) == 0 {
		return
	}
	b, _ := json.Marshal(savedFade{a.orig, a.idents, a.now().Unix()})
	_ = os.MkdirAll(filepath.Dir(a.stateFile), 0o755)
	tmp := a.stateFile + ".tmp"
	if os.WriteFile(tmp, b, 0o644) == nil {
		_ = os.Rename(tmp, a.stateFile)
	}
}

func (a *audio) unsave() {
	if a.stateFile != "" {
		_ = os.Remove(a.stateFile)
	}
}

// restoreSaved undoes a fade a crash or restart left behind: live streams
// (paused too) of the saved applications go back to their originals, the
// gone ones have their saved volume repaired. Called once at startup.
func (a *audio) restoreSaved() {
	if a == nil || a.stateFile == "" {
		return
	}
	b, err := os.ReadFile(a.stateFile)
	if err != nil {
		return
	}
	var saved savedFade
	if json.Unmarshal(b, &saved) != nil || len(saved.Orig) == 0 {
		a.unsave()
		return
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	live := map[string]bool{}
	for _, s := range a.streams(true) {
		app := appOf(s)
		if v, ok := saved.Orig[app]; ok {
			live[app] = true
			if volumeOf(s) < v-sameVolume {
				a.set(s.Index, int(v*100+0.5))
			}
		}
	}
	a.orig, a.idents = saved.Orig, saved.Idents
	if a.idents == nil {
		a.idents = map[string]ident{}
	}
	for app := range a.orig {
		a.lastSet[app] = 0 // unknown how far it got: anything below the original is ours
	}
	log.Printf("audio: found a fade left by the last run; restoring %v", saved.Orig)
	a.forget(live)
}

// forget ends a fade. live: the applications just put back on a live
// stream. The rest that were written during the fade have their saved
// volume repaired through a stand-in, or failing that stay pending.
func (a *audio) forget(live map[string]bool) {
	for app, v := range a.orig {
		faded, wrote := a.lastSet[app]
		if live[app] || !wrote || faded >= v-sameVolume {
			continue
		}
		if id, ok := a.idents[app]; ok && a.startStandIn != nil {
			switch r := a.repair(id, v, 0); r {
			case repairFixed:
				log.Printf("audio: %s had no stream; its saved volume is back to %.0f%%", app, v*100)
				continue
			case repairOK, repairKept:
				if r == repairKept {
					log.Printf("audio: %s had no stream; its saved volume was set by someone else, left alone", app)
				}
				continue
			}
		}
		a.pending[app] = pend{orig: v, faded: faded, until: a.now().Add(pendingFor)}
		log.Printf("audio: %s had no stream to restore; putting it back to %.0f%% when it plays again", app, v*100)
	}
	a.orig = map[string]float64{}
	a.lastSet = map[string]float64{}
	a.idents = map[string]ident{}
	a.unsave()
	if len(a.pending) > 0 && a.watch == nil && a.subscribe != nil {
		a.subscribe(a)
	}
}

// fixPending puts pending applications' new streams back to their original
// volume. Called on every sink-input event while anything is pending.
func (a *audio) fixPending() {
	a.mu.Lock()
	defer a.mu.Unlock()
	if len(a.orig) > 0 {
		return // a fade owns the streams; its snapshot reads pending itself
	}
	now := a.now()
	for app, p := range a.pending {
		if now.After(p.until) {
			delete(a.pending, app)
		}
	}
	if len(a.pending) == 0 {
		return
	}
	for _, s := range a.streams() {
		app := appOf(s)
		p, ok := a.pending[app]
		if !ok || math.Abs(volumeOf(s)-p.faded) > sameVolume {
			continue
		}
		a.set(s.Index, int(p.orig*100+0.5))
		delete(a.pending, app)
		log.Printf("audio: put %s back to %.0f%% (it came back at the faded %.0f%%)", app, p.orig*100, p.faded*100)
	}
}

// pendingCount is for the watcher's exit check.
func (a *audio) pendingCount() int {
	a.mu.Lock()
	defer a.mu.Unlock()
	return len(a.pending)
}

// subscribePactl runs `pactl subscribe` until nothing is pending, calling
// fixPending on each sink-input event ('new' for a fresh stream, 'change'
// for stream-restore applying its volume a moment after creation).
// Restarted after a PipeWire restart. Called with mu held.
func (a *audio) subscribePactl() {
	cmd := exec.Command("pactl", "subscribe")
	out, err := cmd.StdoutPipe()
	if err != nil || cmd.Start() != nil {
		log.Printf("audio: pactl subscribe failed, pending volumes wait for the next fade")
		return
	}
	a.watch = cmd
	go func() {
		sc := bufio.NewScanner(out)
		for sc.Scan() {
			line := sc.Text()
			if strings.Contains(line, "on sink-input") && !strings.Contains(line, "'remove'") {
				a.fixPending()
				if a.pendingCount() == 0 {
					break
				}
			}
		}
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		a.mu.Lock()
		a.watch = nil
		again := len(a.pending) > 0
		a.mu.Unlock()
		if again { // pactl died (PipeWire restarted): watch again shortly
			a.sleep(5 * time.Second)
			a.mu.Lock()
			if a.watch == nil && len(a.pending) > 0 {
				a.subscribePactl()
			}
			a.mu.Unlock()
		}
	}()
}

// apply moves every faded stream to orig*(1-level). The first non-zero
// level snapshots the playing streams; level 0 restores them and forgets.
func (a *audio) apply(level float64) {
	if a == nil {
		return
	}
	if !a.mu.TryLock() { // a ramp is in progress; it owns the streams
		return
	}
	defer a.mu.Unlock()
	if level == a.Level() {
		return
	}
	if level > 0 && len(a.orig) == 0 {
		a.snapshot()
		if len(a.orig) > 0 {
			log.Printf("audio: fading %v", a.orig)
		}
	}
	live := a.setAll(1 - level)
	a.setLevel(level)
	if level == 0 {
		if len(a.orig) > 0 {
			log.Printf("audio: restored")
		}
		a.forget(live)
	}
}

// rampUp brings the faded streams back over `d` in steps, then forgets
// them. Called after the players have been resumed.
func (a *audio) rampUp(d time.Duration) {
	if a == nil {
		return
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if len(a.orig) == 0 {
		a.setLevel(0)
		return
	}
	const steps = 8
	live := map[string]bool{}
	for i := 1; i <= steps; i++ {
		for app := range a.setAll(float64(i) / steps) {
			live[app] = i == steps
		}
		a.sleep(d / steps)
	}
	// A player that recreates its stream on play may do so late; two more
	// full-volume passes catch it before we forget the originals.
	for i := 0; i < 2; i++ {
		a.sleep(time.Second)
		for app := range a.setAll(1) {
			live[app] = true
		}
	}
	log.Printf("audio: ramped %v back up", a.orig)
	a.forget(live)
	a.setLevel(0)
}

// Level is the fade level last applied: 0 full volume, 1 silent. Safe from
// any goroutine; 0 when audio follow is off (a nil *audio).
func (a *audio) Level() float64 {
	if a == nil {
		return 0
	}
	return math.Float64frombits(a.level.Load())
}

func (a *audio) setLevel(v float64) { a.level.Store(math.Float64bits(v)) }
