package scaler

import (
	"os"
	"path/filepath"
	"time"
)

// defaultHistoryDays is the retention of the job history, audit log and
// runner logs when the history_days setting is unset.
const defaultHistoryDays = 30

// housekeeping prunes what is older than the retention (hourly). A failure
// is retried at the next run.
func (m *Manager) housekeeping() {
	retention := time.Duration(m.store.GetInt("history_days", defaultHistoryDays)) * 24 * time.Hour
	if err := m.store.PruneJobs(retention); err != nil {
		m.log.Warn("prune job history", "error", err)
	}
	if err := m.store.PruneAudit(retention); err != nil {
		m.log.Warn("prune audit log", "error", err)
	}
	if err := m.store.PruneSessions(); err != nil {
		m.log.Warn("prune expired sessions", "error", err)
	}
	m.pruneLogs(retention)
}

// pruneLogs removes the runner logs older than retention.
func (m *Manager) pruneLogs(retention time.Duration) {
	entries, _ := os.ReadDir(m.logDir)
	for _, e := range entries {
		if info, err := e.Info(); err == nil && time.Since(info.ModTime()) > retention {
			_ = os.Remove(filepath.Join(m.logDir, e.Name()))
		}
	}
}
