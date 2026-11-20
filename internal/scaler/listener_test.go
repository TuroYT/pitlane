package scaler

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/TuroYT/pitlane/internal/store"
)

// runListener starts the target's run loop and returns a channel closed
// when it returns.
func runListener(h *harness) <-chan struct{} {
	done := make(chan struct{})
	go func() {
		h.t.run(context.Background())
		close(done)
	}()
	return done
}

func (h *harness) status() (string, string) {
	h.t.mu.Lock()
	defer h.t.mu.Unlock()
	return h.t.status, h.t.err
}

func TestListenerRetriesWithGrowingBackoff(t *testing.T) {
	h := newHarness(t, store.Target{Kind: "org", Owner: "acme", Enabled: true, MaxRunners: 2}, 0)
	clk := newFakeClock()
	clk.install(h.m)
	var (
		calls    atomic.Int32
		mu       sync.Mutex
		statuses []string
	)
	clk.onAfter = func(int, time.Duration) {
		s, e := h.status()
		mu.Lock()
		statuses = append(statuses, s+": "+e)
		mu.Unlock()
	}
	h.m.connect = func(Credentials, store.Target, string) (scaleSetAPI, error) {
		if n := calls.Add(1); n <= 3 {
			return nil, fmt.Errorf("github down %d", n)
		}
		return h.api, nil
	}
	done := runListener(h)
	<-h.api.session.running
	if s, e := h.status(); s != "listening" || e != "" {
		t.Fatalf("status %q %q once connected, want listening", s, e)
	}
	h.t.cancel()
	<-done

	if got, want := clk.waited(), []time.Duration{5 * time.Second, 10 * time.Second, 20 * time.Second}; !slices.Equal(got, want) {
		t.Fatalf("waited %v, want %v", got, want)
	}
	mu.Lock()
	defer mu.Unlock()
	if want := []string{"error: github down 1", "error: github down 2", "error: github down 3"}; !slices.Equal(statuses, want) {
		t.Fatalf("statuses %q, want %q", statuses, want)
	}
	if h.api.session.closed != 1 {
		t.Fatalf("session closed %d times, want once", h.api.session.closed)
	}
}

func TestListenerBackoffIsCapped(t *testing.T) {
	h := newHarness(t, store.Target{Kind: "org", Owner: "acme", Enabled: true, MaxRunners: 2}, 0)
	clk := newFakeClock()
	clk.install(h.m)
	clk.onAfter = func(n int, _ time.Duration) {
		if n == 8 {
			h.t.cancel()
		}
	}
	h.m.connect = func(Credentials, store.Target, string) (scaleSetAPI, error) { return nil, errors.New("down") }
	<-runListener(h)
	s := time.Second
	want := []time.Duration{5 * s, 10 * s, 20 * s, 40 * s, 80 * s, 120 * s, 120 * s, 120 * s}
	if got := clk.waited(); !slices.Equal(got, want) {
		t.Fatalf("waited %v, want %v", got, want)
	}
}

// A listener that stayed up longer than listenHealthy starts over with the
// shortest delay, so one failure after hours of service is not penalized.
func TestListenerBackoffResetsAfterHealthyRun(t *testing.T) {
	h := newHarness(t, store.Target{Kind: "org", Owner: "acme", Enabled: true, MaxRunners: 2}, 0)
	clk := newFakeClock()
	clk.install(h.m)
	clk.onAfter = func(n int, _ time.Duration) {
		if n == 4 {
			h.t.cancel()
		}
	}
	var calls atomic.Int32
	h.m.connect = func(Credentials, store.Target, string) (scaleSetAPI, error) {
		if calls.Add(1) == 3 {
			clk.advance(listenHealthy + time.Second) // the third listener ran for a while
		}
		return nil, errors.New("down")
	}
	<-runListener(h)
	s := time.Second
	if got, want := clk.waited(), []time.Duration{5 * s, 10 * s, 5 * s, 10 * s}; !slices.Equal(got, want) {
		t.Fatalf("waited %v, want %v", got, want)
	}
}

// A session that ends without an error is still a failure to retry, and the
// UI says so.
func TestListenerRestartsWhenSessionEndsCleanly(t *testing.T) {
	h := newHarness(t, store.Target{Kind: "org", Owner: "acme", Enabled: true, MaxRunners: 2}, 0)
	clk := newFakeClock()
	clk.install(h.m)
	var (
		runs    atomic.Int32
		second  = make(chan struct{})
		stopped = make(chan [2]string, 1)
	)
	clk.onAfter = func(int, time.Duration) {
		s, e := h.status()
		stopped <- [2]string{s, e}
	}
	h.api.session.runFn = func(ctx context.Context) error {
		if runs.Add(1) == 1 {
			return nil
		}
		close(second)
		<-ctx.Done()
		return ctx.Err()
	}
	done := runListener(h)
	<-second
	h.t.cancel()
	<-done
	if got := <-stopped; got != [2]string{"error", "listener stopped"} {
		t.Fatalf("status while waiting to retry: %q", got)
	}
	if h.api.session.closed != 2 {
		t.Fatalf("session closed %d times, want once per listen", h.api.session.closed)
	}
}

func TestListenStages(t *testing.T) {
	boom := errors.New("boom")
	for _, tc := range []struct {
		name   string
		setup  func(h *harness)
		want   error
		warned string
	}{
		{"connect fails", func(h *harness) {
			h.m.connect = func(Credentials, store.Target, string) (scaleSetAPI, error) { return nil, boom }
		}, boom, ""},
		{"scale set lookup fails", func(h *harness) { h.api.ensureErr = boom }, boom, ""},
		{"session cannot open", func(h *harness) { h.api.openErr = boom }, boom, ""},
		{"session fails", func(h *harness) {
			h.api.session.runFn = func(context.Context) error { return boom }
		}, boom, ""},
		{"canceled is a clean stop", func(h *harness) { h.t.cancel() }, nil, ""},
		{"close failure is only logged", func(h *harness) {
			h.t.cancel()
			h.api.session.closeErr = boom
		}, nil, "close message session"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t, store.Target{Kind: "org", Owner: "acme", Enabled: true, MaxRunners: 2}, 0)
			rec := &recordingLog{}
			h.t.log = slog.New(rec)
			tc.setup(h)
			if err := h.t.listen(); !errors.Is(err, tc.want) {
				t.Fatalf("listen returned %v, want %v", err, tc.want)
			}
			if tc.warned != "" && !rec.has(tc.warned) {
				t.Fatalf("no %q warning logged: %v", tc.warned, rec.logs)
			}
		})
	}
}

// A new or renamed scale set gets its ID recorded for the UI.
func TestResolveScaleSetRecordsID(t *testing.T) {
	h := newHarness(t, store.Target{Kind: "org", Owner: "acme", Enabled: true, MaxRunners: 2}, 0)
	rec := &recordingLog{}
	h.t.log = slog.New(rec)
	h.api.ensureID, h.api.created = 77, true
	id, err := h.t.resolveScaleSet(h.api, h.t.cfg())
	if err != nil || id != 77 {
		t.Fatalf("resolved %d, %v", id, err)
	}
	if got, _ := h.st.Target(h.t.cfg().ID); got.ScaleSetID != 77 {
		t.Fatalf("stored scale set ID %d, want 77", got.ScaleSetID)
	}
	if !rec.has("scale set created") {
		t.Fatalf("creation not logged: %v", rec.logs)
	}

	// Known already: nothing to write, and a store failure is only logged.
	cfg := h.t.cfg()
	cfg.ScaleSetID = 77
	if _, err := h.t.resolveScaleSet(h.api, cfg); err != nil {
		t.Fatal(err)
	}
	_ = h.st.Close()
	cfg.ScaleSetID = 5
	if id, err := h.t.resolveScaleSet(h.api, cfg); err != nil || id != 77 {
		t.Fatalf("a store failure must not fail the listener: %d, %v", id, err)
	}
	if !rec.has("save scale set id") {
		t.Fatalf("store failure not logged: %v", rec.logs)
	}
}
