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

// stopCallSlack is how long a graceful stop may take beyond its grace
// period (Docker's own work) before the call is abandoned.
const stopCallSlack = time.Minute

// enforceTimeouts stops the runners whose job exceeded the target's
// max_job_minutes. The runner's watch then goes through finishRunner as for
// any other exit (logs archived, slot cleaned and released).
func (m *Manager) enforceTimeouts() {
	now := time.Now()
	for _, t := range m.liveTargets() {
		limit, overdue := t.claimOverdue(now)
		for _, r := range overdue {
			t.log.Warn("job exceeded the time limit, stopping its runner", "runner", r.name, "job", r.jobName, "limit", limit)
			// In the background: a graceful stop blocks up to its grace period.
			go t.stopOverdue(r)
		}
	}
}

// claimOverdue marks the runners over the target's job time limit as
// killed, so that one check stops each only once, and returns them with
// the limit.
func (t *target) claimOverdue(now time.Time) (time.Duration, []*runner) {
	t.mu.Lock()
	defer t.mu.Unlock()
	limit := time.Duration(t.t.MaxJobMinutes) * time.Minute
	var overdue []*runner
	for _, r := range t.runners {
		if timedOut(r, limit, now) {
			r.killed = true
			overdue = append(overdue, r)
		}
	}
	return limit, overdue
}

// stopOverdue stops a runner claimed by claimOverdue. On failure the claim
// is dropped, so the next check retries.
func (t *target) stopOverdue(r *runner) {
	if err := t.m.stopGracefully(r.containerID); err != nil {
		t.log.Error("stop runner", "runner", r.name, "error", err)
		t.mu.Lock()
		r.killed = false
		t.mu.Unlock()
	}
}

// stopGracefully stops a runner container: SIGTERM, so the runner can
// cancel its job and report it, then a kill after timeoutStopGrace.
func (m *Manager) stopGracefully(containerID string) error {
	ctx, cancel := context.WithTimeout(context.Background(), timeoutStopGrace+stopCallSlack)
	defer cancel()
	return m.pool.StopRunner(ctx, containerID, timeoutStopGrace)
}
