package scaler

import (
	"errors"
	"log/slog"
	"slices"
	"testing"
	"time"

	"github.com/TuroYT/pitlane/internal/store"
)

// The timing of the drain is covered by TestDeleteScaleSetWhenDrained; these
// are the cases where there is nothing to delete or GitHub refuses.
func TestDeleteScaleSetWhenDrainedEdgeCases(t *testing.T) {
	t.Run("never connected: nothing to delete", func(t *testing.T) {
		h := newHarness(t, store.Target{Kind: "org", Owner: "acme", Enabled: true, MaxRunners: 2}, 0)
		clk := newFakeClock()
		h.t.mu.Lock()
		h.t.client, h.t.scaleSetID = nil, 0
		h.t.mu.Unlock()
		h.t.deleteScaleSetWhenDrained(clk.now, func(time.Duration) {})
		if len(h.api.deleted) != 0 {
			t.Fatalf("deleted %v", h.api.deleted)
		}
	})
	t.Run("GitHub refuses", func(t *testing.T) {
		h := newHarness(t, store.Target{Kind: "org", Owner: "acme", Enabled: true, MaxRunners: 2}, 0)
		clk := newFakeClock()
		rec := &recordingLog{}
		h.t.log = slog.New(rec)
		h.api.deleteErr = errors.New("403")
		h.t.deleteScaleSetWhenDrained(clk.now, func(time.Duration) {})
		if !rec.has("delete scale set") {
			t.Fatalf("failure not logged: %v", rec.logs)
		}
	})
}

func TestStopKeepsScaleSetUnlessRemoving(t *testing.T) {
	h := newHarness(t, store.Target{Kind: "org", Owner: "acme", Enabled: true, MaxRunners: 2}, 0)
	h.t.stop(false)
	if h.t.ctx.Err() == nil || len(h.api.deleted) != 0 {
		t.Fatalf("stop(false): canceled %v, deleted %v", h.t.ctx.Err(), h.api.deleted)
	}
	h.t.stop(true) // no runner left: deleted at once
	if !slices.Equal(h.api.deleted, []int{42}) {
		t.Fatalf("stop(true) deleted %v", h.api.deleted)
	}
}

func TestTargetUpdate(t *testing.T) {
	h := newHarness(t, store.Target{Kind: "org", Owner: "acme", Enabled: true, MaxRunners: 2}, 0)
	next := h.t.cfg()
	next.MaxRunners = 7
	h.t.update(next)
	if h.t.cfg().MaxRunners != 7 {
		t.Fatal("config not applied")
	}
	h.api.session.mu.Lock()
	max := h.api.session.max
	h.api.session.mu.Unlock()
	if max != 7 {
		t.Fatalf("session max %d", max)
	}

	// Before the listener connected there is no session to tell.
	h.t.mu.Lock()
	h.t.session = nil
	h.t.mu.Unlock()
	next.MaxRunners = 8
	h.t.update(next)
	if h.t.cfg().MaxRunners != 8 {
		t.Fatal("config not applied without a session")
	}
}
