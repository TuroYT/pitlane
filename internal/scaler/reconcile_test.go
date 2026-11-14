package scaler

import (
	"context"
	"errors"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/actions/scaleset"

	"github.com/TuroYT/pitlane/internal/store"
)

func (h *harness) setDemand(desired int, lost ...string) {
	h.t.mu.Lock()
	h.t.desired = desired
	for _, id := range lost {
		h.t.lost[id] = lostJob{runner: "pitlane-dead", at: time.Now()}
	}
	h.t.mu.Unlock()
}

func (h *harness) waiting() int {
	h.t.mu.Lock()
	defer h.t.mu.Unlock()
	return h.t.waiting
}

func TestReconcileTarget(t *testing.T) {
	for _, tc := range []struct {
		name     string
		min, max int
		desired  int
		lost     []string
		free     int
		start    int // runners started
		waiting  int
	}{
		{"desired jobs", 0, 4, 3, nil, 8, 3, 0},
		{"warm runners on top of the demand", 2, 4, 1, nil, 8, 3, 0},
		{"capped at max", 1, 4, 10, nil, 8, 4, 0},
		{"lost jobs are not replaced", 0, 4, 3, []string{"j1", "j2"}, 8, 1, 0},
		{"more lost jobs than assigned", 1, 4, 1, []string{"j1", "j2"}, 8, 1, 0},
		{"no free slot: the rest waits", 0, 4, 3, nil, 1, 1, 2},
		{"nothing wanted", 0, 4, 0, nil, 8, 0, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t, store.Target{Kind: "org", Owner: "acme", Enabled: true, MinRunners: tc.min, MaxRunners: tc.max}, tc.free)
			h.setDemand(tc.desired, tc.lost...)
			h.t.reconcile()
			waitFor(t, "runners started", func() bool { return h.startingCount() == 0 && h.runnerCount() == tc.start })
			if got := len(h.p.snapshot().acquired); got != tc.start {
				t.Fatalf("acquired %d slots, want %d", got, tc.start)
			}
			if got := h.waiting(); got != tc.waiting {
				t.Fatalf("waiting %d, want %d", got, tc.waiting)
			}
			// Already satisfied: a second pass starts nothing more.
			h.t.reconcile()
			time.Sleep(20 * time.Millisecond)
			if got := len(h.p.snapshot().acquired); got != tc.start {
				t.Fatalf("second reconcile acquired more slots: %d", got)
			}
			// Each started runner is registered (record kept for the orphan
			// cleanup) and got its JIT configuration.
			recs, _ := h.st.RegisteredRunners(h.t.cfg().ID)
			if len(recs) != tc.start {
				t.Fatalf("registered %d runners, want %d", len(recs), tc.start)
			}
			for _, spec := range h.p.snapshot().started {
				if spec.JITConfig != "jit-"+spec.Name || spec.Owner != "acme" {
					t.Fatalf("runner spec %+v", spec)
				}
			}
		})
	}
}

func TestReconcileRemovesOnlyOldIdleRunners(t *testing.T) {
	h := newHarness(t, store.Target{Kind: "org", Owner: "acme", Enabled: true, MaxRunners: 4}, 0)
	for _, r := range []store.RegisteredRunner{{ID: 1, Name: "young"}, {ID: 2, Name: "old"}, {ID: 3, Name: "busy"}} {
		r.TargetID = h.t.cfg().ID
		if err := h.st.AddRunner(r); err != nil {
			t.Fatal(err)
		}
	}
	h.addRunner("young", 1, 10*time.Second, false, false)
	h.addRunner("old", 2, time.Minute, false, false)
	h.addRunner("busy", 3, time.Hour, true, false)
	h.t.reconcile()
	if got := h.api.removedIDs(); !slices.Equal(got, []int64{2}) {
		t.Fatalf("deregistered %v, want only the runner idle for more than 30 s", got)
	}
	if got := h.p.snapshot().removed; !slices.Equal(got, []string{"ctr-old"}) {
		t.Fatalf("removed containers %v", got)
	}
	if h.st.RunnerIDByName("old") != 0 || h.st.RunnerIDByName("young") != 1 {
		t.Fatal("the removed runner's record must be deleted, the others kept")
	}
}

func TestReconcileKeepsRunnerWhenGitHubRefusesRemoval(t *testing.T) {
	h := newHarness(t, store.Target{Kind: "org", Owner: "acme", Enabled: true, MaxRunners: 4}, 0)
	_ = h.st.AddRunner(store.RegisteredRunner{ID: 7, TargetID: h.t.cfg().ID, Name: "idle"})
	h.addRunner("idle", 7, time.Minute, false, false)
	h.api.removeErr[7] = errors.New("runner is assigned a job")
	h.t.reconcile()
	if got := h.p.snapshot().removed; len(got) != 0 {
		t.Fatalf("container removed although GitHub refused: %v", got)
	}
	if h.st.RunnerIDByName("idle") != 7 {
		t.Fatal("record dropped although the runner is still registered")
	}
}

func TestReconcileReplacesNonStrictIdleRunners(t *testing.T) {
	h := newHarness(t, store.Target{Kind: "org", Owner: "acme", Enabled: true, Strict: true, MinRunners: 1, MaxRunners: 4}, 4)
	h.addRunner("fresh-loose", 1, time.Second, false, false) // stale even when young
	h.addRunner("busy-loose", 2, time.Hour, true, false)     // its job finishes first
	h.addRunner("refused", 3, time.Second, false, false)
	h.api.removeErr[3] = errors.New("job just assigned")
	h.setDemand(1) // the busy runner covers it
	h.t.reconcile()
	waitFor(t, "replacement started", func() bool { return h.startingCount() == 0 })
	if got := h.api.removedIDs(); !slices.Equal(got, []int64{1}) {
		t.Fatalf("deregistered %v, want the idle non-strict runner only", got)
	}
	if got := h.p.snapshot().removed; !slices.Equal(got, []string{"ctr-fresh-loose"}) {
		t.Fatalf("removed %v", got)
	}
	acq := h.p.snapshot().acquired
	if len(acq) != 1 || !acq[0].Strict {
		t.Fatalf("want one strict replacement (min runners 1), got %+v", acq)
	}
	// The refused one is retried at the next reconcile.
	delete(h.api.removeErr, 3)
	h.t.reconcile()
	if got := h.api.removedIDs(); !slices.Contains(got, 3) {
		t.Fatalf("refused stale runner not retried: %v", got)
	}
}

// A slot whose preparation fails is released, except when the wipe failed
// (the pool keeps it dirty for a rebuild).
func TestLaunchFailures(t *testing.T) {
	h := newHarness(t, store.Target{Kind: "org", Owner: "acme", Enabled: true, MaxRunners: 4}, 4)
	h.p.prepErr = errors.New("externals")
	h.setDemand(1)
	h.t.reconcile()
	waitFor(t, "launch", func() bool { return h.startingCount() == 0 && len(h.p.snapshot().released) == 1 })

	h.p.mu.Lock()
	h.p.prepErr, h.p.startErr = nil, errors.New("docker")
	h.p.mu.Unlock()
	h.t.reconcile()
	waitFor(t, "launch", func() bool { return h.startingCount() == 0 && len(h.p.snapshot().released) == 2 })
	if h.runnerCount() != 0 {
		t.Fatal("no runner should be tracked")
	}
	// The registration made for the failed runner is removed again.
	if got := h.api.removedIDs(); len(got) != 1 {
		t.Fatalf("JIT registration not removed: %v", got)
	}
	if recs, _ := h.st.RegisteredRunners(h.t.cfg().ID); len(recs) != 0 {
		t.Fatalf("record kept: %+v", recs)
	}
}

// reconcile, Scale and Snapshot run concurrently in production (listener,
// ticker, HTTP handlers): run them together under -race.
func TestReconcileConcurrentWithScaleAndSnapshot(t *testing.T) {
	h := newHarness(t, store.Target{Kind: "org", Owner: "acme", Enabled: true, MaxRunners: 3}, 3)
	s := &scaler{t: h.t, session: h.api.session}
	var wg sync.WaitGroup
	for i := range 4 {
		wg.Add(3)
		go func() {
			defer wg.Done()
			for range 20 {
				h.t.reconcile()
			}
		}()
		go func() {
			defer wg.Done()
			for j := range 20 {
				_ = s.Scale(context.Background(), &scaleset.RunnerScaleSetMessage{Statistics: &scaleset.RunnerScaleSetStatistic{TotalAssignedJobs: (i + j) % 4}})
			}
		}()
		go func() {
			defer wg.Done()
			for range 20 {
				for _, ts := range h.m.Snapshot() {
					if len(ts.Runners) > 3 {
						t.Errorf("more runners than max: %d", len(ts.Runners))
					}
				}
			}
		}()
	}
	wg.Wait()
	waitFor(t, "launches", func() bool { return h.startingCount() == 0 })
	if n := h.runnerCount(); n > 3 {
		t.Fatalf("%d runners for max 3", n)
	}
}
