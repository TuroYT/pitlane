package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/TuroYT/pitlane/internal/pool"
	"github.com/TuroYT/pitlane/internal/store"
)

func TestJobsEndpoint(t *testing.T) {
	t.Parallel()
	_, st, _, h := apiServer(t)
	for i, j := range []store.Job{
		{JobID: "a", RunID: 1, Owner: "acme", Repo: "api", WorkflowRef: "refs/heads/main", Result: "succeeded", RunnerName: "pitlane-1"},
		{JobID: "b", RunID: 2, Owner: "acme", Repo: "api", WorkflowRef: "refs/heads/dev", Result: "failed", RunnerName: "pitlane-2"},
		{JobID: "c", RunID: 3, Owner: "acme", Repo: "web", WorkflowRef: "refs/heads/main", Result: "succeeded", RunnerName: "pitlane-3"},
	} {
		j.TargetID, j.Slot, j.QueuedAt, j.FinishedAt = 1, -1, int64(100+i), int64(200+i)
		j.WorkflowFullRef = j.Owner + "/" + j.Repo + "/.github/workflows/ci.yml@" + j.WorkflowRef
		if err := st.UpsertJob(j); err != nil {
			t.Fatal(err)
		}
	}
	list := func(query string) (int, []store.Job, string) {
		w := get(h, "/api/jobs"+query, "vera")
		var jobs []store.Job
		if w.Code == http.StatusOK {
			if err := json.Unmarshal(w.Body.Bytes(), &jobs); err != nil {
				t.Fatalf("%s: %v (%s)", query, err, w.Body)
			}
		}
		return w.Code, jobs, w.Header().Get("X-Total-Count")
	}
	for _, tc := range []struct {
		query     string
		code      int
		wantJobs  int
		wantTotal string
	}{
		{"", 200, 3, "3"},
		{"?repo=acme/api", 200, 2, "2"},
		{"?repo=acme/web&result=succeeded", 200, 1, "1"},
		{"?result=failed", 200, 1, "1"},
		{"?ref=refs/heads/dev", 200, 1, "1"},
		{"?repo=acme/none", 200, 0, "0"},
		{"?limit=1", 200, 1, "3"}, // the total is the whole match, not the page
		{"?limit=1&offset=2", 200, 1, "3"},
		{"?limit=1&offset=5", 200, 0, "3"},
		{"?limit=-4&offset=-1", 200, 3, "3"}, // bad paging falls back to the defaults
		{"?limit=100000", 200, 3, "3"},       // and a huge page to the default
		{"?repo=acme", 400, 0, ""},           // not owner/name
		{"?repo=a/b/c", 400, 0, ""},          // name with a slash
		{"?ref=" + strings.Repeat("r", maxRefFilter+1), 400, 0, ""},
		{"?result=" + strings.Repeat("r", maxResultFilter+1), 400, 0, ""},
	} {
		code, jobs, total := list(tc.query)
		if code != tc.code || (code == 200 && (len(jobs) != tc.wantJobs || total != tc.wantTotal)) {
			t.Errorf("%q: %d, %d jobs, total %q; want %d, %d, %q", tc.query, code, len(jobs), total, tc.code, tc.wantJobs, tc.wantTotal)
		}
	}
	// Nothing at all is [], not null.
	_, _, _, h2 := apiServer(t)
	if w := get(h2, "/api/jobs", "vera"); strings.TrimSpace(w.Body.String()) != "[]" {
		t.Fatalf("empty history: %q", w.Body)
	}
	if w := get(h, "/api/jobs", ""); w.Code != http.StatusUnauthorized {
		t.Fatalf("anonymous: %d", w.Code)
	}
}

func TestJobReposEndpoint(t *testing.T) {
	t.Parallel()
	_, st, _, h := apiServer(t)
	for i, repo := range []string{"api", "web", "api"} {
		if err := st.UpsertJob(store.Job{TargetID: 1, JobID: strconv.Itoa(i), RunID: int64(i + 1), Owner: "acme", Repo: repo, Slot: -1}); err != nil {
			t.Fatal(err)
		}
	}
	w := get(h, "/api/jobs/repos", "vera")
	var repos []string
	if err := json.Unmarshal(w.Body.Bytes(), &repos); err != nil || w.Code != http.StatusOK || len(repos) != 2 {
		t.Fatalf("%d %s", w.Code, w.Body)
	}
}

func TestJobMemoryEndpoint(t *testing.T) {
	t.Parallel()
	_, st, fp, h := apiServer(t)
	fp.cfg.MemoryMB = 1024
	fp.setMem(pool.MemoryState{Total: 16 << 30, Budget: 14 << 30})
	now := time.Now().Unix()
	for i, peak := range []int64{2 << 30, 3 << 30} {
		runner := "pitlane-" + strconv.Itoa(i)
		if err := st.UpsertJob(store.Job{TargetID: 1, JobID: strconv.Itoa(i), RunID: int64(i + 1), Owner: "acme", Repo: "api", RunnerName: runner, Slot: -1,
			StartedAt: now - 100, FinishedAt: now - 10, Result: "succeeded"}); err != nil {
			t.Fatal(err)
		}
		ok, err := st.SetJobUsage(t.Context(), runner, store.JobUsage{WallMs: 1000, Runner: store.ContainerUsage{MemPeak: &peak}})
		if err != nil || !ok {
			t.Fatalf("usage: %v %v", ok, err)
		}
	}
	w := get(h, "/api/jobs/memory", "vera")
	var m memorySizing
	if err := json.Unmarshal(w.Body.Bytes(), &m); err != nil || w.Code != http.StatusOK {
		t.Fatalf("%d %s", w.Code, w.Body)
	}
	if m.Slots != 4 || m.MemoryLimitMB != 1024 || m.MemTotal != 16<<30 || m.Usable != 14<<30 || m.WindowDays != 7 || m.Count != 2 ||
		m.P90 == nil || *m.P90 != 3<<30 || m.SuggestedSlots == nil || *m.SuggestedSlots != 4 {
		t.Fatalf("%+v", m)
	}
}

func TestStreamWriter(t *testing.T) {
	t.Parallel()
	rec := httptest.NewRecorder()
	w := streamWriter(rec)
	if _, err := w.Write([]byte("line\n")); err != nil || !rec.Flushed || rec.Body.String() != "line\n" {
		t.Fatalf("flushed=%v body=%q err=%v", rec.Flushed, rec.Body, err)
	}
	plain := noFlush{httptest.NewRecorder()}
	if got := streamWriter(plain); got != any(plain) {
		t.Fatal("a writer that cannot flush must be used as it is")
	}
}

func TestJobLogEndpoint(t *testing.T) {
	t.Parallel()
	_, st, _, h := apiServer(t)
	tg, err := st.CreateTarget(store.Target{Kind: "repo", Owner: "acme", Repo: "api", Enabled: true, MaxRunners: 1, InstallationID: 7})
	if err != nil {
		t.Fatal(err)
	}
	for _, j := range []store.Job{
		{TargetID: tg.ID, JobID: "j1", RunID: 42, Owner: "acme", Repo: "api", RunnerName: "pitlane-live", Slot: -1},
		{TargetID: tg.ID, JobID: "j2", RunID: 0, Owner: "acme", Repo: "api", RunnerName: "pitlane-norun", Slot: -1},
		{TargetID: 999, JobID: "j3", RunID: 43, Owner: "gone", Repo: "repo", RunnerName: "pitlane-gone", Slot: -1},
	} {
		if err := st.UpsertJob(j); err != nil {
			t.Fatal(err)
		}
	}
	for _, tc := range []struct {
		name, runner string
		code         int
		msg          string
	}{
		{"invalid name", "a;b", 400, "invalid runner name"},
		{"no job", "pitlane-none", 404, "no job recorded"},
		{"job without a run", "pitlane-norun", 404, "no job recorded"},
		{"target removed", "pitlane-gone", 404, "no longer enabled"},
	} {
		w := send(h, "GET", "/api/runners/"+tc.runner+"/job-log", "", session("vera"))
		if w.Code != tc.code || !bodyHas(w, tc.msg) {
			t.Errorf("%s: %d %s, want %d %q", tc.name, w.Code, w.Body, tc.code, tc.msg)
		}
	}
}

func TestGitHubJobFallsBackToTheRunPage(t *testing.T) {
	t.Parallel()
	s, st, _, h := apiServer(t)
	tg, _ := st.CreateTarget(store.Target{Kind: "repo", Owner: "acme", Repo: "api", Enabled: true, MaxRunners: 1})
	for _, j := range []store.Job{
		{TargetID: tg.ID, JobID: "j1", RunID: 42, Owner: "acme", Repo: "api", RunnerName: "pitlane-a", Slot: -1},
		{TargetID: tg.ID, JobID: "j2", RunID: 43, Owner: "acme", Repo: "../x", RunnerName: "pitlane-b", Slot: -1},
	} {
		if err := st.UpsertJob(j); err != nil {
			t.Fatal(err)
		}
	}
	// No GitHub App key: the job page cannot be looked up, the run page is
	// still a useful answer.
	w := get(h, "/api/runners/pitlane-a/github", "vera")
	if w.Code != http.StatusFound || w.Header().Get("Location") != "https://github.com/acme/api/actions/runs/42" {
		t.Fatalf("%d %q", w.Code, w.Header().Get("Location"))
	}
	// A repository name that is not one is never put in a URL.
	if w := get(h, "/api/runners/pitlane-b/github", "vera"); w.Code != http.StatusNotFound {
		t.Fatalf("odd repository: %d %q", w.Code, w.Header().Get("Location"))
	}
	if w := get(h, "/api/runners/bad;name/github", "vera"); w.Code != http.StatusBadRequest {
		t.Fatalf("odd runner: %d", w.Code)
	}
	// A cached ID is used without asking GitHub.
	s.jobIDs.put(jobKey{"pitlane-a", 42}, 9001)
	if w := get(h, "/api/runners/pitlane-a/github", "vera"); w.Header().Get("Location") != "https://github.com/acme/api/actions/runs/42/job/9001" {
		t.Fatalf("%q", w.Header().Get("Location"))
	}
}

func TestRunnerLogsEndpoint(t *testing.T) {
	t.Parallel()
	_, _, _, h := apiServer(t)
	if w := get(h, "/api/runners/pitlane-none/logs", "vera"); w.Code != http.StatusNotFound || !bodyHas(w, "no logs for this runner") {
		t.Fatalf("%d %s", w.Code, w.Body)
	}
	if w := get(h, "/api/runners/pitlane-none/logs", ""); w.Code != http.StatusUnauthorized {
		t.Fatalf("anonymous: %d", w.Code)
	}
}
