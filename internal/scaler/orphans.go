package scaler

import (
	"context"
	"maps"
	"slices"
	"time"

	"github.com/TuroYT/pitlane/internal/gh"
	"github.com/TuroYT/pitlane/internal/store"
)

// Orphan runners: a runner container that dies before its runner deregisters
// (OOM, host reboot, docker rm) leaves an offline runner on GitHub. The scale
// set API cannot list runners, so Pitlane records the ID of every runner it
// registers and checks those against GitHub's REST runner list, which tells
// whether a runner is online or busy.

const (
	orphanInterval = 10 * time.Minute
	// A runner younger than this may still be starting (registered, container
	// not created yet): never touched.
	orphanGrace = 15 * time.Minute
	// orphanMinGap skips a cleanup that follows the previous one too
	// closely (a listener restarting in a loop runs one at every start).
	orphanMinGap = time.Minute
	// orphanCleanupTimeout bounds the Docker and GitHub calls of a cleanup.
	orphanCleanupTimeout = 2 * time.Minute
)

type orphanAction int

const (
	orphanKeep   orphanAction = iota // leave it alone
	orphanVerify                     // not in the REST list: ask the scale set API
	orphanRemove                     // offline, idle, no container: deregister
)

// orphanDecision decides what to do with a recorded runner. It is
// conservative: anything alive, busy, online or recent is kept.
func orphanDecision(r store.RegisteredRunner, now time.Time, live map[string]bool, remote map[int64]gh.Runner) orphanAction {
	if live[r.Name] || now.Sub(time.Unix(r.CreatedAt, 0)) < orphanGrace {
		return orphanKeep
	}
	g, ok := remote[r.ID]
	if !ok {
		return orphanVerify
	}
	if g.Name != r.Name || g.Busy || g.Status != "offline" {
		return orphanKeep
	}
	return orphanRemove
}

// forgetRunner drops the record of a runner after a successful removal (or
// when GitHub no longer knows it).
func (m *Manager) forgetRunner(id int64, err error) {
	if id != 0 && (err == nil || isNotFound(err)) {
		_ = m.store.DeleteRunner(id)
	}
}

// cleanupOrphansAll runs the orphan cleanup of every target (every
// orphanInterval).
func (m *Manager) cleanupOrphansAll() {
	for _, t := range m.liveTargets() {
		t.cleanupOrphans()
	}
}

// cleanupOrphans removes this target's recorded runners that are offline on
// GitHub, not busy, and have no Pitlane container. Any doubt (API error,
// unknown status) keeps the runner.
func (t *target) cleanupOrphans() {
	t.orphanMu.Lock()
	defer t.orphanMu.Unlock()
	if time.Since(t.orphanAt) < orphanMinGap {
		return
	}
	t.orphanAt = time.Now()
	t.mu.Lock()
	client, cfg := t.client, t.t
	known := slices.Collect(maps.Keys(t.runners))
	t.mu.Unlock()
	if client == nil || t.m.ListRunners == nil {
		return
	}
	recs, err := t.m.store.RegisteredRunners(cfg.ID)
	if err != nil || len(recs) == 0 {
		return
	}
	ctx, cancel := context.WithTimeout(t.ctx, orphanCleanupTimeout)
	defer cancel()
	live, remote, ok := t.runnerViews(ctx, cfg, known)
	if !ok {
		return
	}
	now := time.Now()
	for _, r := range recs {
		t.resolveOrphan(ctx, client, r, orphanDecision(r, now, live, remote))
	}
}

// runnerViews returns the runners that have a container (live, including
// known, the target's own) and GitHub's runners by ID. It reports false,
// after logging why, when either cannot be listed.
func (t *target) runnerViews(ctx context.Context, cfg store.Target, known []string) (map[string]bool, map[int64]gh.Runner, bool) {
	live, err := t.m.pool.RunnerNames(ctx)
	if err != nil {
		t.log.Warn("orphan runners: cannot list containers", "error", err)
		return nil, nil, false
	}
	for _, name := range known {
		live[name] = true
	}
	list, err := t.m.ListRunners(ctx, cfg)
	if err != nil {
		t.log.Warn("orphan runners: cannot list GitHub runners", "error", err)
		return nil, nil, false
	}
	remote := make(map[int64]gh.Runner, len(list))
	for _, g := range list {
		remote[g.ID] = g
	}
	return live, remote, true
}

// resolveOrphan applies an orphanDecision to a recorded runner.
func (t *target) resolveOrphan(ctx context.Context, client scaleSetAPI, r store.RegisteredRunner, action orphanAction) {
	switch action {
	case orphanVerify:
		if isNotFound(client.getRunner(ctx, r.ID)) {
			_ = t.m.store.DeleteRunner(r.ID)
			t.forgetLostRunner(r.Name)
		}
	case orphanRemove:
		if err := client.removeRunner(ctx, r.ID); err != nil && !isNotFound(err) {
			t.log.Warn("remove orphan runner", "runner", r.Name, "id", r.ID, "error", err)
			return
		}
		t.log.Info("removed orphan runner from GitHub", "runner", r.Name, "id", r.ID)
		_ = t.m.store.DeleteRunner(r.ID)
		t.forgetLostRunner(r.Name) // GitHub reports it idle: its job is over
	}
}
