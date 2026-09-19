package main

import (
	"log"
	"strings"

	"github.com/godbus/dbus/v5"
)

// mpris pauses whatever is playing when the desk empties and resumes exactly
// those players when someone is back, leaving anything that was already
// paused (or that the user touched meanwhile) alone.
type mpris struct {
	conn   *dbus.Conn
	paused []string // bus names we paused, resumed in order
}

func newMpris() *mpris {
	conn, err := dbus.ConnectSessionBus()
	if err != nil {
		log.Printf("mpris: %v (media pause disabled)", err)
		return nil
	}
	return &mpris{conn: conn}
}

const (
	mprisPrefix = "org.mpris.MediaPlayer2."
	mprisPath   = "/org/mpris/MediaPlayer2"
	mprisPlayer = "org.mpris.MediaPlayer2.Player"
)

func (m *mpris) players() []string {
	var names []string
	if err := m.conn.BusObject().Call("org.freedesktop.DBus.ListNames", 0).Store(&names); err != nil {
		log.Printf("mpris: list: %v", err)
		return nil
	}
	var out []string
	for _, n := range names {
		if strings.HasPrefix(n, mprisPrefix) {
			out = append(out, n)
		}
	}
	return out
}

func (m *mpris) status(name string) string {
	v, err := m.conn.Object(name, mprisPath).GetProperty(mprisPlayer + ".PlaybackStatus")
	if err != nil {
		return ""
	}
	s, _ := v.Value().(string)
	return s
}

func short(name string) string { return strings.TrimPrefix(name, mprisPrefix) }

// pauseAll pauses every playing player and remembers it.
func (m *mpris) pauseAll() {
	if m == nil {
		return
	}
	m.paused = m.paused[:0]
	for _, n := range m.players() {
		if m.status(n) != "Playing" {
			continue
		}
		if err := m.conn.Object(n, mprisPath).Call(mprisPlayer+".Pause", 0).Err; err != nil {
			log.Printf("mpris: pause %s: %v", short(n), err)
			continue
		}
		m.paused = append(m.paused, n)
		log.Printf("mpris: paused %s", short(n))
	}
}

// resume plays back only what pauseAll stopped, and only if it is still
// sitting paused (the user may have started or stopped it themselves).
func (m *mpris) resume() {
	if m == nil {
		return
	}
	for _, n := range m.paused {
		if m.status(n) != "Paused" {
			log.Printf("mpris: %s changed while away, leaving it", short(n))
			continue
		}
		if err := m.conn.Object(n, mprisPath).Call(mprisPlayer+".Play", 0).Err; err != nil {
			log.Printf("mpris: play %s: %v", short(n), err)
			continue
		}
		log.Printf("mpris: resumed %s", short(n))
	}
	m.paused = m.paused[:0]
}
