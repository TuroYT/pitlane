// Package scaler connects GitHub runner scale sets to the Docker slot pool.
// One listener runs per enabled target; when GitHub assigns jobs to a scale
// set, the matching number of ephemeral runners is started in free slots.
package scaler

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"

	"github.com/TuroYT/pitlane/internal/gh"
	"github.com/TuroYT/pitlane/internal/store"
)

// Manager runs one listener per target.
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
	creds   func() Credentials
	changed func()

	mu      sync.Mutex
	targets map[int64]*target
	adopted map[int64][]*runner
	lost    map[int64]map[string]lostJob // found at startup, handed to targets by Sync
	// ctx is set by Start; until then Sync does nothing (the UI is served
	// while slots start, and runners left by a previous run are not adopted
	// yet).
	ctx context.Context
}

// New creates a manager. changed is called whenever visible state changes.
func New(st *store.Store, p Pool, dataDir string, creds func() Credentials, changed func(), log *slog.Logger) *Manager {
	logDir := filepath.Join(dataDir, "logs")
	_ = os.MkdirAll(logDir, 0o700)
	return &Manager{store: st, pool: p, connect: newScaleSetAPI, log: log, logDir: logDir, creds: creds, changed: changed,
		predict: newPredictor(st.MemoryHistory),
		targets: map[int64]*target{}, adopted: map[int64][]*runner{}, lost: map[int64]map[string]lostJob{}}
}

// Start adopts runners left by a previous process and starts every enabled target.
func (m *Manager) Start(ctx context.Context) error {
	orphans, err := m.pool.RunnerContainers(ctx)
	if err != nil {
		return err
	}
	targets, err := m.store.Targets()
	if err != nil {
		return fmt.Errorf("load targets: %w", err)
	}
	enabled := map[int64]bool{}
	for _, t := range targets {
		enabled[t.ID] = t.Enabled
	}
	running := map[string]*runner{}
	for _, o := range orphans {
		// Assume busy since the container was created: the job timeout then
		// still applies after a restart.
		// The lease comes from the container's labels: a strict runner is
		// released (slot rebuilt, throwaway caches removed) as strict.
		r := &runner{name: o.Name, containerID: o.ID, lease: o.Lease(), busy: true, startedAt: time.Now(), busySince: o.Created}
		if o.Created.Year() < 2000 {
			r.busySince = time.Now()
		}
		r.runnerID = m.store.RunnerIDByName(o.Name)
		if o.Untrusted || !enabled[o.TargetID] {
			// No listener would ever watch it: stop it now. Its owner cannot
			// be trusted, so its slot is rebuilt.
			r.lease = untrustedLease(r.lease)
			m.log.Warn("runner container from a previous run has no known enabled target, stopping it",
				"container", o.Name, "id", o.ID, "target", o.TargetID, "slot", o.Slot, "running", o.Running)
			go m.stopUntracked(r, o.Running)
			continue
		}
		if !o.Running {
			go m.finishRunner(nil, r)
			continue
		}
		m.log.Info("adopting runner from previous run", "runner", o.Name, "slot", o.Slot, "strict", o.Strict)
		m.mu.Lock()
		m.adopted[o.TargetID] = append(m.adopted[o.TargetID], r)
		m.mu.Unlock()
		running[o.Name] = r
	}
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

func (m *Manager) loop(ctx context.Context) {
	tick := time.NewTicker(3 * time.Second)
	defer tick.Stop()
	clean := time.NewTicker(time.Hour)
	defer clean.Stop()
	timeouts := time.NewTicker(10 * time.Second) // in memory only: cheap
	defer timeouts.Stop()
	orphans := time.NewTicker(orphanInterval)
	defer orphans.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-m.pool.Released():
			m.reconcileAll()
		case <-tick.C:
			m.reconcileAll()
		case <-clean.C:
			m.housekeeping()
			go m.EnforceStrictAll(ctx)
		case <-timeouts.C:
			m.enforceTimeouts()
		case <-orphans.C:
			go m.cleanupOrphansAll()
		}
	}
}

func (m *Manager) reconcileAll() {
	m.mu.Lock()
	ts := make([]*target, 0, len(m.targets))
	for _, t := range m.targets {
		ts = append(ts, t)
	}
	m.mu.Unlock()
	// Oldest demand first so a busy target cannot starve the others.
	sort.Slice(ts, func(i, j int) bool { return ts[i].waitingSince().Before(ts[j].waitingSince()) })
	for _, t := range ts {
		t.reconcile()
	}
}

func (m *Manager) housekeeping() {
	retention := time.Duration(m.store.GetInt("history_days", 30)) * 24 * time.Hour
	if err := m.store.PruneJobs(retention); err != nil {
		m.log.Warn("prune job history", "error", err)
	}
	if err := m.store.PruneAudit(retention); err != nil {
		m.log.Warn("prune audit log", "error", err)
	}
	if err := m.store.PruneSessions(); err != nil {
		m.log.Warn("prune expired sessions", "error", err)
	}
	entries, _ := os.ReadDir(m.logDir)
	for _, e := range entries {
		if info, err := e.Info(); err == nil && time.Since(info.ModTime()) > retention {
			_ = os.Remove(filepath.Join(m.logDir, e.Name()))
		}
	}
}

// ApplyStrictRequirement records why a target must run strict (reason, ""
// when it need not, as GitHub just reported it) and switches it to strict if
// needed. It reports whether the stored target changed; the caller syncs.
func (m *Manager) ApplyStrictRequirement(t store.Target, reason string) bool {
	if reason == t.StrictRequired && (reason == "" || t.Strict) {
		return false
	}
	flipped, err := m.store.SetStrictRequirement(t.ID, reason)
	if err != nil {
		m.log.Error("record strict requirement", "target", t.FullName(), "reason", reason, "error", err)
		return false
	}
	if flipped {
		m.log.Warn("target switched to strict mode", "target", t.FullName(), "reason", reason)
	}
	return true
}

// EnforceStrictAll asks GitHub, for every target, whether it must run strict
// (at start, then hourly) and switches the ones that must. When GitHub
// cannot be asked, a target keeps its current mode.
func (m *Manager) EnforceStrictAll(ctx context.Context) {
	if m.StrictReason == nil {
		return
	}
	targets, err := m.store.Targets()
	if err != nil {
		m.log.Error("load targets", "error", err)
		return
	}
	changed := false
	for _, t := range targets {
		cctx, cancel := context.WithTimeout(ctx, 30*time.Second)
		reason, err := m.StrictReason(cctx, t)
		cancel()
		if err != nil {
			m.log.Warn("cannot check whether the target must run strict, its mode is left unchanged", "target", t.FullName(), "strict", t.Strict, "error", err)
			continue
		}
		if m.ApplyStrictRequirement(t, reason) {
			changed = true
		}
	}
	if changed {
		m.Sync()
	}
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
	want := map[int64]store.Target{}
	strict := false
	for _, t := range targets {
		if t.Enabled {
			want[t.ID] = t
			strict = strict || t.Strict
		}
	}
	m.pool.SetStrictDemand(strict)
	m.mu.Lock()
	for id, t := range m.targets {
		w, ok := want[id]
		if !ok || w.ScaleSetName != t.cfg().ScaleSetName || w.InstallationID != t.cfg().InstallationID {
			delete(m.targets, id)
			if ok {
				m.keepLostLocked(id, t)
			}
			go t.stop(!ok)
			continue
		}
		t.update(w)
	}
	creds := m.creds()
	for id, w := range want {
		if _, ok := m.targets[id]; ok || !creds.Valid() {
			continue
		}
		t := newTarget(m, w)
		for _, r := range m.adopted[id] {
			// Adopted after a restart: its job is unknown, reserve what the
			// target's jobs usually need.
			r.predicted, _ = m.predict.predict(m.ctx, jobKey{owner: w.Owner, repo: w.Repo})
			m.pool.SetPrediction(r.lease.Slot, r.predicted)
			t.runners[r.name] = r
			go t.watch(r)
		}
		delete(m.adopted, id)
		for jobID, l := range m.lost[id] {
			t.lost[jobID] = l
		}
		delete(m.lost, id)
		m.targets[id] = t
		go t.run(m.ctx)
	}
	m.mu.Unlock()
	m.changed()
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
