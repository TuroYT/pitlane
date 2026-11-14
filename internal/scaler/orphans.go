package scaler

import (
	"context"
	"errors"
	"time"

	"github.com/actions/scaleset"

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
)

type orphanAction int

const (
	orphanKeep   orphanAction = iota // leave it alone
	orphanForget                     // already gone from GitHub: drop the record
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

func isNotFound(err error) bool {
	return errors.Is(err, scaleset.NotFoundError) || errors.Is(err, scaleset.RunnerNotFoundError)
}

// forgetRunner drops the record of a runner after a successful removal (or
// when GitHub no longer knows it).
func (m *Manager) forgetRunner(id int64, err error) {
	if id != 0 && (err == nil || isNotFound(err)) {
		_ = m.store.DeleteRunner(id)
	}
}

func (m *Manager) cleanupOrphansAll() {
	m.mu.Lock()
	ts := make([]*target, 0, len(m.targets))
	for _, t := range m.targets {
		ts = append(ts, t)
	}
	m.mu.Unlock()
	for _, t := range ts {
		t.cleanupOrphans()
	}
}

// cleanupOrphans removes this target's recorded runners that are offline on
// GitHub, not busy, and have no Pitlane container. Any doubt (API error,
// unknown status) keeps the runner.
func (t *target) cleanupOrphans() {
	t.orphanMu.Lock()
	defer t.orphanMu.Unlock()
	if time.Since(t.orphanAt) < time.Minute { // listener restarts in a loop
		return
	}
	t.orphanAt = time.Now()
	t.mu.Lock()
	client, cfg := t.client, t.t
	known := map[string]bool{}
	for name := range t.runners {
		known[name] = true
	}
	t.mu.Unlock()
	if client == nil || t.m.ListRunners == nil {
		return
	}
	recs, err := t.m.store.RegisteredRunners(cfg.ID)
	if err != nil || len(recs) == 0 {
		return
	}
	ctx, cancel := context.WithTimeout(t.ctx, 2*time.Minute)
	defer cancel()
	live, err := t.m.pool.RunnerNames(ctx)
	if err != nil {
		t.log.Warn("orphan runners: cannot list containers", "error", err)
		return
	}
	for name := range known {
		live[name] = true
	}
	list, err := t.m.ListRunners(ctx, cfg)
	if err != nil {
		t.log.Warn("orphan runners: cannot list GitHub runners", "error", err)
		return
	}
	remote := map[int64]gh.Runner{}
	for _, g := range list {
		remote[g.ID] = g
	}
	now := time.Now()
	for _, r := range recs {
		switch orphanDecision(r, now, live, remote) {
		case orphanVerify:
			_, err := client.GetRunner(ctx, int(r.ID))
			if isNotFound(err) {
				_ = t.m.store.DeleteRunner(r.ID)
			}
		case orphanRemove:
			err := client.RemoveRunner(ctx, r.ID)
			if err != nil && !isNotFound(err) {
				t.log.Warn("remove orphan runner", "runner", r.Name, "id", r.ID, "error", err)
				continue
			}
			t.log.Info("removed orphan runner from GitHub", "runner", r.Name, "id", r.ID)
			_ = t.m.store.DeleteRunner(r.ID)
		}
	}
}
