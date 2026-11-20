package server

import (
	"encoding/json"
	"net/http"
	"sync"
	"time"
)

const (
	// eventsHeartbeat is how often the event stream sends the state even
	// when nothing changed (it refreshes the host stats).
	eventsHeartbeat = 5 * time.Second
	// eventsCoalesce is how long the event stream waits after a change, so
	// a burst of changes sends one state.
	eventsCoalesce = 150 * time.Millisecond
)

// snapshot is the state the dashboard shows.
func (s *Server) snapshot() map[string]any {
	return map[string]any{
		"targets": s.mgr.Snapshot(),
		"slots":   s.pool.Slots(),
		"host":    readHost(s.cfg.DataDir, &s.cpu),
		"memory":  s.pool.Memory(),
		"time":    time.Now().Unix(),
	}
}

func (s *Server) handleState(w http.ResponseWriter, r *http.Request) { writeJSON(w, s.snapshot()) }

// handleEvents streams the state as server-sent events: on connection,
// after every change and every eventsHeartbeat.
func (s *Server) handleEvents(w http.ResponseWriter, r *http.Request) {
	fl, ok := w.(http.Flusher)
	if !ok {
		writeErr(w, http.StatusInternalServerError, "streaming unsupported")
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("X-Accel-Buffering", "no")
	ch := s.hub.subscribe()
	defer s.hub.unsubscribe(ch)
	send := func() bool {
		b, _ := json.Marshal(s.snapshot())
		if _, err := w.Write([]byte("data: " + string(b) + "\n\n")); err != nil {
			return false
		}
		fl.Flush()
		return true
	}
	if !send() {
		return
	}
	heartbeat := time.NewTicker(eventsHeartbeat)
	defer heartbeat.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case <-ch:
			time.Sleep(eventsCoalesce)
			if !send() {
				return
			}
		case <-heartbeat.C:
			if !send() {
				return
			}
		}
	}
}

// hub tells the event streams that the state changed. A subscriber's channel
// holds at most one pending signal, so notify never blocks and a slow stream
// still sees the latest change.
type hub struct {
	mu   sync.Mutex // guards subs
	subs map[chan struct{}]bool
}

func newHub() *hub { return &hub{subs: map[chan struct{}]bool{}} }

// subscribe returns a channel signalled after each change.
func (h *hub) subscribe() chan struct{} {
	ch := make(chan struct{}, 1)
	h.mu.Lock()
	h.subs[ch] = true
	h.mu.Unlock()
	return ch
}

func (h *hub) unsubscribe(ch chan struct{}) {
	h.mu.Lock()
	delete(h.subs, ch)
	h.mu.Unlock()
}

// notify signals every subscriber without blocking.
func (h *hub) notify() {
	h.mu.Lock()
	defer h.mu.Unlock()
	for ch := range h.subs {
		select {
		case ch <- struct{}{}:
		default:
		}
	}
}
