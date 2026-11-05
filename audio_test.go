package main

import (
	"encoding/json"
	"os"
	"testing"
	"time"
)

// fakePactl is a sink-input table that set() writes into, the way PipeWire's
// stream-restore remembers the last volume written per application: a new
// stream of an application starts at that volume.
type fakePactl struct {
	next     int
	streams  map[int]*fakeStream
	restore  map[string]float64 // application.name -> remembered volume
	standIns int
}

type fakeStream struct {
	app    string
	vol    float64
	corked bool
	proxy  string // the stand-in mark, if this is one
}

func newFake() *fakePactl {
	return &fakePactl{next: 1, streams: map[int]*fakeStream{}, restore: map[string]float64{}}
}

func (f *fakePactl) open(app string) int {
	v, ok := f.restore[app]
	if !ok {
		v = 1
	}
	f.next++
	f.streams[f.next] = &fakeStream{app: app, vol: v}
	return f.next
}

func (f *fakePactl) close(i int) { delete(f.streams, i) }

func (f *fakePactl) list() ([]byte, error) {
	var out []map[string]any
	for i, s := range f.streams {
		out = append(out, map[string]any{
			"index":      i,
			"corked":     s.corked,
			"volume":     map[string]any{"mono": map[string]any{"value": s.vol * 65536}},
			"properties": props(s),
		})
	}
	return json.Marshal(out)
}

func props(s *fakeStream) map[string]string {
	p := map[string]string{"application.name": s.app}
	if s.proxy != "" {
		p[proxyMark] = s.proxy
	}
	return p
}

type fakeStandIn struct {
	f *fakePactl
	i int
}

func (s fakeStandIn) stop() { s.f.close(s.i) }

// standIn opens a stand-in the way WirePlumber would: at the saved volume
// for its application, and writes to it land in that saved volume.
func (f *fakePactl) standIn(p map[string]string, _ int) (standIn, error) {
	i := f.open(p["application.name"])
	f.streams[i].proxy = p[proxyMark]
	f.standIns++
	return fakeStandIn{f, i}, nil
}

func (f *fakePactl) set(i, pct int) {
	if s, ok := f.streams[i]; ok {
		s.vol = float64(pct) / 100
		f.restore[s.app] = s.vol
	}
}

func testAudio(f *fakePactl) (*audio, *time.Time) {
	a := newAudio("emotune", "")
	clock := time.Date(2026, 11, 4, 9, 42, 0, 0, time.UTC)
	a.list, a.set = f.list, f.set
	a.sleep = func(time.Duration) {}
	a.now = func() time.Time { return clock }
	a.subscribe = nil
	a.startStandIn = nil // never a real pw-play from a test; repairing() fakes one
	return a, &clock
}

func near(t *testing.T, what string, got, want float64) {
	t.Helper()
	if got < want-0.011 || got > want+0.011 {
		t.Errorf("%s = %.2f, want %.2f", what, got, want)
	}
}

// The 2026-11-04 bug: deskvoice speaks one short stream per utterance. It
// ended partway down the fade, the ramp found nothing of it to restore, and
// every later utterance came back at the faded volume.
func TestStreamGoneMidFadeIsPutBackWhenItReturns(t *testing.T) {
	f := newFake()
	a, _ := testAudio(f)
	ff := f.open("firefox")
	dv := f.open("deskvoice")

	a.apply(0.25)
	a.apply(0.75) // deskvoice now at 25%
	f.close(dv)   // the sentence ends mid-fade
	a.apply(1)    // away: silent
	a.rampUp(time.Second)

	near(t, "firefox after the ramp", f.streams[ff].vol, 1)
	near(t, "deskvoice as stream-restore remembers it", f.restore["deskvoice"], 0.25)
	if _, ok := a.pending["deskvoice"]; !ok {
		t.Fatal("deskvoice should be pending")
	}
	if _, ok := a.pending["firefox"]; ok {
		t.Error("firefox was restored live, it must not be pending")
	}

	// hours later the next utterance opens a stream at the faded volume
	dv = f.open("deskvoice")
	near(t, "new deskvoice stream before the fix", f.streams[dv].vol, 0.25)
	a.fixPending()
	near(t, "new deskvoice stream after the fix", f.streams[dv].vol, 1)
	near(t, "deskvoice in stream-restore", f.restore["deskvoice"], 1)
	if len(a.pending) != 0 {
		t.Errorf("pending should be empty, got %v", a.pending)
	}
}

func TestPendingLeavesAVolumeSomeoneElseSet(t *testing.T) {
	f := newFake()
	a, _ := testAudio(f)
	dv := f.open("deskvoice")
	a.apply(0.5)
	f.close(dv)
	a.apply(0) // a cancelled fade: restore at once

	if _, ok := a.pending["deskvoice"]; !ok {
		t.Fatal("deskvoice should be pending after a cancelled fade too")
	}
	f.restore["deskvoice"] = 0.8 // the user turned it to 80% in a mixer
	dv = f.open("deskvoice")
	a.fixPending()
	near(t, "deskvoice", f.streams[dv].vol, 0.8)
}

func TestFadeSnapshotUsesPendingOriginal(t *testing.T) {
	f := newFake()
	a, _ := testAudio(f)
	dv := f.open("deskvoice")
	a.apply(0.6)
	f.close(dv)
	a.apply(1)
	a.rampUp(time.Second)

	// a new fade starts while deskvoice is speaking at the stale level,
	// before any sink-input event got to fix it
	f.open("deskvoice")
	a.apply(0.5)
	near(t, "deskvoice's remembered original", a.orig["deskvoice"], 1)
}

func TestPendingExpires(t *testing.T) {
	f := newFake()
	a, clock := testAudio(f)
	dv := f.open("deskvoice")
	a.apply(0.5)
	f.close(dv)
	a.apply(0)

	*clock = clock.Add(25 * time.Hour)
	dv = f.open("deskvoice")
	a.fixPending()
	near(t, "deskvoice after expiry (left alone)", f.streams[dv].vol, 0.5)
	if len(a.pending) != 0 {
		t.Error("pending should have expired")
	}
}

func TestStreamNeverWrittenIsNotPending(t *testing.T) {
	f := newFake()
	a, _ := testAudio(f)
	dv := f.open("deskvoice")
	a.snapshot() // seen, then gone before the first write
	f.close(dv)
	a.apply(0.5)
	a.apply(0)
	if len(a.pending) != 0 {
		t.Errorf("nothing was written for deskvoice; pending = %v", a.pending)
	}
}

func repairing(f *fakePactl) (*audio, *time.Time) {
	a, clock := testAudio(f)
	a.startStandIn = f.standIn
	return a, clock
}

func TestGoneStreamIsRepairedAtOnce(t *testing.T) {
	f := newFake()
	a, _ := repairing(f)
	dv := f.open("deskvoice")
	a.apply(0.5)
	f.close(dv)
	a.apply(1)
	a.rampUp(time.Second)

	near(t, "deskvoice saved volume, straight after the ramp", f.restore["deskvoice"], 1)
	if len(a.pending) != 0 {
		t.Errorf("repaired: nothing should be pending, got %v", a.pending)
	}
	for _, s := range f.streams {
		if s.proxy != "" {
			t.Error("the stand-in was left running")
		}
	}
}

func TestRepairLeavesAValueSomeoneElseSet(t *testing.T) {
	f := newFake()
	a, _ := repairing(f)
	dv := f.open("deskvoice")
	a.apply(0.5)
	f.close(dv)
	f.restore["deskvoice"] = 1.2 // turned up past the original elsewhere
	a.apply(0)
	near(t, "deskvoice", f.restore["deskvoice"], 1.2)
}

func TestPausedStreamIsRestoredToo(t *testing.T) {
	f := newFake()
	a, _ := repairing(f)
	mpv := f.open("mpv")
	f.streams[mpv].vol = 0.8
	a.apply(0.5)
	f.streams[mpv].corked = true // paused by hand during the fade
	a.apply(1)
	a.rampUp(time.Second)
	near(t, "paused mpv", f.streams[mpv].vol, 0.8)
	if f.standIns != 0 {
		t.Error("a live (paused) stream needs no stand-in")
	}
}

func TestStandInsAreNeverFaded(t *testing.T) {
	f := newFake()
	a, _ := repairing(f)
	ff := f.open("firefox")
	f.streams[ff].proxy = "someone-elses-stand-in"
	a.apply(0.5)
	if len(a.orig) != 0 {
		t.Errorf("a stand-in was snapshotted: %v", a.orig)
	}
}

func TestCrashMidFadeIsUndoneOnStart(t *testing.T) {
	state := t.TempDir() + "/audio-fade.json"
	f := newFake()
	a, _ := repairing(f)
	a.stateFile = state
	ff := f.open("firefox")
	dv := f.open("deskvoice")
	f.streams[ff].vol = 0.9
	a.apply(0.7) // firefox 27%, deskvoice 30%
	f.close(dv)
	// killed here. The next run:
	b, _ := repairing(f)
	b.stateFile = state
	b.restoreSaved()
	near(t, "firefox", f.streams[ff].vol, 0.9)
	near(t, "deskvoice saved volume", f.restore["deskvoice"], 1)
	if _, err := os.Stat(state); err == nil {
		t.Error("the state file should be gone")
	}
}
