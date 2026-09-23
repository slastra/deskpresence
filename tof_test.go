package main

import (
	"context"
	"encoding/binary"
	"encoding/hex"
	"io"
	"strings"
	"testing"
	"time"
)

// A report captured from the laptop's sensor on 2026-09-23: someone seated
// at 476 mm, confidence 100, present.
const tofSeated = "02 06 42 ec 81 db 08 00 00 00 e4 f6 1f ba 49 99 3f 00 e2 5b 0d 1a aa 00 00 00 dc 01 00 00 64 01 26 02"

func tofReport(t *testing.T, hexs string) []byte {
	t.Helper()
	b, err := hex.DecodeString(strings.ReplaceAll(hexs, " ", ""))
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// tofSample wraps a raw report in the misc device's sample header.
func tofSample(raw []byte) []byte {
	h := make([]byte, tofHeader)
	binary.LittleEndian.PutUint32(h[0:], 0x2000e1)
	binary.LittleEndian.PutUint64(h[4:], 12345)
	binary.LittleEndian.PutUint32(h[12:], uint32(len(raw)))
	return append(h, raw...)
}

// The same report with nothing in range: 1500 mm, confidence 0, absent.
func tofAway(t *testing.T) []byte {
	raw := tofReport(t, tofSeated)
	binary.LittleEndian.PutUint32(raw[tofDistanceAt:], 1500)
	raw[30], raw[tofPresentAt] = 0, 0
	return raw
}

func TestParseToF(t *testing.T) {
	stream := append(tofSample(tofReport(t, tofSeated)), tofSample(tofAway(t))...)
	got, rest := parseToF(stream)
	if len(rest) != 0 || len(got) != 2 {
		t.Fatalf("got %d samples, %d bytes left", len(got), len(rest))
	}
	if got[0] != (ToFSample{DistanceMM: 476, Present: true}) {
		t.Errorf("seated: %+v", got[0])
	}
	if got[1] != (ToFSample{DistanceMM: 1500, Present: false}) {
		t.Errorf("away: %+v", got[1])
	}
	if f := got[0].Frame(); !f.Present() || f.Distance() != 47 {
		t.Errorf("seated frame: %s", f)
	}
	if f := got[1].Frame(); f.Present() {
		t.Errorf("away frame: %s", f)
	}
}

func TestParseToFSplitRead(t *testing.T) {
	s := tofSample(tofReport(t, tofSeated))
	got, rest := parseToF(s[:20])
	if len(got) != 0 || len(rest) != 20 {
		t.Fatalf("partial: %d samples, %d left", len(got), len(rest))
	}
	got, rest = parseToF(append(rest, s[20:]...))
	if len(got) != 1 || len(rest) != 0 {
		t.Fatalf("joined: %d samples, %d left", len(got), len(rest))
	}
}

func TestDecodeByteList(t *testing.T) {
	if s := decodeByteList("86 76 53 51 76 49 95 72 79 68 0 0 0"); s != tofModel {
		t.Errorf("got %q", s)
	}
}

// A healthy but quiet sensor keeps the policy fed; a failed read stops it.
func TestStreamToFHeartbeat(t *testing.T) {
	pr, pw := io.Pipe()
	out := make(chan Frame, 64)
	done := make(chan error, 1)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { done <- streamToF(ctx, pr, out, 20*time.Millisecond) }()

	go pw.Write(tofSample(tofReport(t, tofSeated)))
	time.Sleep(110 * time.Millisecond) // one sample, then silence
	n := len(out)
	if n < 3 {
		t.Fatalf("only %d frames in 110 ms of silence; heartbeat not repeating", n)
	}
	for i := 0; i < n; i++ {
		if f := <-out; !f.Present() {
			t.Fatalf("frame %d not present: %s", i, f)
		}
	}

	pw.CloseWithError(io.ErrClosedPipe)
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("read failure returned nil")
		}
	case <-time.After(time.Second):
		t.Fatal("stream kept running after the read failed")
	}
	time.Sleep(60 * time.Millisecond)
	if len(out) != 0 {
		t.Errorf("%d frames after the reader died", len(out))
	}
}
