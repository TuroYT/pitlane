package scaler

import (
	"context"
	"time"

	"github.com/TuroYT/pitlane/internal/pool"
)

// staleForStrict reports whether an idle runner was started without strict
// isolation for a target that is strict now: reconcile replaces it.
func staleForStrict(r *runner, strict bool) bool {
	return strict && !r.lease.Strict && !r.busy && !r.exited
}

// reconcile starts runners until the target has what GitHub asked for (or
// no slot is free), and removes idle runners that are no longer needed.
func (t *target) reconcile() {
	if t.ctx.Err() != nil {
		return
	}
	t.reconcileMu.Lock()
	defer t.reconcileMu.Unlock()

	t.mu.Lock()
	client := t.client // replaced by listen(): read under the lock only
	if client == nil {
		t.mu.Unlock()
		return
	}
	cfg := t.t
	now := time.Now()
	want := min(cfg.MaxRunners, cfg.MinRunners+effectiveDesired(t.desired, pruneLostJobs(t.lost, now)))
	have := t.starting
	waiting := t.starting // runners not running a job yet
	var idle, stale []*runner
	for _, r := range t.runners {
		if r.exited {
			continue
		}
		if staleForStrict(r, cfg.Strict) {
			stale = append(stale, r) // not counted: replaced right away
			continue
		}
		have++
		if !r.busy {
			waiting++
			if time.Since(r.startedAt) > 30*time.Second {
				idle = append(idle, r)
			}
		}
	}
	preds := pendingPredictions(t.pending, now)
	since := now
	if t.waiting > 0 {
		since = t.waitSince
	}
	t.mu.Unlock()

	// Runners are prepared and started in parallel, in the background: the
	// listener is never blocked and a slow start does not delay the others.
	// Runners not running a job yet cover the heaviest pending jobs; each
	// new one reserves the memory of the next (GitHub picks which runner
	// runs which job, so this is the safe bet), or of a typical job of the
	// target for a warm runner.
	started := 0
	reason := ""
	for i := 0; i < want-have; i++ {
		predicted := int64(0)
		if k := waiting + i; k < len(preds) {
			predicted = preds[k]
		} else {
			predicted, _ = t.m.predict.predict(t.ctx, jobKey{owner: cfg.Owner, repo: cfg.Repo})
		}
		lease, why := t.m.pool.Admit(t.ctx, pool.Request{Owner: cfg.Owner, TargetID: cfg.ID, Strict: cfg.Strict, Predicted: predicted, Since: since})
		if why != "" {
			reason = why
			break
		}
		started++
		t.mu.Lock()
		t.starting++
		t.mu.Unlock()
		go t.launch(lease)
	}
	missing := max(0, want-have-started)

	// The target became strict: its idle runners started without strict
	// isolation go (retried at every reconcile until none is left).
	for _, r := range stale {
		if r.runnerID != 0 {
			if err := client.removeRunner(t.ctx, r.runnerID); err != nil {
				continue // a job was just assigned to it, or GitHub is unreachable
			}
			t.m.forgetRunner(r.runnerID, nil)
		}
		t.log.Info("replacing idle runner started without strict isolation", "runner", r.name)
		t.m.pool.Remove(context.Background(), r.containerID)
	}

	// Scale down: idle runners beyond what is needed are deregistered first
	// (GitHub refuses if a job was just assigned) and only then killed.
	excess := have + started - want
	for _, r := range idle {
		if excess <= 0 {
			break
		}
		if r.runnerID != 0 {
			if err := client.removeRunner(t.ctx, r.runnerID); err != nil {
				continue
			}
			t.m.forgetRunner(r.runnerID, nil)
		}
		t.log.Info("removing idle runner", "runner", r.name)
		t.m.pool.Remove(context.Background(), r.containerID)
		excess--
	}

	t.mu.Lock()
	// A target that got a runner and still waits goes behind the others
	// (reconcileAll serves the longest waiting first): targets take turns.
	if missing > 0 && (t.waiting == 0 || started > 0) {
		t.waitSince = time.Now()
	}
	if missing == 0 {
		reason = ""
	} else if reason == "" {
		reason = pool.WaitSlot // capped by MaxRunners or still starting
	}
	changed := t.waiting != missing || started > 0 || t.waitReason != reason
	t.waiting, t.waitReason = missing, reason
	t.mu.Unlock()
	if changed {
		t.m.changed()
	}
}
