package scaler

import (
	"maps"
	"time"

	"github.com/TuroYT/pitlane/internal/pool"
	"github.com/TuroYT/pitlane/internal/store"
)

// Runners left by a previous process: their containers keep running while
// Pitlane restarts. Start sorts them, and Sync hands the adopted ones to
// their target's next listener.

// adoptContainers sorts the runner containers left by a previous process:
// running ones of an enabled target are adopted, exited ones are finished,
// and the others are stopped. It returns the adopted runners by name.
func (m *Manager) adoptContainers(containers []pool.Orphan, enabled map[int64]store.Target) map[string]*runner {
	running := map[string]*runner{}
	for _, o := range containers {
		r := m.leftoverRunner(o)
		_, known := enabled[o.TargetID]
		switch {
		case o.Untrusted || !known:
			// No listener would ever watch it: stop it now. Its owner cannot
			// be trusted, so its slot is rebuilt.
			r.lease = untrustedLease(r.lease)
			m.log.Warn("runner container from a previous run has no known enabled target, stopping it",
				"container", o.Name, "id", o.ID, "target", o.TargetID, "slot", o.Slot, "running", o.Running)
			go m.stopUntracked(r, o.Running)
		case !o.Running:
			go m.finishRunner(nil, r)
		default:
			m.log.Info("adopting runner from previous run", "runner", o.Name, "slot", o.Slot, "strict", o.Strict)
			m.mu.Lock()
			m.adopted[o.TargetID] = append(m.adopted[o.TargetID], r)
			m.mu.Unlock()
			running[o.Name] = r
		}
	}
	return running
}

// leftoverRunner rebuilds the runner of a container left by a previous
// process.
func (m *Manager) leftoverRunner(o pool.Orphan) *runner {
	// Assume busy since the container was created: the job timeout then
	// still applies after a restart.
	// The lease comes from the container's labels: a strict runner is
	// released (slot rebuilt, throwaway caches removed) as strict.
	now := time.Now()
	r := &runner{name: o.Name, containerID: o.ID, lease: o.Lease(), busy: true, startedAt: now, busySince: o.Created}
	if !plausible(o.Created) {
		r.busySince = now
	}
	r.runnerID = m.store.RunnerIDByName(o.Name)
	return r
}

// untrustedLease turns the lease of a runner whose target is unknown into a
// strict one: its slot is rebuilt and handed to nobody before that.
func untrustedLease(l pool.Lease) pool.Lease {
	l.Strict, l.Owner, l.TargetID = true, pool.StrictOwner, 0
	return l
}

// stopUntracked stops a runner container left by a previous run that no
// target will adopt (target label missing or invalid, target deleted or
// disabled), the same way as a job over its time limit, then finishes it:
// logs archived, container removed (forcibly if the graceful stop failed),
// slot released as strict.
func (m *Manager) stopUntracked(r *runner, running bool) {
	if running {
		if err := m.stopGracefully(r.containerID); err != nil {
			m.log.Error("stop untracked runner, removing it forcibly", "container", r.name, "id", r.containerID, "error", err)
		}
	}
	m.finishRunner(nil, r)
}

// handOverLocked gives a new target the runners adopted at startup and the
// lost jobs kept for it. The target is not shared yet: its maps are filled
// before any of its goroutines starts. Callers hold m.mu.
func (m *Manager) handOverLocked(t *target) {
	cfg := t.t
	adopted := m.adopted[cfg.ID]
	for _, r := range adopted {
		// Adopted after a restart: its job is unknown, reserve what the
		// target's jobs usually need.
		r.predicted, _ = m.predict.predict(m.ctx, jobKey{owner: cfg.Owner, repo: cfg.Repo})
		m.pool.SetPrediction(r.lease.Slot, r.predicted)
		t.runners[r.name] = r
	}
	maps.Copy(t.lost, m.lost[cfg.ID])
	delete(m.adopted, cfg.ID)
	delete(m.lost, cfg.ID)
	for _, r := range adopted {
		go t.watch(r)
	}
}
