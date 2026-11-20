package scaler

import (
	"context"
	"testing"
	"time"

	"github.com/TuroYT/pitlane/internal/pool"
	"github.com/TuroYT/pitlane/internal/store"
)

func TestRecordUsage(t *testing.T) {
	h := newHarness(t, store.Target{Kind: "org", Owner: "acme", Enabled: true, MaxRunners: 4}, 0)
	h.m.usageRetry = 10 * time.Millisecond
	u := pool.JobUsage{Wall: 4 * time.Second,
		Runner: pool.CgroupUsage{MemPeak: 100, CPUUsec: 2_000_000, CPUSomeUsec: 400_000, IOFullUsec: -1, MemFullUsec: 0, AnonPeak: -1},
		Slot:   pool.CgroupUsage{MemPeak: -1, CPUUsec: -1, CPUSomeUsec: -1, IOFullUsec: -1, MemFullUsec: -1, AnonPeak: -1}}

	// Unknown usage is never written.
	h.m.recordUsage("pitlane-x", pool.JobUsage{Runner: u.Slot, Slot: u.Slot})

	// The job is not recorded yet when the runner exits: retried.
	h.m.recordUsage("pitlane-x", u)
	if err := h.st.UpsertJob(store.Job{TargetID: h.t.cfg().ID, JobID: "j1", RunnerName: "pitlane-x", Slot: 0, StartedAt: 1}); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "usage recorded on retry", func() bool {
		j, err := h.st.JobByRunner(context.Background(), "pitlane-x")
		return err == nil && j.Usage != nil
	})
	j, _ := h.st.JobByRunner(context.Background(), "pitlane-x")
	got := j.Usage
	if got.WallMs != 4000 || *got.Runner.MemPeak != 100 || got.Runner.IOFullUsec != nil || *got.Runner.MemFullUsec != 0 ||
		got.Slot.MemPeak != nil || *got.MemPeak != 100 || *got.Runner.CPUPressure != 10 {
		t.Errorf("usage: %+v", got)
	}
}
