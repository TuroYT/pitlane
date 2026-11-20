package scaler

import (
	"context"
	"time"

	"github.com/TuroYT/pitlane/internal/pool"
	"github.com/TuroYT/pitlane/internal/store"
)

// defaultUsageRetry is how long recordUsage waits before trying again when
// the runner's job is not recorded yet (its JobStarted message can arrive
// after the runner exited).
const defaultUsageRetry = 15 * time.Second

// recordUsage stores what a runner's job used. A failure only loses the
// measurement.
func (m *Manager) recordUsage(runner string, u pool.JobUsage) {
	if !u.Known() {
		return
	}
	su := storeUsage(u)
	save := func() bool {
		ok, err := m.store.SetJobUsage(context.Background(), runner, su)
		if err != nil {
			m.log.Warn("record job resource usage", "runner", runner, "error", err)
			return true
		}
		return ok
	}
	if save() {
		return
	}
	go func() {
		time.Sleep(m.usageRetry)
		save() // still unknown: the runner never ran a job
	}()
}

// storeUsage converts a measurement to its stored form (-1 = unknown).
func storeUsage(u pool.JobUsage) store.JobUsage {
	v := func(n int64) *int64 {
		if n < 0 {
			return nil
		}
		return &n
	}
	c := func(c pool.CgroupUsage) store.ContainerUsage {
		return store.ContainerUsage{MemPeak: v(c.MemPeak), CPUUsec: v(c.CPUUsec), CPUSomeUsec: v(c.CPUSomeUsec), IOFullUsec: v(c.IOFullUsec), MemFullUsec: v(c.MemFullUsec)}
	}
	slot := c(u.Slot)
	slot.AnonPeak = v(u.Slot.AnonPeak) // only the slot's is stored (its peak counts the page cache)
	return store.JobUsage{WallMs: u.Wall.Milliseconds(), Runner: c(u.Runner), Slot: slot}
}
