package main

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// The Lenovo Slim 7 (14IRP8) carries an ST VL53L1 time-of-flight sensor on the
// Intel sensor hub. hid_sensor_custom exposes it as HID-SENSOR-2000e1.N.auto:
// a sysfs enable switch plus a misc device streaming raw input reports. The
// "HOD" (human object detection) instance does the presence verdict itself,
// with a few seconds of hysteresis, and reports only when something changes.
//
// Access needs a udev rule (the input group on the laptop); see
// 70-lenovo-tof-sensor.rules in the laptop's /etc/udev/rules.d.

const (
	tofSysfs = "/sys/bus/platform/devices"
	tofModel = "VL53L1_HOD"
	// Every sample is a packed header (usage u32, timestamp u64, length u32)
	// followed by one raw input report.
	tofHeader = 16
	// Report layout, from the sensor's input-* fields in sysfs: state u8,
	// event u8, an 8-byte stamp, four u32 custom timing fields, then the
	// three that matter here.
	tofDistanceAt = 26 // u32, mm; 1500 means nothing in range
	tofPresentAt  = 31 // u8, 0 or 1
	tofReportMin  = 32
	// The sensor is silent while nothing changes, and the policy calls a
	// silent sensor stale after -stale. Re-send the last reading at this
	// rate while the device is open and healthy.
	tofHeartbeat = time.Second
)

// ToFSample is one decoded report.
type ToFSample struct {
	DistanceMM uint32
	Present    bool
}

// Frame maps a ToF reading onto the LD2410 shape the policy consumes: a
// stationary target at the measured distance, or no target.
func (s ToFSample) Frame() Frame {
	if !s.Present {
		return Frame{}
	}
	return Frame{State: 2, StaticCM: uint16(min(s.DistanceMM/10, 65535)), StaticEn: 100}
}

// parseToF splits a byte stream into samples, returning what it decoded and
// the unconsumed tail (a sample split across reads).
func parseToF(buf []byte) ([]ToFSample, []byte) {
	var out []ToFSample
	for len(buf) >= tofHeader {
		n := int(binary.LittleEndian.Uint32(buf[12:16]))
		if len(buf) < tofHeader+n {
			break
		}
		raw := buf[tofHeader : tofHeader+n]
		buf = buf[tofHeader+n:]
		if n < tofReportMin {
			continue // not the report layout this reads; skip it
		}
		out = append(out, ToFSample{
			DistanceMM: binary.LittleEndian.Uint32(raw[tofDistanceAt:]),
			Present:    raw[tofPresentAt] != 0,
		})
	}
	return out, buf
}

// findToF returns the sysfs name of the presence instance, matched by its
// model string: the .N suffix is enumeration order, not a fixed address.
func findToF() (string, error) {
	devs, _ := filepath.Glob(filepath.Join(tofSysfs, "HID-SENSOR-2000e1.*"))
	for _, d := range devs {
		vals, _ := filepath.Glob(filepath.Join(d, "feature-*-200306", "feature-*-200306-value"))
		for _, v := range vals {
			b, err := os.ReadFile(v)
			if err == nil && decodeByteList(string(b)) == tofModel {
				return filepath.Base(d), nil
			}
		}
	}
	return "", fmt.Errorf("no %s sensor under %s", tofModel, tofSysfs)
}

// decodeByteList turns sysfs's "86 76 53 ... 0 0" into the string it spells.
func decodeByteList(s string) string {
	var b []byte
	for _, f := range strings.Fields(s) {
		n, err := strconv.Atoi(f)
		if err != nil || n == 0 {
			continue
		}
		b = append(b, byte(n))
	}
	return string(b)
}

func setToF(name string, on bool) error {
	v := "0"
	if on {
		v = "1"
	}
	return os.WriteFile(filepath.Join(tofSysfs, name, "enable_sensor"), []byte(v), 0)
}

// readToF streams frames from the sensor, reconnecting on failure the way
// readSerial does. The sensor is switched on only while it is being read.
func readToF(ctx context.Context, out chan<- Frame) {
	var lastErr string
	for ctx.Err() == nil {
		name, err := findToF()
		if err != nil {
			warnOnce(&lastErr, "tof: "+err.Error())
			sleepCtx(ctx, 5*time.Second)
			continue
		}
		if err := setToF(name, true); err != nil {
			warnOnce(&lastErr, fmt.Sprintf("tof: enable %s: %v", name, err))
			sleepCtx(ctx, 5*time.Second)
			continue
		}
		dev, err := os.Open(filepath.Join("/dev", name))
		if err != nil {
			warnOnce(&lastErr, fmt.Sprintf("tof: open %s: %v", name, err))
			_ = setToF(name, false)
			sleepCtx(ctx, 5*time.Second)
			continue
		}
		lastErr = ""
		log.Printf("tof: reading /dev/%s", name)
		err = streamToF(ctx, dev, out, tofHeartbeat)
		dev.Close()
		_ = setToF(name, false)
		if err != nil && ctx.Err() == nil {
			log.Printf("tof: read: %v (reconnecting)", err)
		}
		sleepCtx(ctx, 2*time.Second)
	}
}

// streamToF forwards every sample as a frame and repeats the last one every
// beat while reads keep succeeding. It returns on a read error or when ctx
// ends; closing r is the caller's job (and is what unblocks the reader).
func streamToF(ctx context.Context, r io.Reader, out chan<- Frame, beat time.Duration) error {
	samples := make(chan []ToFSample)
	failed := make(chan error, 1)
	go func() {
		var buf []byte
		chunk := make([]byte, 4096)
		for {
			n, err := r.Read(chunk)
			if n > 0 {
				var got []ToFSample
				got, buf = parseToF(append(buf, chunk[:n]...))
				if len(got) > 0 {
					select {
					case samples <- got:
					case <-ctx.Done():
						return
					}
				}
			}
			if err != nil {
				if errors.Is(err, io.EOF) {
					err = io.ErrUnexpectedEOF // a live device never ends
				}
				failed <- err
				return
			}
		}
	}()

	send := func(f Frame) {
		select {
		case out <- f:
		default: // consumer is behind; the next beat carries the same news
		}
	}
	var last Frame
	have := false
	tick := time.NewTicker(beat)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case err := <-failed:
			return err
		case got := <-samples:
			for _, s := range got {
				last, have = s.Frame(), true
				send(last)
			}
			tick.Reset(beat)
		case <-tick.C:
			if have {
				send(last)
			}
		}
	}
}
