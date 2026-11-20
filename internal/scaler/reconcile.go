package scaler

import (
	"context"
	"time"

	"github.com/TuroYT/pitlane/internal/pool"
	"github.com/TuroYT/pitlane/internal/store"
)

// idleGrace is how long a runner may sit without a job before scale-down
// may remove it: GitHub needs a few seconds to hand a fresh runner the job
// it was started for.
const idleGrace = 30 * time.Second

// staleForStrict reports whether an idle runner was started without strict
// isolation for a target that is strict now: reconcile replaces it.
func staleForStrict(r *runner, strict bool) bool {
	return strict && !r.lease.Strict && !r.busy && !r.exited
}

// reconcilePlan is the target state reconcile reads under t.mu, then acts
// on without the lock (admission and Docker calls are slow).
type reconcilePlan struct {
	client scaleSetAPI
	cfg    store.Target
	// want is the number of runners the target should have.
	want int
	// have counts live runners, including those still starting; waiting
	// counts those of them not running a job yet.
	have, waiting int
	// idle runners are old enough to be removed when there are too many;
	// stale ones are replaced right away (see staleForStrict) and are not
	// counted in have.
	idle, stale []*runner
	// preds is the memory of the pending jobs, heaviest first.
	preds []int64
	// since is when the target started waiting, its turn for admission.
	since time.Time
}

// reconcile starts runners until the target has what GitHub asked for (or
// no slot is free), and removes idle runners that are no longer needed.
func (t *target) reconcile() {
	if t.ctx.Err() != nil {
		return
	}
	t.reconcileMu.Lock()
	defer t.reconcileMu.Unlock()

	p, ok := t.plan(time.Now())
	if !ok {
		return
	}
	started, reason := t.startRunners(p)
	t.replaceStale(p.client, p.stale)
	t.scaleDown(p.client, p.idle, p.have+started-p.want)
	t.recordWait(max(0, p.want-p.have-started), started, reason)
}

// plan snapshots what reconcile needs. It reports false while the listener
// has no client yet.
func (t *target) plan(now time.Time) (reconcilePlan, bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.client == nil { // replaced by listen(): read under the lock only
		return reconcilePlan{}, false
	}
	p := reconcilePlan{client: t.client, cfg: t.t, since: now}
	demand := effectiveDesired(t.desired, pruneLostJobs(t.lost, now))
	p.want = min(p.cfg.MaxRunners, p.cfg.MinRunners+demand)
	p.have, p.waiting = t.starting, t.starting
	for _, r := range t.runners {
		switch {
		case r.exited:
		case staleForStrict(r, p.cfg.Strict):
			p.stale = append(p.stale, r)
		default:
			p.have++
			if !r.busy {
				p.waiting++
				if now.Sub(r.startedAt) > idleGrace {
					p.idle = append(p.idle, r)
				}
			}
		}
	}
	p.preds = pendingPredictions(t.pending, now)
	if t.waiting > 0 {
		p.since = t.waitSince
	}
	return p, true
}

// startRunners admits the missing runners and launches them in parallel,
// in the background: the listener is never blocked and a slow start does
// not delay the others. It stops at the first refusal and returns how many
// started and why the next one must wait ("" when none must).
func (t *target) startRunners(p reconcilePlan) (started int, reason string) {
	for i := range max(0, p.want-p.have) {
		lease, why := t.m.pool.Admit(t.ctx, pool.Request{Owner: p.cfg.Owner, TargetID: p.cfg.ID, Strict: p.cfg.Strict,
			Predicted: t.reservation(p, p.waiting+i), Since: p.since})
		if why != "" {
			return started, why
		}
		started++
		t.mu.Lock()
		t.starting++
		t.mu.Unlock()
		go t.launch(lease)
	}
	return started, ""
}

// reservation is the memory the k-th runner not running a job yet reserves.
// Those runners cover the heaviest pending jobs: GitHub picks which runner
// runs which job, so this is the safe bet. A warm runner, beyond the pending
// jobs, reserves what a typical job of the target needs.
func (t *target) reservation(p reconcilePlan, k int) int64 {
	if k < len(p.preds) {
		return p.preds[k]
	}
	predicted, _ := t.m.predict.predict(t.ctx, jobKey{owner: p.cfg.Owner, repo: p.cfg.Repo})
	return predicted
}

// replaceStale removes the idle runners started without strict isolation
// for a target that became strict (retried at every reconcile until none is
// left).
func (t *target) replaceStale(client scaleSetAPI, stale []*runner) {
	for _, r := range stale {
		t.retire(client, r, "replacing idle runner started without strict isolation")
	}
}

// scaleDown removes up to excess idle runners.
func (t *target) scaleDown(client scaleSetAPI, idle []*runner, excess int) {
	for _, r := range idle {
		if excess <= 0 {
			return
		}
		if t.retire(client, r, "removing idle runner") {
			excess--
		}
	}
}

// retire deregisters an idle runner from GitHub first, then removes its
// container. It reports false, leaving the runner alone, when GitHub refuses
// (a job was just assigned to it) or cannot be reached.
func (t *target) retire(client scaleSetAPI, r *runner, msg string) bool {
	if r.runnerID != 0 {
		if err := client.removeRunner(t.ctx, r.runnerID); err != nil {
			return false
		}
		t.m.forgetRunner(r.runnerID, nil)
	}
	t.log.Info(msg, "runner", r.name)
	t.m.pool.Remove(context.Background(), r.containerID)
	return true
}

// recordWait publishes how many runners are missing and why.
func (t *target) recordWait(missing, started int, reason string) {
	t.mu.Lock()
	// A target that got a runner and still waits goes behind the others
	// (reconcileAll serves the longest waiting first): targets take turns.
	if missing > 0 && (t.waiting == 0 || started > 0) {
		t.waitSince = time.Now()
	}
	switch {
	case missing == 0:
		reason = ""
	case reason == "":
		reason = pool.WaitSlot // capped by MaxRunners or still starting
	}
	changed := t.waiting != missing || started > 0 || t.waitReason != reason
	t.waiting, t.waitReason = missing, reason
	t.mu.Unlock()
	if changed {
		t.m.changed()
	}
}
