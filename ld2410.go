package main

import (
	"encoding/binary"
	"fmt"
)

// Frame is one LD2410 report (basic or engineering mode; the first 13 payload
// bytes are the same in both).
type Frame struct {
	Engineering bool
	State       byte   // 0 none, 1 moving, 2 stationary, 3 both
	MovingCM    uint16 // moving target distance
	MovingEn    byte   // moving target energy 0-100
	StaticCM    uint16 // stationary target distance
	StaticEn    byte
	DetectCM    uint16 // sensor's own "detection distance"
}

func (f Frame) Present() bool { return f.State != 0 }

// Distance returns the closest reported target distance, or 0 when none.
func (f Frame) Distance() uint16 {
	switch f.State {
	case 1:
		return f.MovingCM
	case 2:
		return f.StaticCM
	case 3:
		if f.MovingCM != 0 && f.MovingCM < f.StaticCM {
			return f.MovingCM
		}
		return f.StaticCM
	}
	return 0
}

func (f Frame) String() string {
	names := [...]string{"none", "moving", "static", "both"}
	n := "?"
	if int(f.State) < len(names) {
		n = names[f.State]
	}
	return fmt.Sprintf("%s mv=%dcm/%d st=%dcm/%d det=%dcm", n, f.MovingCM, f.MovingEn, f.StaticCM, f.StaticEn, f.DetectCM)
}

var (
	dataHeader = []byte{0xF4, 0xF3, 0xF2, 0xF1}
	dataFooter = []byte{0xF8, 0xF7, 0xF6, 0xF5}
)

// Parser is a resynchronising byte-stream parser for LD2410 data frames.
// Feed it whatever the serial port hands you; it yields complete frames and
// silently skips config/ack frames (FD FC FB FA) and garbage.
type Parser struct {
	buf []byte
}

func (p *Parser) Feed(b []byte) []Frame {
	p.buf = append(p.buf, b...)
	var out []Frame
	for {
		i := indexOf(p.buf, dataHeader)
		if i < 0 {
			// keep the last 3 bytes in case a header straddles reads
			if len(p.buf) > 3 {
				p.buf = p.buf[len(p.buf)-3:]
			}
			return out
		}
		p.buf = p.buf[i:]
		if len(p.buf) < 6 {
			return out
		}
		n := int(binary.LittleEndian.Uint16(p.buf[4:6]))
		total := 4 + 2 + n + 4
		if n < 11 || n > 64 { // basic is 13; engineering ~45
			p.buf = p.buf[1:]
			continue
		}
		if len(p.buf) < total {
			return out
		}
		payload := p.buf[6 : 6+n]
		if string(p.buf[6+n:total]) != string(dataFooter) || payload[1] != 0xAA {
			p.buf = p.buf[1:]
			continue
		}
		f := Frame{
			Engineering: payload[0] == 0x01,
			State:       payload[2],
			MovingCM:    binary.LittleEndian.Uint16(payload[3:5]),
			MovingEn:    payload[5],
			StaticCM:    binary.LittleEndian.Uint16(payload[6:8]),
			StaticEn:    payload[8],
			DetectCM:    binary.LittleEndian.Uint16(payload[9:11]),
		}
		out = append(out, f)
		p.buf = p.buf[total:]
	}
}

func indexOf(hay, needle []byte) int {
	for i := 0; i+len(needle) <= len(hay); i++ {
		if string(hay[i:i+len(needle)]) == string(needle) {
			return i
		}
	}
	return -1
}

// basicFrame builds a wire-format basic-mode frame; used by tests and --replay.
func basicFrame(state byte, mv, st uint16, mvEn, stEn byte) []byte {
	payload := []byte{0x02, 0xAA, state, 0, 0, mvEn, 0, 0, stEn, 0, 0, 0x55, 0x00}
	binary.LittleEndian.PutUint16(payload[3:], mv)
	binary.LittleEndian.PutUint16(payload[6:], st)
	binary.LittleEndian.PutUint16(payload[9:], min(mv, st))
	b := append([]byte{}, dataHeader...)
	b = append(b, byte(len(payload)), 0)
	b = append(b, payload...)
	return append(b, dataFooter...)
}
