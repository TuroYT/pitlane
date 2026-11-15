package scaler

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/TuroYT/pitlane/internal/store"
)

// housekeeping deletes what is older than history_days: jobs, audit
// entries, archived runner logs. Everything newer stays, and so does a
// session that has not expired.
func TestHousekeeping(t *testing.T) {
	h := newHarness(t, store.Target{}, 1)
	st, m := h.st, h.m
	if err := st.Set("history_days", "7"); err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	old, recent := now.Add(-8*24*time.Hour), now.Add(-6*24*time.Hour)

	for id, at := range map[string]time.Time{"old": old, "recent": recent} {
		if err := st.UpsertJob(store.Job{TargetID: 1, JobID: id, Slot: -1, QueuedAt: at.Unix()}); err != nil {
			t.Fatal(err)
		}
		if err := st.AddAudit(store.AuditEntry{At: at.Unix(), Actor: "alice", Action: "target.update", Subject: id}); err != nil {
			t.Fatal(err)
		}
		f := m.LogFile("pitlane-" + id)
		if err := os.WriteFile(f, []byte("log"), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Chtimes(f, at, at); err != nil {
			t.Fatal(err)
		}
	}
	if err := st.CreateSession("expired", "alice", -time.Minute); err != nil {
		t.Fatal(err)
	}
	if err := st.CreateSession("live", "alice", time.Hour); err != nil {
		t.Fatal(err)
	}

	m.housekeeping()

	jobs, err := st.Jobs(context.Background(), 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(jobs) != 1 || jobs[0].JobID != "recent" {
		t.Errorf("jobs kept: %+v", jobs)
	}
	audit, _, err := st.AuditPage(context.Background(), 10, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(audit) != 1 || audit[0].Subject != "recent" {
		t.Errorf("audit kept: %+v", audit)
	}
	logs, err := filepath.Glob(filepath.Join(m.logDir, "*"))
	if err != nil {
		t.Fatal(err)
	}
	if len(logs) != 1 || filepath.Base(logs[0]) != "pitlane-recent.log" {
		t.Errorf("logs kept: %v", logs)
	}
	if _, ok := st.Session("live"); !ok {
		t.Error("live session pruned")
	}
	if _, ok := st.Session("expired"); ok {
		t.Error("expired session still valid")
	}
}
