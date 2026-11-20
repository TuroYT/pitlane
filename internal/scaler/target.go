package scaler

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"github.com/TuroYT/pitlane/internal/store"
)

type target struct {
	m      *Manager
	log    *slog.Logger
	ctx    context.Context
	cancel context.CancelFunc

	mu          sync.Mutex
	t           store.Target
	client      scaleSetAPI
	session     jobSession
	scaleSetID  int
	status      string
	err         string
	desired     int
	waiting     int
	waitReason  string
	waitSince   time.Time
	pending     map[string]pendingJob // by GitHub job ID
	runners     map[string]*runner
	lost        map[string]lostJob // jobs bound to dead runners, by job ID
	starting    int                // runners being prepared/started in the background
	reconcileMu sync.Mutex
	orphanMu    sync.Mutex
	orphanAt    time.Time // last orphan cleanup
}

func newTarget(m *Manager, t store.Target) *target {
	ctx, cancel := context.WithCancel(context.Background())
	return &target{m: m, t: t, ctx: ctx, cancel: cancel, status: "starting", runners: map[string]*runner{}, lost: map[string]lostJob{}, pending: map[string]pendingJob{},
		log: m.log.With("target", t.FullName())}
}

func (t *target) cfg() store.Target {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.t
}

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

// stop cancels the listener. When remove is set the scale set is deleted on
// GitHub once its busy runners are done.
func (t *target) stop(remove bool) {
	t.cancel()
	// Removed outside the lock: each removal is a Docker call, and the
	// runners' watch goroutines need the lock to finish.
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
	if !remove {
		return
	}
	deadline := time.Now().Add(6 * time.Hour)
	for time.Now().Before(deadline) {
		t.mu.Lock()
		n := len(t.runners)
		client, id := t.client, t.scaleSetID
		t.mu.Unlock()
		if n == 0 {
			if client != nil && id != 0 {
				if err := client.deleteScaleSet(context.Background(), id); err != nil {
					t.log.Warn("delete scale set", "error", err)
				} else {
					t.log.Info("scale set deleted", "id", id)
				}
			}
			return
		}
		time.Sleep(5 * time.Second)
	}
}
