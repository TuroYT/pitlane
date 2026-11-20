package server

import (
	"bytes"
	"log/slog"
	"net/http"
	"strings"
	"testing"
)

// The job history feeds several metrics. A history that cannot be read is
// not reported as "no job": the scrape fails, and the cause goes to the log.
func TestMetricsFailWhenTheJobHistoryIsUnreadable(t *testing.T) {
	st, dir := storeAt(t)
	logs := &bytes.Buffer{}
	s := New(Config{DataDir: dir, InsecureNoAuth: true}, st, nil, slog.New(slog.NewTextHandler(logs, nil)), nil)
	s.pool = newFakePool()
	h := s.Handler()
	if w := get(h, "/metrics", ""); w.Code != http.StatusOK {
		t.Fatalf("healthy store: %d %s", w.Code, w.Body)
	}
	dropTable(t, dir, "jobs")
	w := get(h, "/metrics", "")
	checkInternalErr(t, w, logs, http.StatusInternalServerError, "metrics_jobs", "no such table", "sql")
	if !strings.Contains(logs.String(), "no such table") {
		t.Errorf("log lost the error:\n%s", logs)
	}
}
