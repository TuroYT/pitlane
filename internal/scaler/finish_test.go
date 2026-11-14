package scaler

import (
	"errors"
	"os"
	"slices"
	"testing"
	"time"

	"github.com/TuroYT/pitlane/internal/store"
)

func (h *harness) stopped() []string { return h.p.snapshot().stopped }

func TestEnforceTimeoutsStopsOnceAndRetriesAfterError(t *testing.T) {
	h := newHarness(t, store.Target{Kind: "org", Owner: "acme", Enabled: true, MaxRunners: 4, MaxJobMinutes: 1}, 0)
	late := h.addRunner("late", 1, time.Hour, true, false)
	late.busySince = time.Now().Add(-2 * time.Minute)
	ok := h.addRunner("ok", 2, time.Hour, true, false)
	ok.busySince = time.Now().Add(-30 * time.Second)
	h.addRunner("idle", 3, time.Hour, false, false)

	h.p.stopErr = errors.New("daemon busy")
	h.m.enforceTimeouts()
	waitFor(t, "first stop", func() bool { return len(h.stopped()) == 1 })
	waitFor(t, "kill flag reset after the failed stop", func() bool {
		h.t.mu.Lock()
		defer h.t.mu.Unlock()
		return !late.killed
	})

	h.p.mu.Lock()
	h.p.stopErr = nil
	h.p.mu.Unlock()
	h.m.enforceTimeouts()
	waitFor(t, "retried stop", func() bool { return len(h.stopped()) == 2 })
	h.m.enforceTimeouts() // already stopped: not again
	time.Sleep(20 * time.Millisecond)
	if got := h.stopped(); !slices.Equal(got, []string{"ctr-late", "ctr-late"}) {
		t.Fatalf("stopped %v, want the late runner twice (failed, then retried) and nothing else", got)
	}
	h.t.mu.Lock()
	defer h.t.mu.Unlock()
	if !late.killed {
		t.Fatal("a stopped runner must stay marked killed")
	}
}

func TestFinishRunner(t *testing.T) {
	for _, tc := range []struct {
		name      string
		jobID     string // job still running on it
		removeErr error
		lost      bool
		forgotten bool
	}{
		{"job completed", "", nil, false, true},
		{"job did not complete", "j1", nil, true, true},
		{"already gone from GitHub", "", notFound(), false, true},
		{"GitHub unreachable keeps the record", "", errors.New("timeout"), false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t, store.Target{Kind: "org", Owner: "acme", Enabled: true, MaxRunners: 4}, 0)
			_ = h.st.AddRunner(store.RegisteredRunner{ID: 9, TargetID: h.t.cfg().ID, Name: "r"})
			r := h.addRunner("r", 9, time.Minute, true, false)
			r.jobID = tc.jobID
			h.api.removeErr[9] = tc.removeErr
			h.p.logs = "job output"
			h.m.finishRunner(h.t, r)

			s := h.p.snapshot()
			if !slices.Equal(s.removed, []string{"ctr-r"}) {
				t.Fatalf("container removed %v", s.removed)
			}
			if len(s.released) != 1 || s.released[0] != r.lease {
				t.Fatalf("slot released %+v, want the runner's lease", s.released)
			}
			if h.runnerCount() != 0 {
				t.Fatal("runner still tracked")
			}
			if _, lost := h.t.lost["j1"]; lost != tc.lost {
				t.Fatalf("lost job recorded: %v, want %v", lost, tc.lost)
			}
			if got := h.st.RunnerIDByName("r") == 0; got != tc.forgotten {
				t.Fatalf("record deleted: %v, want %v", got, tc.forgotten)
			}
			if b, err := os.ReadFile(h.m.LogFile("r")); err != nil || string(b) != "job output" {
				t.Fatalf("archived log %q %v", b, err)
			}
			if h.p.freeDisks != 1 {
				t.Fatal("disk check skipped")
			}
		})
	}
}

// The runner's watch goes through finishRunner once its container exits.
func TestWatchFinishesRunner(t *testing.T) {
	h := newHarness(t, store.Target{Kind: "org", Owner: "acme", Enabled: true, MaxRunners: 4}, 0)
	r := h.addRunner("r", 0, time.Minute, true, false)
	go h.t.watch(r)
	h.p.exit(r.containerID)
	waitFor(t, "finish", func() bool { return h.runnerCount() == 0 && len(h.p.snapshot().released) == 1 })
}
