package server

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/TuroYT/pitlane/internal/pool"
	"github.com/TuroYT/pitlane/internal/store"
)

// stateBody is the part of the dashboard state the tests read.
type stateBody struct {
	Targets []struct {
		Owner string `json:"owner"`
		Repo  string `json:"repo"`
	} `json:"targets"`
	Slots  []pool.SlotState `json:"slots"`
	Host   hostStats        `json:"host"`
	Memory pool.MemoryState `json:"memory"`
	Time   int64            `json:"time"`
}

func TestStateEndpoint(t *testing.T) {
	t.Parallel()
	_, st, fp, h := apiServer(t)
	fp.setSlots([]pool.SlotState{{Index: 0, Status: "busy", Runner: "pitlane-a", StorageDriver: "vfs"}})
	fp.setMem(pool.MemoryState{Enabled: true, Budget: 123})
	if _, err := st.CreateTarget(store.Target{Kind: "repo", Owner: "acme", Repo: "api", Enabled: true, MaxRunners: 1}); err != nil {
		t.Fatal(err)
	}
	if w := get(h, "/api/state", ""); w.Code != http.StatusUnauthorized {
		t.Fatalf("anonymous: %d", w.Code)
	}
	w := get(h, "/api/state", "vera")
	var got stateBody
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil || w.Code != http.StatusOK {
		t.Fatalf("%d %s: %v", w.Code, w.Body, err)
	}
	if len(got.Targets) != 1 || got.Targets[0].Owner != "acme" || len(got.Slots) != 1 || got.Slots[0].Runner != "pitlane-a" ||
		got.Memory.Budget != 123 || got.Host.CPUs < 1 || got.Time == 0 {
		t.Fatalf("state %+v", got)
	}
	if !strings.Contains(w.Body.String(), `"storageDriver":"vfs"`) {
		t.Fatalf("the slot storage driver is missing: %s", w.Body)
	}
}

// readEvent reads the next "data:" line of an event stream and decodes it.
func readEvent(t *testing.T, br *bufio.Reader) stateBody {
	t.Helper()
	for {
		line, err := br.ReadString('\n')
		if err != nil {
			t.Fatalf("event stream ended: %v", err)
		}
		if data, ok := strings.CutPrefix(line, "data: "); ok {
			var ev stateBody
			if err := json.Unmarshal([]byte(data), &ev); err != nil {
				t.Fatalf("event %q: %v", data, err)
			}
			return ev
		}
	}
}

func TestEventsStream(t *testing.T) {
	t.Parallel()
	s, st, fp, h := apiServer(t)
	done := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer close(done)
		h.ServeHTTP(w, r)
	}))
	t.Cleanup(srv.Close)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, "GET", srv.URL+"/api/events", nil)
	req.AddCookie(session("vera"))
	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK || resp.Header.Get("Content-Type") != "text/event-stream" ||
		resp.Header.Get("Cache-Control") != "no-cache" || resp.Header.Get("X-Accel-Buffering") != "no" ||
		resp.Header.Get("Content-Security-Policy") == "" {
		t.Fatalf("%d %v", resp.StatusCode, resp.Header)
	}
	br := bufio.NewReader(resp.Body)

	// The snapshot comes at once, without any change.
	if ev := readEvent(t, br); ev.Memory.Budget != 0 || ev.Time == 0 || len(ev.Targets) != 0 {
		t.Fatalf("initial snapshot %+v", ev)
	}
	// A change is pushed.
	fp.setMem(pool.MemoryState{Budget: 7})
	if _, err := st.CreateTarget(store.Target{Kind: "repo", Owner: "acme", Repo: "api", Enabled: true, MaxRunners: 1}); err != nil {
		t.Fatal(err)
	}
	s.Notify()
	if ev := readEvent(t, br); ev.Memory.Budget != 7 || len(ev.Targets) != 1 {
		t.Fatalf("snapshot after a change %+v", ev)
	}

	// A closed connection ends the handler and drops its subscription.
	cancel()
	<-done
	s.hub.mu.Lock()
	n := len(s.hub.subs)
	s.hub.mu.Unlock()
	if n != 0 {
		t.Fatalf("%d subscribers left after the client went away", n)
	}
}

// noFlush is a ResponseWriter that cannot stream.
type noFlush struct{ http.ResponseWriter }

// brokenWriter is a ResponseWriter whose client is gone.
type brokenWriter struct{ http.ResponseWriter }

func (brokenWriter) Write([]byte) (int, error) { return 0, errors.New("broken pipe") }
func (brokenWriter) Flush()                    {}

func TestEventsHandlerEdgeCases(t *testing.T) {
	t.Parallel()
	s, _, _, _ := apiServer(t)
	t.Run("no streaming support", func(t *testing.T) {
		rec := httptest.NewRecorder()
		s.handleEvents(noFlush{rec}, httptest.NewRequest("GET", "/api/events", nil))
		if rec.Code != http.StatusInternalServerError || !bodyHas(rec, "streaming unsupported") {
			t.Fatalf("%d %s", rec.Code, rec.Body)
		}
	})
	t.Run("client gone at once", func(t *testing.T) {
		done := make(chan struct{})
		go func() {
			defer close(done)
			s.handleEvents(brokenWriter{httptest.NewRecorder()}, httptest.NewRequest("GET", "/api/events", nil))
		}()
		select {
		case <-done:
		case <-time.After(10 * time.Second):
			t.Fatal("handler still running on a broken connection")
		}
		s.hub.mu.Lock()
		defer s.hub.mu.Unlock()
		if len(s.hub.subs) != 0 {
			t.Fatalf("%d subscribers left", len(s.hub.subs))
		}
	})
}

func TestHubNotifyNeverBlocks(t *testing.T) {
	t.Parallel()
	h := newHub()
	slow, fast := h.subscribe(), h.subscribe()
	for range 3 { // the slow subscriber never reads: its signal is not queued twice
		h.notify()
	}
	<-fast
	select {
	case <-fast:
		t.Fatal("one change signalled twice")
	default:
	}
	<-slow
	h.unsubscribe(slow)
	h.notify() // no subscriber blocks, none panics after unsubscribe
	select {
	case <-slow:
		t.Fatal("unsubscribed channel signalled")
	default:
	}
	<-fast
}
