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
