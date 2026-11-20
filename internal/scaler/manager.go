// Package scaler connects GitHub runner scale sets to the Docker slot pool.
// One listener runs per enabled target; when GitHub assigns jobs to a scale
// set, the matching number of ephemeral runners is started in free slots.
package scaler

import (
	"context"
	"fmt"
	"log/slog"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"time"

	"github.com/TuroYT/pitlane/internal/gh"
	"github.com/TuroYT/pitlane/internal/store"
)

// Periods of the manager's loop.
const (
	// reconcileInterval also catches demand the pool's release signal and
	// the listener did not report.
	reconcileInterval    = 3 * time.Second
	housekeepingInterval = time.Hour
	// timeoutCheckInterval is short: the check reads memory only.
	timeoutCheckInterval = 10 * time.Second
)

// Manager runs one listener per target.
//
// Lock order: m.mu before t.mu. Neither is held across GitHub or Docker
// calls.
type Manager struct {
	Version string
	// ListRunners lists a target's self-hosted runners through the REST API
	// (used to find orphan runners). Nil disables orphan cleanup.
	ListRunners func(ctx context.Context, t store.Target) ([]gh.Runner, error)
	// StrictReason asks GitHub why a target must run strict ("" when it
	// need not). Nil disables the periodic check.
	StrictReason func(ctx context.Context, t store.Target) (string, error)
	// JobQueuedAt asks the REST API when a job was queued, for jobs whose
	// messages carry no queue time. Nil disables the lookup.
	JobQueuedAt func(ctx context.Context, t store.Target, owner, repo string, runID int64, runner string) (time.Time, error)

	store *store.Store
	pool  Pool
	// connect opens the scale set API of a target (newScaleSetAPI; tests
	// set a fake).
	connect func(creds Credentials, t store.Target, version string) (scaleSetAPI, error)
	log     *slog.Logger
	predict *predictor
	logDir  string
	// logLimit caps an archived runner log (0: maxArchivedLog).
	logLimit int64
	creds    func() Credentials
	changed  func()
	// usageRetry and queueLookupRetry are the waits of recordUsage and
	// fetchQueueTime before they ask again (shortened by tests).
	usageRetry       time.Duration
	queueLookupRetry time.Duration
	// now, after and tick are the time sources (nil: the real clock; see
	// clock.go). Tests replace them to run without waiting.
	now   func() time.Time
	after func(d time.Duration) <-chan time.Time
	tick  func(d time.Duration) (<-chan time.Time, func())

	mu sync.Mutex
	// targets are the running listeners, by target ID. Guarded by mu.
	targets map[int64]*target
	// adopted and lost are found at startup and handed to each target's
	// listener by Sync. Guarded by mu.
	adopted map[int64][]*runner
	lost    map[int64]map[string]lostJob
	// ctx is set by Start; until then Sync does nothing (the UI is served
	// while slots start, and runners left by a previous run are not adopted
	// yet). Guarded by mu.
	ctx context.Context
}

// New creates a manager. changed is called whenever visible state changes.
func New(st *store.Store, p Pool, dataDir string, creds func() Credentials, changed func(), log *slog.Logger) *Manager {
	logDir := filepath.Join(dataDir, "logs")
	_ = os.MkdirAll(logDir, 0o700)
	return &Manager{store: st, pool: p, connect: newScaleSetAPI, log: log, logDir: logDir, creds: creds, changed: changed,
		predict:    newPredictor(st.MemoryHistory),
		usageRetry: defaultUsageRetry, queueLookupRetry: defaultQueueLookupRetry,
		targets: map[int64]*target{}, adopted: map[int64][]*runner{}, lost: map[int64]map[string]lostJob{}}
}

// Start adopts runners left by a previous process and starts every enabled target.
func (m *Manager) Start(ctx context.Context) error {
	containers, err := m.pool.RunnerContainers(ctx)
	if err != nil {
		return fmt.Errorf("list runner containers: %w", err)
	}
	targets, err := m.store.Targets()
	if err != nil {
		return fmt.Errorf("load targets: %w", err)
	}
	enabled, _ := enabledTargets(targets)
	running := m.adoptContainers(containers, enabled)
	m.findLostJobs(ctx, running)
	// Only now may Sync start listeners: adopted runners and lost jobs are
	// known, so they are handed to their target.
	m.mu.Lock()
	m.ctx = ctx
	m.mu.Unlock()
	m.Sync()
	go m.EnforceStrictAll(ctx)
	go m.loop(ctx)
	return nil
}

// enabledTargets returns the enabled targets by ID, and whether one of them
// is strict.
func enabledTargets(targets []store.Target) (map[int64]store.Target, bool) {
	enabled := map[int64]store.Target{}
	strict := false
	for _, t := range targets {
		if t.Enabled {
			enabled[t.ID] = t
			strict = strict || t.Strict
		}
	}
	return enabled, strict
}

// loop runs the manager's periodic work until ctx ends.
func (m *Manager) loop(ctx context.Context) {
	tick, stop := m.ticker(reconcileInterval)
	defer stop()
	clean, stop := m.ticker(housekeepingInterval)
	defer stop()
	timeouts, stop := m.ticker(timeoutCheckInterval)
	defer stop()
	orphans, stop := m.ticker(orphanInterval)
	defer stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-m.pool.Released():
			m.reconcileAll()
		case <-tick:
			m.reconcileAll()
		case <-clean:
			m.housekeeping()
			go m.EnforceStrictAll(ctx)
		case <-timeouts:
			m.enforceTimeouts()
		case <-orphans:
			go m.cleanupOrphansAll()
		}
	}
}

// liveTargets returns the running targets, to work on them without holding
// m.mu.
func (m *Manager) liveTargets() []*target {
	m.mu.Lock()
	defer m.mu.Unlock()
	return slices.Collect(maps.Values(m.targets))
}

// reconcileAll reconciles every target, the one waiting longest first.
func (m *Manager) reconcileAll() {
	for _, t := range byWaitTurn(m.liveTargets()) {
		t.reconcile()
	}
}

// byWaitTurn orders targets oldest demand first, so a busy target cannot
// starve the others; targets that wait for nothing go last. Each target's
// state is read once, so the order is consistent while targets change.
func byWaitTurn(ts []*target) []*target {
	type turn struct {
		t       *target
		waiting bool
		since   time.Time
	}
	turns := make([]turn, len(ts))
	for i, t := range ts {
		waiting, since := t.waitTurn()
		turns[i] = turn{t: t, waiting: waiting, since: since}
	}
	slices.SortStableFunc(turns, func(a, b turn) int {
		if a.waiting != b.waiting {
			if a.waiting {
				return -1
			}
			return 1
		}
		if !a.waiting {
			return 0
		}
		return a.since.Compare(b.since)
	})
	out := make([]*target, len(turns))
	for i, tn := range turns {
		out[i] = tn.t
	}
	return out
}

// Sync starts, restarts or stops listeners so they match the stored targets.
// Before Start it only notifies the UI: Start syncs once it is ready.
func (m *Manager) Sync() {
	m.mu.Lock()
	started := m.ctx != nil
	m.mu.Unlock()
	if !started {
		m.changed()
		return
	}
	targets, err := m.store.Targets()
	if err != nil {
		m.log.Error("load targets", "error", err)
		return
	}
	want, strict := enabledTargets(targets)
	m.pool.SetStrictDemand(strict)
	m.mu.Lock()
	m.syncRunningLocked(want)
	m.startMissingLocked(want)
	m.mu.Unlock()
	m.changed()
}

// syncRunningLocked updates the running listeners to their stored target,
// and stops those whose target was disabled, deleted or moved to another
// scale set or installation. Callers hold m.mu.
func (m *Manager) syncRunningLocked(want map[int64]store.Target) {
	for id, t := range m.targets {
		w, ok := want[id]
		if ok && !needsNewListener(t.cfg(), w) {
			t.update(w)
			continue
		}
		delete(m.targets, id)
		if ok {
			m.keepLostLocked(id, t)
		}
		go t.stop(!ok)
	}
}

// needsNewListener reports whether a target changed in a way its running
// listener cannot follow.
func needsNewListener(cur, next store.Target) bool {
	return next.ScaleSetName != cur.ScaleSetName || next.InstallationID != cur.InstallationID
}

// startMissingLocked starts a listener for every wanted target that has
// none, when there are credentials to start it with. Callers hold m.mu.
func (m *Manager) startMissingLocked(want map[int64]store.Target) {
	if !m.creds().Valid() {
		return
	}
	for id, w := range want {
		if _, ok := m.targets[id]; ok {
			continue
		}
		t := newTarget(m, w)
		m.handOverLocked(t)
		m.targets[id] = t
		go t.run(m.ctx)
	}
}

// Restart restarts every listener (e.g. after credentials change).
func (m *Manager) Restart() {
	m.mu.Lock()
	for id, t := range m.targets {
		delete(m.targets, id)
		m.keepLostLocked(id, t)
		go t.stop(false)
	}
	m.mu.Unlock()
	m.Sync()
}

// Shutdown stops listeners; busy runners keep running and are adopted on restart.
func (m *Manager) Shutdown() {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, t := range m.targets {
		t.cancel()
	}
}

// RunnerContainer returns the container of a live runner.
func (m *Manager) RunnerContainer(name string) (string, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, t := range m.targets {
		t.mu.Lock()
		r, ok := t.runners[name]
		t.mu.Unlock()
		if ok {
			return r.containerID, true
		}
	}
	return "", false
}
