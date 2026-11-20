package scaler

import (
	"context"
	"time"
)

// jobStart is when a runner's job started, for its time limit: GitHub's
// assignment time, since the JobStarted message can arrive tens of seconds
// later. A zero, future or impossible time (before the runner existed, e.g.
// a skewed clock) falls back to now.
func jobStart(assigned, runnerCreated, now time.Time) time.Time {
	if assigned.IsZero() || assigned.After(now) || assigned.Before(runnerCreated) {
		return now
	}
	return assigned
}

// timedOut reports whether a runner's job ran longer than limit (0 = no limit).
func timedOut(r *runner, limit time.Duration, now time.Time) bool {
	return limit > 0 && r.busy && !r.exited && !r.killed && !r.busySince.IsZero() && now.Sub(r.busySince) > limit
}

// timeoutStopGrace is how long a runner over its time limit has, after
// SIGTERM, to cancel its job and report it before it is killed.
const timeoutStopGrace = 30 * time.Second

// enforceTimeouts stops the runners whose job exceeded the target's
// max_job_minutes. The runner's watch then goes through finishRunner as for
// any other exit (logs archived, slot cleaned and released).
func (m *Manager) enforceTimeouts() {
	m.mu.Lock()
	ts := make([]*target, 0, len(m.targets))
	for _, t := range m.targets {
		ts = append(ts, t)
	}
	m.mu.Unlock()
	now := time.Now()
	for _, t := range ts {
		t.mu.Lock()
		limit := time.Duration(t.t.MaxJobMinutes) * time.Minute
		var kill []*runner
		for _, r := range t.runners {
			if timedOut(r, limit, now) {
				r.killed = true
				kill = append(kill, r)
			}
		}
		t.mu.Unlock()
		for _, r := range kill {
			t.log.Warn("job exceeded the time limit, stopping its runner", "runner", r.name, "job", r.jobName, "limit", limit)
			// In the background: a graceful stop blocks up to its grace period.
			go func(t *target, r *runner) {
				ctx, cancel := context.WithTimeout(context.Background(), timeoutStopGrace+time.Minute)
				defer cancel()
				if err := m.pool.StopRunner(ctx, r.containerID, timeoutStopGrace); err != nil {
					t.log.Error("stop runner", "runner", r.name, "error", err)
					t.mu.Lock()
					r.killed = false // retried at the next check
					t.mu.Unlock()
				}
			}(t, r)
		}
	}
}
