package main

import (
	"encoding/binary"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"go.bug.st/serial"
)

// LD2410 configuration over the same UART. The daemon must be stopped first;
// it holds the port. Settings persist in the module's flash.
//
//	deskpresence config show
//	deskpresence config gates <maxMovingGate> <maxStaticGate> <unmannedSeconds>
//	deskpresence config sens <gate|all> <movingSens> <staticSens>
//	deskpresence config factory
//
// Gates are 75 cm each: 0 = 0-75, 1 = 75-150, 2 = 150-225, 3 = 225-300 ...
// A sensitivity of 100 disables detection in that gate.

var (
	cmdHeader = []byte{0xFD, 0xFC, 0xFB, 0xFA}
	cmdFooter = []byte{0x04, 0x03, 0x02, 0x01}
)

func runConfig(args []string, portGlob string, baud int) {
	if len(args) == 0 {
		fatalf("usage: deskpresence config show|gates|sens|factory")
	}
	dev := resolvePort(portGlob)
	if dev == "" {
		fatalf("no device matches %s", portGlob)
	}
	port, err := serial.Open(dev, &serial.Mode{BaudRate: baud})
	if err != nil {
		fatalf("open %s: %v (is the daemon stopped?)", dev, err)
	}
	defer port.Close()
	_ = port.SetReadTimeout(300 * time.Millisecond)
	s := &session{port: port}

	s.must(0x00FF, []byte{0x01, 0x00}) // enable configuration
	defer s.must(0x00FE, nil)          // end configuration

	switch args[0] {
	case "show":
		prm, err := parseParams(s.must(0x0061, nil))
		if err != nil {
			fatalf("%v", err)
		}
		fmt.Printf("gates: max %d  moving up to gate %d (%d cm)  static up to gate %d (%d cm)\n",
			prm.MaxGate, prm.MaxMoving, (prm.MaxMoving+1)*75, prm.MaxStatic, (prm.MaxStatic+1)*75)
		fmt.Printf("unmanned duration: %d s\n", prm.UnmannedSeconds)
		fmt.Printf("%-6s %-10s %-8s %-8s\n", "gate", "range", "moving", "static")
		for g := 0; g <= prm.MaxGate; g++ {
			fmt.Printf("%-6d %3d-%-6d %-8d %-8d\n", g, g*75, (g+1)*75, prm.MovingSens[g], prm.StaticSens[g])
		}
	case "gates":
		if len(args) != 4 {
			fatalf("usage: config gates <maxMovingGate> <maxStaticGate> <unmannedSeconds>")
		}
		mv, st, dur := atoi(args[1]), atoi(args[2]), atoi(args[3])
		s.must(0x0060, params(0, mv, 1, st, 2, dur))
		fmt.Printf("set: moving gate %d, static gate %d, unmanned %ds\n", mv, st, dur)
	case "sens":
		if len(args) != 4 {
			fatalf("usage: config sens <gate|all> <movingSens> <staticSens>")
		}
		gate := 0xFFFF
		if args[1] != "all" {
			gate = atoi(args[1])
		}
		mv, st := atoi(args[2]), atoi(args[3])
		s.must(0x0064, params(0, gate, 1, mv, 2, st))
		fmt.Printf("set: gate %s moving %d static %d\n", args[1], mv, st)
	case "factory":
		s.must(0x00A2, nil)
		fmt.Println("factory defaults restored (take effect after the module restarts)")
		s.must(0x00A3, nil)
	default:
		fatalf("unknown config action %q", args[0])
	}
}

type session struct{ port serial.Port }

// Params is the module's detection configuration (command 0x61).
type Params struct {
	MaxGate, MaxMoving, MaxStatic int
	MovingSens, StaticSens        []int // per-gate thresholds 0-100
	UnmannedSeconds               int
}

func parseParams(p []byte) (Params, error) {
	if len(p) < 4 || p[0] != 0xAA {
		return Params{}, fmt.Errorf("unexpected parameter payload % X", p)
	}
	n := int(p[1]) + 1
	if len(p) < 4+2*n+2 {
		return Params{}, fmt.Errorf("short parameter payload % X", p)
	}
	return Params{
		MaxGate: int(p[1]), MaxMoving: int(p[2]), MaxStatic: int(p[3]),
		MovingSens:      ints(p[4 : 4+n]),
		StaticSens:      ints(p[4+n : 4+2*n]),
		UnmannedSeconds: int(binary.LittleEndian.Uint16(p[4+2*n:])),
	}, nil
}

// enterEngineering switches a freshly opened port to engineering mode (per-gate
// energies in every frame) and returns the module parameters. Engineering mode
// does not persist across module power cycles, so the daemon does this on
// every (re)connect.
func enterEngineering(port serial.Port) (Params, error) {
	s := &session{port: port}
	if _, err := s.send(0x00FF, []byte{0x01, 0x00}); err != nil {
		return Params{}, err
	}
	defer s.send(0x00FE, nil)
	raw, err := s.send(0x0061, nil)
	if err != nil {
		return Params{}, err
	}
	prm, err := parseParams(raw)
	if err != nil {
		return Params{}, err
	}
	_, err = s.send(0x0062, nil)
	return prm, err
}

func (s *session) must(cmd uint16, value []byte) []byte {
	out, err := s.send(cmd, value)
	if err != nil {
		fatalf("%v", err)
	}
	return out
}

// send issues a command and waits for its ack, returning the ack payload after
// the status word.
func (s *session) send(cmd uint16, value []byte) ([]byte, error) {
	body := make([]byte, 2, 2+len(value))
	binary.LittleEndian.PutUint16(body, cmd)
	body = append(body, value...)
	frame := append([]byte{}, cmdHeader...)
	frame = append(frame, byte(len(body)), byte(len(body)>>8))
	frame = append(frame, body...)
	frame = append(frame, cmdFooter...)
	if _, err := s.port.Write(frame); err != nil {
		return nil, fmt.Errorf("write: %w", err)
	}
	deadline := time.Now().Add(2 * time.Second)
	var buf []byte
	tmp := make([]byte, 256)
	for time.Now().Before(deadline) {
		n, err := s.port.Read(tmp)
		if err != nil {
			return nil, fmt.Errorf("read: %w", err)
		}
		buf = append(buf, tmp[:n]...)
		for {
			i := indexOf(buf, cmdHeader)
			if i < 0 || len(buf) < i+6 {
				break
			}
			ln := int(binary.LittleEndian.Uint16(buf[i+4:]))
			if len(buf) < i+6+ln+4 {
				break
			}
			body := buf[i+6 : i+6+ln]
			buf = buf[i+6+ln+4:]
			if len(body) >= 4 && binary.LittleEndian.Uint16(body) == cmd|0x0100 {
				if st := binary.LittleEndian.Uint16(body[2:]); st != 0 {
					return nil, fmt.Errorf("command %04X failed, status %d", cmd, st)
				}
				return body[4:], nil
			}
		}
	}
	return nil, fmt.Errorf("no ack for command %04X", cmd)
}

// params encodes word/dword pairs as the 0x60 and 0x64 commands expect.
func params(kv ...int) []byte {
	var b []byte
	for i := 0; i+1 < len(kv); i += 2 {
		b = binary.LittleEndian.AppendUint16(b, uint16(kv[i]))
		b = binary.LittleEndian.AppendUint32(b, uint32(kv[i+1]))
	}
	return b
}

func atoi(s string) int {
	n, err := strconv.Atoi(strings.TrimSpace(s))
	if err != nil {
		fatalf("not a number: %q", s)
	}
	return n
}

func fatalf(format string, a ...any) {
	fmt.Fprintf(os.Stderr, format+"\n", a...)
	os.Exit(1)
}
