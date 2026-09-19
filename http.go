package main

import (
	"embed"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"sync"
	"time"
)

//go:embed web/index.html
var webFS embed.FS

// hub fans sensor frames and status out to SSE subscribers.
type hub struct {
	mu     sync.Mutex
	subs   map[chan string]struct{}
	params Params
	status string
}

func newHub() *hub { return &hub{subs: map[chan string]struct{}{}} }

func (h *hub) publish(kind string, v any) {
	b, _ := json.Marshal(v)
	msg := fmt.Sprintf("event: %s\ndata: %s\n\n", kind, b)
	h.mu.Lock()
	defer h.mu.Unlock()
	for ch := range h.subs {
		select {
		case ch <- msg:
		default: // slow client; drop
		}
	}
}

func (h *hub) setParams(p Params) {
	h.mu.Lock()
	h.params = p
	h.mu.Unlock()
	h.publish("params", p)
}

func (h *hub) serve(addr string) {
	mux := http.NewServeMux()
	mux.HandleFunc("/index.html", func(w http.ResponseWriter, r *http.Request) {
		b, _ := webFS.ReadFile("web/index.html")
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Write(b)
	})
	mux.HandleFunc("/params", func(w http.ResponseWriter, r *http.Request) {
		h.mu.Lock()
		p := h.params
		h.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(p)
	})
	mux.HandleFunc("/events", func(w http.ResponseWriter, r *http.Request) {
		fl, ok := w.(http.Flusher)
		if !ok {
			http.Error(w, "no streaming", 500)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		ch := make(chan string, 64)
		h.mu.Lock()
		h.subs[ch] = struct{}{}
		p := h.params
		h.mu.Unlock()
		defer func() {
			h.mu.Lock()
			delete(h.subs, ch)
			h.mu.Unlock()
		}()
		b, _ := json.Marshal(p)
		fmt.Fprintf(w, "event: params\ndata: %s\n\n", b)
		fl.Flush()
		keep := time.NewTicker(15 * time.Second)
		defer keep.Stop()
		for {
			select {
			case <-r.Context().Done():
				return
			case m := <-ch:
				fmt.Fprint(w, m)
				fl.Flush()
			case <-keep.C:
				fmt.Fprint(w, ": keepalive\n\n")
				fl.Flush()
			}
		}
	})
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/index.html", http.StatusFound)
	})
	log.Printf("http: listening on %s", addr)
	if err := http.ListenAndServe(addr, mux); err != nil {
		log.Printf("http: %v", err)
	}
}

type frameEvent struct {
	T     int64  `json:"t"`
	State byte   `json:"state"`
	MvCM  uint16 `json:"mv"`
	MvEn  byte   `json:"mvE"`
	StCM  uint16 `json:"st"`
	StEn  byte   `json:"stE"`
	Det   uint16 `json:"det"`
	MG    []int  `json:"mg,omitempty"`
	SG    []int  `json:"sg,omitempty"`
}

func ints(b []byte) []int {
	if b == nil {
		return nil
	}
	out := make([]int, len(b))
	for i, v := range b {
		out[i] = int(v)
	}
	return out
}

func toEvent(f Frame, now time.Time) frameEvent {
	return frameEvent{T: now.UnixMilli(), State: f.State, MvCM: f.MovingCM, MvEn: f.MovingEn,
		StCM: f.StaticCM, StEn: f.StaticEn, Det: f.DetectCM, MG: ints(f.MovingGates), SG: ints(f.StaticGates)}
}
