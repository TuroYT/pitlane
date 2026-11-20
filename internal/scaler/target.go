package scaler

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"github.com/TuroYT/pitlane/internal/store"
)

// target is the listener of one enabled target and the runners it started.
// It lives until Sync or Restart stops it; a changed target gets a new one.
type target struct {
	m      *Manager
	log    *slog.Logger
	ctx    context.Context // canceled when the listener stops
	cancel context.CancelFunc

	// mu guards the fields below it up to reconcileMu, and the fields of
	// the runners in runners.
	mu sync.Mutex
	t  store.Target
	// client, session and scaleSetID are set by listen() once connected
	// (client nil before).
	client     scaleSetAPI
	session    jobSession
	scaleSetID int
	status     string // starting, listening or error, shown in the UI
	err        string
	// desired is GitHub's count of jobs assigned to the scale set.
	desired int
	// waiting is how many runners reconcile could not start, waitReason
	// why, and waitSince when the target last got its turn.
	waiting    int
	waitReason string
	waitSince  time.Time
	pending    map[string]pendingJob // by GitHub job ID
	runners    map[string]*runner    // by runner name
	lost       map[string]lostJob    // jobs bound to dead runners, by job ID
	starting   int                   // runners being prepared/started in the background

	// reconcileMu serializes reconcile: it is called from the manager's
	// loop, the listener and update.
	reconcileMu sync.Mutex
	// orphanMu serializes cleanupOrphans and guards orphanAt, the time of
	// its last run.
	orphanMu sync.Mutex
	orphanAt time.Time
}

func newTarget(m *Manager, t store.Target) *target {
	ctx, cancel := context.WithCancel(context.Background())
	return &target{m: m, t: t, ctx: ctx, cancel: cancel, status: "starting", runners: map[string]*runner{}, lost: map[string]lostJob{}, pending: map[string]pendingJob{},
		log: m.log.With("target", t.FullName())}
}

// cfg returns the target's stored configuration.
func (t *target) cfg() store.Target {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.t
}

// update applies a changed configuration the running listener can follow.
func (t *target) update(n store.Target) {
	t.mu.Lock()
	t.t = n
	session := t.session
	t.mu.Unlock()
	if session != nil {
		session.setMaxRunners(n.MaxRunners)
	}
	go t.reconcile()
}

// waitingSince orders targets for admission: the one waiting longest goes
// first, and a target that waits for nothing goes last.
func (t *target) waitingSince() time.Time {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.waiting == 0 {
		return time.Now()
	}
	return t.waitSince
}

func (t *target) setStatus(status, errMsg string) {
	t.mu.Lock()
	t.status, t.err = status, errMsg
	t.mu.Unlock()
	t.m.changed()
}

// Removing a deleted target's scale set waits for its busy runners, polling
// every drainPoll, for at most drainTimeout.
const (
	drainPoll    = 5 * time.Second
	drainTimeout = 6 * time.Hour
)

// stop cancels the listener and removes the idle runners. When remove is
// set the scale set is deleted on GitHub once its busy runners are done.
func (t *target) stop(remove bool) {
	t.cancel()
	t.removeIdleRunners()
	if remove {
		t.deleteScaleSetWhenDrained()
	}
}

// removeIdleRunners removes the containers of the runners without a job,
// outside the lock: each removal is a Docker call, and the runners' watch
// goroutines need the lock to finish.
func (t *target) removeIdleRunners() {
	t.mu.Lock()
	var idle []string
	for _, r := range t.runners {
		if !r.busy {
			idle = append(idle, r.containerID)
		}
	}
	t.mu.Unlock()
	for _, id := range idle {
		t.m.pool.Remove(context.Background(), id)
	}
}

// deleteScaleSetWhenDrained deletes the scale set on GitHub once the target
// has no runner left, giving up after drainTimeout.
func (t *target) deleteScaleSetWhenDrained() {
	deadline := time.Now().Add(drainTimeout)
	for t.runnerCount() > 0 {
		if time.Now().After(deadline) {
			return
		}
		time.Sleep(drainPoll)
	}
	t.mu.Lock()
	client, id := t.client, t.scaleSetID
	t.mu.Unlock()
	if client == nil || id == 0 {
		return
	}
	if err := client.deleteScaleSet(context.Background(), id); err != nil {
		t.log.Warn("delete scale set", "error", err)
		return
	}
	t.log.Info("scale set deleted", "id", id)
}

func (t *target) runnerCount() int {
	t.mu.Lock()
	defer t.mu.Unlock()
	return len(t.runners)
}
