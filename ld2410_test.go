package main

import "testing"

func TestParserBasic(t *testing.T) {
	var p Parser
	raw := append([]byte{0x00, 0xF4, 0xF3}, basicFrame(3, 120, 130, 55, 80)...)
	raw = append(raw, basicFrame(0, 0, 0, 0, 0)...)
	// split at an awkward point to prove resync across reads
	fs := p.Feed(raw[:9])
	fs = append(fs, p.Feed(raw[9:])...)
	if len(fs) != 2 {
		t.Fatalf("got %d frames, want 2: %+v", len(fs), fs)
	}
	if !fs[0].Present() || fs[0].Distance() != 120 || fs[0].StaticEn != 80 {
		t.Errorf("frame0 = %s", fs[0])
	}
	if fs[1].Present() || fs[1].Distance() != 0 {
		t.Errorf("frame1 = %s", fs[1])
	}
}

func TestParserSkipsAck(t *testing.T) {
	var p Parser
	ack := []byte{0xFD, 0xFC, 0xFB, 0xFA, 0x04, 0x00, 0xFF, 0x01, 0x00, 0x00, 0x04, 0x03, 0x02, 0x01}
	fs := p.Feed(append(ack, basicFrame(2, 0, 90, 0, 40)...))
	if len(fs) != 1 || fs[0].State != 2 || fs[0].Distance() != 90 {
		t.Fatalf("got %+v", fs)
	}
}

// Need drives the tty's VMIN: a whole frame when aligned, the rest of the
// frame when a read ended mid-frame, never 0.
func TestParserNeed(t *testing.T) {
	var p Parser
	f := basicFrame(3, 120, 130, 55, 80)
	if len(f) != basicFrameLen {
		t.Fatalf("basic frame is %d bytes, basicFrameLen says %d", len(f), basicFrameLen)
	}
	if p.Need() != basicFrameLen {
		t.Fatalf("before any frame Need = %d, want %d", p.Need(), basicFrameLen)
	}
	p.Feed(f)
	if p.Need() != len(f) {
		t.Fatalf("aligned Need = %d, want %d", p.Need(), len(f))
	}
	p.Feed(f[:9])
	if p.Need() != len(f)-9 {
		t.Fatalf("mid-frame Need = %d, want %d", p.Need(), len(f)-9)
	}
	p.Feed(f[9:])
	if p.Need() != len(f) {
		t.Fatalf("realigned Need = %d, want %d", p.Need(), len(f))
	}
}

// The log line carries the per-gate moving energies when the frame has
// them, since the summary distance is not what decides.
func TestFrameStringGates(t *testing.T) {
	eng := Frame{Engineering: true, State: 1, MovingCM: 156, MovingEn: 100, MovingGates: []byte{62, 48, 12, 5, 0, 0, 0, 0, 0}}
	if got, want := eng.String(), "moving mv=156cm/100 st=0cm/0 det=0cm gates=[62 48 12 5 0 0 0 0 0]"; got != want {
		t.Fatalf("engineering frame:\n got %s\nwant %s", got, want)
	}
	if got := (Frame{State: 2, StaticCM: 90}).String(); got != "static mv=0cm/0 st=90cm/0 det=0cm" {
		t.Fatalf("basic frame: got %s", got)
	}
}
