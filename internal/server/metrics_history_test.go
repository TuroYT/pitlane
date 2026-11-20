package server

import (
	"net/http"
	"strings"
	"testing"
)

// The job history feeds several metrics. A history that cannot be read is
// not reported as "no job": the scrape fails, and the cause goes to the log.
func TestMetricsFailWhenTheJobHistoryIsUnreadable(t *testing.T) {
	t.Parallel()
	s, _, dir, logs := serverAt(t)
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
