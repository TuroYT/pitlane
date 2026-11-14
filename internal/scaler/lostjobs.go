package scaler

import (
	"context"
	"time"
)

// Lost jobs: when a busy runner container disappears (killed for exceeding
// the job time limit, OOM, docker rm, removed while Pitlane was stopped),
// GitHub keeps its job "in progress" and the runner "offline, busy" for many
// minutes. The scale set statistics keep counting that job in
// TotalAssignedJobs, so without correction Pitlane would start a fresh
// runner for it, which then sits idle (the job is bound to the dead runner)
// and holds a slot until GitHub gives up.
//
// Pitlane remembers such jobs, keyed by GitHub job ID, and subtracts them
// from the demand. An entry is dropped when:
//   - the job's JobCompleted message arrives;
//   - the job is started again on another runner (it needs a runner again);
//   - the orphan cleanup sees its runner offline and idle on GitHub (the job
//     is no longer bound to it) or no longer registered;
//   - lostJobTTL elapses, so a mistake can never block scaling for long.

const lostJobTTL = 30 * time.Minute

type lostJob struct {
	runner string
	at     time.Time
}

// pruneLostJobs drops expired entries and returns how many remain.
func pruneLostJobs(lost map[string]lostJob, now time.Time) int {
	for id, l := range lost {
		if now.Sub(l.at) > lostJobTTL {
			delete(lost, id)
		}
	}
	return len(lost)
}

// effectiveDesired is GitHub's assigned job count minus the jobs bound to
// runners Pitlane knows are dead.
func effectiveDesired(assigned, lost int) int {
	return max(0, assigned-lost)
}

// markLost records that runner died while running job. Callers hold t.mu.
func (t *target) markLostLocked(jobID, runner string, now time.Time) {
	if jobID == "" {
		return
	}
	if _, ok := t.lost[jobID]; !ok {
		t.log.Info("runner gone before its job completed, not replacing it while GitHub still counts the job",
			"runner", runner, "job", jobID)
	}
	t.lost[jobID] = lostJob{runner: runner, at: now}
}

// runnerGone is called once a runner's container is gone. If its job did
// not complete yet, the job is recorded as lost.
func (t *target) runnerGone(r *runner) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if r.busy && r.jobID != "" {
		t.markLostLocked(r.jobID, r.name, time.Now())
	}
}

// forgetLostRunner drops the lost jobs of a runner that GitHub no longer
// reports busy (or no longer knows).
func (t *target) forgetLostRunner(name string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	for id, l := range t.lost {
		if l.runner == name {
			delete(t.lost, id)
		}
	}
}

// findLostJobs runs at startup. Jobs that started and did not finish on a
// runner still registered on GitHub are either running on an adopted runner
// (the runner learns its job, so a later kill is tracked) or bound to a
// runner whose container is gone: those are recorded as lost.
func (m *Manager) findLostJobs(ctx context.Context, running map[string]*runner) {
	jobs, err := m.store.InFlightJobs(ctx)
	if err != nil {
		m.log.Warn("find in-flight jobs", "error", err)
		return
	}
	now := time.Now()
	for _, j := range jobs {
		if r, ok := running[j.RunnerName]; ok {
			r.jobID = j.JobID
			continue
		}
		if m.lost[j.TargetID] == nil {
			m.lost[j.TargetID] = map[string]lostJob{}
		}
		m.lost[j.TargetID][j.JobID] = lostJob{runner: j.RunnerName, at: now}
		m.log.Info("runner gone while Pitlane was stopped, not replacing it while GitHub still counts its job",
			"runner", j.RunnerName, "job", j.JobID)
	}
}

// keepLostLocked hands the lost jobs of a target being restarted to its
// next listener. Callers hold m.mu.
func (m *Manager) keepLostLocked(id int64, t *target) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if len(t.lost) == 0 {
		return
	}
	if m.lost[id] == nil {
		m.lost[id] = map[string]lostJob{}
	}
	for jobID, l := range t.lost {
		m.lost[id][jobID] = l
	}
}
