package server

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/TuroYT/pitlane/internal/pool"
	"github.com/TuroYT/pitlane/internal/scaler"
	"github.com/TuroYT/pitlane/internal/store"
)

// newErrServer is a server whose log is kept in logs.
func newErrServer(t *testing.T) (*Server, *store.Store, *bytes.Buffer) {
	t.Helper()
	st, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	logs := &bytes.Buffer{}
	log := slog.New(slog.NewTextHandler(logs, nil))
	s := New(Config{DataDir: t.TempDir(), InsecureNoAuth: true}, st, &pool.Pool{}, log, nil)
	s.SetManager(scaler.New(st, nil, t.TempDir(), func() scaler.Credentials { return scaler.Credentials{} }, func() {}, log))
	return s, st, logs
}

// checkInternalErr checks that a failure response carries the status, the
// code and a request ID but none of the error text, which the log has
// under the same request ID.
func checkInternalErr(t *testing.T, w *httptest.ResponseRecorder, logs *bytes.Buffer, status int, code string, leaks ...string) {
	t.Helper()
	if w.Code != status {
		t.Fatalf("status %d, want %d (%s)", w.Code, status, w.Body)
	}
	var body apiError
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("body %q: %v", w.Body, err)
	}
	if body.Code != code || body.RequestID == "" || w.Header().Get("X-Request-Id") != body.RequestID {
		t.Fatalf("code %q request %q header %q, want code %q", body.Code, body.RequestID, w.Header().Get("X-Request-Id"), code)
	}
	if !strings.Contains(body.Error, code) || !strings.Contains(body.Error, body.RequestID) {
		t.Errorf("message %q does not give the code and the request ID", body.Error)
	}
	for _, l := range leaks {
		if strings.Contains(strings.ToLower(w.Body.String()), strings.ToLower(l)) {
			t.Errorf("response leaks %q: %s", l, w.Body)
		}
	}
	if !strings.Contains(logs.String(), "request_id="+body.RequestID) || !strings.Contains(logs.String(), "code="+code) {
		t.Errorf("log does not record the failure under its request ID:\n%s", logs)
	}
}

func TestStoreErrorsStayInTheLog(t *testing.T) {
	for _, tc := range []struct {
		name, path, code string
		h                func(*Server) http.HandlerFunc
	}{
		{"jobs", "/api/jobs", "jobs_list", func(s *Server) http.HandlerFunc { return s.handleJobs }},
		{"job repos", "/api/jobs/repos", "job_repos", func(s *Server) http.HandlerFunc { return s.handleJobRepos }},
		{"job memory", "/api/jobs/memory", "job_memory", func(s *Server) http.HandlerFunc { return s.handleJobMemory }},
		{"audit", "/api/audit", "audit_list", func(s *Server) http.HandlerFunc { return s.handleAudit }},
		{"users", "/api/users", "users_list", func(s *Server) http.HandlerFunc { return s.handleUsers }},
		{"compare", "/api/compare", "targets_list", func(s *Server) http.HandlerFunc { return s.handleCompare }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, st, logs := newErrServer(t)
			_ = st.Close() // every query now fails
			w := httptest.NewRecorder()
			tc.h(s)(w, httptest.NewRequest("GET", tc.path, nil))
			checkInternalErr(t, w, logs, http.StatusInternalServerError, tc.code, "sql", "database is closed")
			if !strings.Contains(logs.String(), "database is closed") {
				t.Errorf("log lost the error:\n%s", logs)
			}
		})
	}
}

func TestJobLogGitHubErrorStaysInTheLog(t *testing.T) {
	s, st, logs := newErrServer(t)
	tg, err := st.CreateTarget(store.Target{Kind: "repo", Owner: "acme", Repo: "app", InstallationID: 7, Enabled: true})
	if err != nil {
		t.Fatal(err)
	}
	if err := st.UpsertJob(store.Job{TargetID: tg.ID, JobID: "j1", RunID: 42, Owner: "acme", Repo: "app", RunnerName: "pitlane-1"}); err != nil {
		t.Fatal(err)
	}
	// No App key stored: getting an installation token fails.
	r := httptest.NewRequest("GET", "/api/runners/pitlane-1/job-log", nil)
	r.SetPathValue("name", "pitlane-1")
	w := httptest.NewRecorder()
	s.handleJobLog(w, r)
	checkInternalErr(t, w, logs, http.StatusBadGateway, "github_token", "pem", "key")
}

func TestRequestIDFromProxy(t *testing.T) {
	r := httptest.NewRequest("GET", "/", nil)
	r.Header.Set("X-Request-Id", "abc-123_x.y")
	if got := requestID(r); got != "abc-123_x.y" {
		t.Errorf("proxy request ID not kept: %q", got)
	}
	for _, bad := range []string{"a b", "x\ny", strings.Repeat("a", 65), "<script>"} {
		r.Header.Set("X-Request-Id", bad)
		if got := requestID(r); got == bad || len(got) != 16 {
			t.Errorf("request ID %q: got %q, want a new one", bad, got)
		}
	}
}

func TestStoreRefusalsStayReadable(t *testing.T) {
	s, _, _ := newErrServer(t)
	r := httptest.NewRequest("DELETE", "/api/users/nobody", nil)
	r.SetPathValue("login", "nobody")
	w := httptest.NewRecorder()
	s.handleDeleteUser(w, r)
	if w.Code != http.StatusNotFound || !strings.Contains(w.Body.String(), store.ErrNotFound.Error()) {
		t.Fatalf("got %d %s", w.Code, w.Body)
	}
}
