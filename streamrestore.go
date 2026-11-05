package main

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"math"
	"os/exec"
	"strconv"
	"strings"
	"time"
)

// Putting an application's remembered volume right when it has no stream.
//
// WirePlumber's stream-restore saves every volume change on a stream under
// the application's identity and hands that volume to the application's
// next stream. The fade writes volumes, so a stream that ENDS mid-fade
// leaves the faded volume saved (deskvoice's one-pw-play-per-sentence
// stream stayed at 24% all of 2026-11-04). There is no API to write a
// stream-restore entry, but a stream with the same identity reaches it:
// repair opens a silent stand-in, which WirePlumber files under the same
// key, sets its volume, and closes it. The stand-in's initial volume is the
// one WirePlumber holds, so it also tells whether that value is ours to fix
// or one somebody set on purpose.
//
// WirePlumber 0.5 keys a stream by media.role when that is "Notification",
// else by the first of application.id, application.name, media.name,
// node.name (scripts/node/state-stream.lua, formKey). pw-play always sets
// an application.name of its own, so only identities at or above that
// level can be reproduced.
//
// Stand-ins carry proxyMark so neither the fade nor deskvoice's ducker takes
// one for the application. deskvoice implements the same contract in
// deskvoice/stream_restore.py.

const proxyMark = "desk.volume-proxy"

type ident struct {
	Key      string `json:"key"`
	Value    string `json:"value"`
	Channels int    `json:"channels"`
}

// identityOf is what WirePlumber keys the stream's saved volume by; ok is
// false when a stand-in can't reproduce it.
func identityOf(s sinkInput) (ident, bool) {
	n := len(s.Volume)
	if n == 0 {
		n = 2
	}
	if s.Properties["media.role"] == "Notification" {
		return ident{"media.role", "Notification", n}, true
	}
	for _, k := range []string{"application.id", "application.name"} {
		if v := s.Properties[k]; v != "" {
			return ident{k, v, n}, true
		}
	}
	return ident{}, false
}

func isProxy(s sinkInput) bool { _, ok := s.Properties[proxyMark]; return ok }

type repairResult int

const (
	repairFailed repairResult = iota
	repairFixed               // saved volume set back to the original
	repairOK                  // it already was
	repairKept                // somebody else's value: left alone
)

func (r repairResult) String() string {
	return [...]string{"failed", "fixed", "ok", "kept"}[r]
}

// standIn is a running stand-in stream; tests replace startStandIn.
type standIn interface{ stop() }

type cmdStandIn struct{ cmd *exec.Cmd }

func (c cmdStandIn) stop() { _ = c.cmd.Process.Kill(); _ = c.cmd.Wait() }

func spaJSON(props map[string]string) string {
	var b strings.Builder
	b.WriteString("{ ")
	for k, v := range props {
		kj, _ := json.Marshal(k)
		vj, _ := json.Marshal(v)
		b.Write(kj)
		b.WriteString(" = ")
		b.Write(vj)
		b.WriteString(" ")
	}
	b.WriteString("}")
	return b.String()
}

func pwPlayStandIn(props map[string]string, channels int) (standIn, error) {
	cmd := exec.Command("pw-play", "-P", spaJSON(props), "--raw", "--format", "f32",
		"--rate", "48000", "--channels", strconv.Itoa(channels), "/dev/zero")
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	return cmdStandIn{cmd}, nil
}

func token() string {
	b := make([]byte, 12)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// repair sets the saved volume of `id` back to `original` if what
// WirePlumber holds is one the fade wrote: at least `lowest` and below the
// original. Called with a.mu held.
func (a *audio) repair(id ident, original, lowest float64) repairResult {
	tok := token()
	props := map[string]string{id.Key: id.Value, proxyMark: tok}
	if _, ok := props["application.name"]; !ok {
		props["application.name"] = "volume repair" // ranks below application.id and the role
	}
	si, err := a.startStandIn(props, id.Channels)
	if err != nil {
		return repairFailed
	}
	defer si.stop()

	find := func() (sinkInput, bool) {
		out, err := a.list()
		if err != nil {
			return sinkInput{}, false
		}
		var all []sinkInput
		if json.Unmarshal(out, &all) != nil {
			return sinkInput{}, false
		}
		for _, s := range all {
			if s.Properties[proxyMark] == tok {
				return s, true
			}
		}
		return sinkInput{}, false
	}
	var s sinkInput
	found := false
	// bounded by tries as well as time, so a stalled clock can't spin it
	deadline := a.now().Add(1500 * time.Millisecond)
	for try := 0; try < 50 && a.now().Before(deadline); try++ {
		if s, found = find(); found {
			break
		}
		a.sleep(30 * time.Millisecond)
	}
	if !found {
		return repairFailed
	}
	// stream-restore applies the saved volume just AFTER the node appears (a
	// first read can show 0): settle, read again, and read once more before
	// calling a value somebody else's
	heldNow := func() float64 {
		a.sleep(150 * time.Millisecond)
		if again, ok := find(); ok {
			s = again
		}
		return volumeOf(s)
	}
	held := heldNow()
	if held < lowest-sameVolume || held >= original+sameVolume {
		held = heldNow()
	}
	switch {
	case math.Abs(held-original) <= sameVolume:
		return repairOK
	case held < lowest-sameVolume || held >= original-sameVolume:
		return repairKept
	}
	a.set(s.Index, int(original*100+0.5))
	a.sleep(150 * time.Millisecond) // WirePlumber stores it on the params-changed event
	return repairFixed
}
