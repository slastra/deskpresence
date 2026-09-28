package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// Input is how recently someone touched the keyboard or mouse, as hypridle
// reports it (listeners at 20 s, 10 min and 1 h, idle inhibitors ignored:
// a video or a stuck portal inhibit must not keep the OLED lit). It is the
// conventional layer under the radar; see Policy.Want.
type Input string

const (
	InputUnknown Input = ""       // hypridle not running, or it has said nothing this boot
	InputActive  Input = "active" // input within the last 20 s
	InputQuiet   Input = "quiet"  // 20 s to 10 min without input
	InputIdle    Input = "idle"   // 10-60 min without input
	InputAway    Input = "away"   // an hour or more: the panel goes off whatever the radar says
)

func parseInput(s string) (Input, bool) {
	switch in := Input(s); in {
	case InputActive, InputQuiet, InputIdle, InputAway:
		return in, true
	}
	return InputUnknown, false
}

// readInput reads the input file hypridle writes through `deskpresence
// input <level>`: "<level> <unix seconds>". A missing or malformed file is
// unknown, and so is any level while hypridle is not running: a stale
// "active" left by a crashed hypridle must never keep the panel lit.
func readInput(path string, alive bool) Input {
	if !alive {
		return InputUnknown
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return InputUnknown
	}
	f := strings.Fields(string(b))
	if len(f) == 0 {
		return InputUnknown
	}
	in, _ := parseInput(f[0])
	return in
}

// processAlive reports whether a process with this comm is running, by
// /proc. Cached for 30 s: it runs on the 250 ms tick.
type processAlive struct {
	comm    string
	checked time.Time
	alive   bool
}

func (p *processAlive) get(now time.Time) bool {
	if now.Sub(p.checked) < 30*time.Second && !p.checked.IsZero() {
		return p.alive
	}
	p.checked, p.alive = now, false
	dirs, _ := filepath.Glob("/proc/[0-9]*/comm")
	for _, d := range dirs {
		if b, err := os.ReadFile(d); err == nil && strings.TrimSpace(string(b)) == p.comm {
			p.alive = true
			break
		}
	}
	return p.alive
}

// runInput is `deskpresence input <level>`, what hypridle's listeners run.
// The write is atomic so the daemon never reads half a file.
func runInput(args []string, path string) {
	if len(args) != 1 {
		fmt.Fprintln(os.Stderr, "usage: deskpresence input active|quiet|idle|away")
		os.Exit(2)
	}
	in, ok := parseInput(args[0])
	if !ok {
		fmt.Fprintf(os.Stderr, "deskpresence input: %q is not active, quiet, idle or away\n", args[0])
		os.Exit(2)
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, []byte(string(in)+" "+strconv.FormatInt(time.Now().Unix(), 10)+"\n"), 0o644); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	if err := os.Rename(tmp, path); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
