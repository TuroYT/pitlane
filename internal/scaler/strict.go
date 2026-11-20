package scaler

import (
	"context"
	"time"

	"github.com/TuroYT/pitlane/internal/store"
)

// strictCheckTimeout bounds the GitHub calls that tell whether one target
// must run strict.
const strictCheckTimeout = 30 * time.Second

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
		cctx, cancel := context.WithTimeout(ctx, strictCheckTimeout)
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
