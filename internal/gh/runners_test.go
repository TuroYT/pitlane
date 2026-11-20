package gh

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"testing"
)

func TestLatestRunnerVersion(t *testing.T) {
	tests := []struct {
		name   string
		status int
		body   string
		want   string
		fail   bool
	}{
		{"tag with a leading v", 200, `{"tag_name":"v2.330.0"}`, "2.330.0", false},
		{"tag without v", 200, `{"tag_name":"2.331.1"}`, "2.331.1", false},
		{"pre-release suffix is kept", 200, `{"tag_name":"v2.332.0-rc1"}`, "2.332.0-rc1", false},
		{"rate limited", 403, `{"message":"API rate limit exceeded"}`, "", true},
		{"no release", 404, `{"message":"Not Found"}`, "", true},
		{"malformed JSON", 200, `{"tag_name":`, "", true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			serveAPI(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != "/repos/actions/runner/releases/latest" {
					t.Errorf("path = %s", r.URL.Path)
				}
				requireGitHubHeaders(t, r)
				// Public endpoint: no credentials must be sent.
				if a := r.Header.Get("Authorization"); a != "" {
					t.Errorf("Authorization = %q, want none", a)
				}
				w.WriteHeader(tc.status)
				_, _ = w.Write([]byte(tc.body))
			}))
			got, err := LatestRunnerVersion(context.Background())
			if (err != nil) != tc.fail {
				t.Fatalf("err = %v, want failure: %v", err, tc.fail)
			}
			if got != tc.want {
				t.Errorf("version = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestLatestRunnerVersionCanceled(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	serveAPI(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		cancel()
		<-r.Context().Done()
	}))
	if _, err := LatestRunnerVersion(ctx); !errors.Is(err, context.Canceled) {
		t.Errorf("err = %v, want context.Canceled", err)
	}
}

// fakeRuns serves the completed runs of acme/app and the jobs of each run.
// A run whose jobs reply is missing answers 500.
type fakeRuns struct {
	t       *testing.T
	runs    string            // JSON of the workflow_runs list
	jobs    map[string]string // run ID -> JSON of its jobs
	perPage string
}

func (f *fakeRuns) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	requireGitHubHeaders(f.t, r)
	if got := r.Header.Get("Authorization"); got != "Bearer tok" {
		f.t.Errorf("Authorization = %q, want Bearer tok", got)
	}
	switch {
	case r.URL.Path == "/repos/acme/app/actions/runs":
		if got := r.URL.Query().Get("status"); got != "completed" {
			f.t.Errorf("status = %q, want completed", got)
		}
		f.perPage = r.URL.Query().Get("per_page")
		_, _ = w.Write([]byte(`{"workflow_runs":` + f.runs + `}`))
	case strings.HasPrefix(r.URL.Path, "/repos/acme/app/actions/runs/") && strings.HasSuffix(r.URL.Path, "/jobs"):
		id := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/repos/acme/app/actions/runs/"), "/jobs")
		body, ok := f.jobs[id]
		if !ok {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		_, _ = w.Write([]byte(body))
	default:
		f.t.Errorf("unexpected request %s %s", r.Method, r.URL)
		w.WriteHeader(http.StatusNotFound)
	}
}

func TestRecentJobs(t *testing.T) {
	f := &fakeRuns{
		t:    t,
		runs: `[{"id":1,"name":"CI"},{"id":2,"name":"Release"},{"id":3,"name":"Broken"},{"id":4,"name":"Empty"}]`,
		jobs: map[string]string{
			"1": `{"jobs":[
				{"name":"build","runner_name":"pitlane-a","conclusion":"success","started_at":"2026-01-02T10:00:00Z","completed_at":"2026-01-02T10:01:30Z"},
				{"name":"test","runner_name":"pitlane-b","conclusion":"failure","started_at":"2026-01-02T10:00:00Z","completed_at":"2026-01-02T10:05:00Z"},
				{"name":"skipped","runner_name":"","conclusion":"skipped","started_at":"2026-01-02T10:00:00Z","completed_at":"2026-01-02T10:00:00Z"},
				{"name":"never started","runner_name":"x","conclusion":"success","completed_at":"2026-01-02T10:00:10Z"},
				{"name":"clock skew","runner_name":"x","conclusion":"success","started_at":"2026-01-02T10:00:10Z","completed_at":"2026-01-02T10:00:00Z"}]}`,
			"2": `{"jobs":[{"name":"publish","runner_name":"pitlane-c","conclusion":"success","started_at":"2026-01-03T08:00:00Z","completed_at":"2026-01-03T08:00:00.5Z"}]}`,
			// run 3: the jobs request fails with a 500, the run is skipped.
			"4": `{"jobs":[]}`,
		},
	}
	serveAPI(t, f)
	got, err := RecentJobs(context.Background(), "tok", "acme", "app", 60)
	if err != nil {
		t.Fatal(err)
	}
	want := []JobRun{
		{Workflow: "CI", Name: "build", RunnerName: "pitlane-a", Seconds: 90},
		{Workflow: "Release", Name: "publish", RunnerName: "pitlane-c", Seconds: 0.5},
	}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Errorf("jobs = %+v, want %+v", got, want)
	}
	if f.perPage != "60" {
		t.Errorf("per_page = %q, want 60", f.perPage)
	}
}

func TestRecentJobsPageSize(t *testing.T) {
	for _, tc := range []struct {
		maxRuns int
		want    string
	}{{1, "1"}, {100, "100"}, {500, "100"}} {
		f := &fakeRuns{t: t, runs: `[]`}
		serveAPI(t, f)
		got, err := RecentJobs(context.Background(), "tok", "acme", "app", tc.maxRuns)
		if err != nil || len(got) != 0 {
			t.Fatalf("maxRuns %d: %v %v", tc.maxRuns, got, err)
		}
		if f.perPage != tc.want {
			t.Errorf("maxRuns %d: per_page = %q, want %q (GitHub caps it at 100)", tc.maxRuns, f.perPage, tc.want)
		}
	}
}

func TestRecentJobsRunsFailure(t *testing.T) {
	for _, tc := range []struct {
		name   string
		status int
		body   string
		code   int
	}{
		{"unauthorized", 401, `{"message":"Bad credentials"}`, 401},
		{"not found", 404, `{"message":"Not Found"}`, 404},
		{"malformed JSON", 200, `{"workflow_runs":[`, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			serveAPI(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(tc.status)
				_, _ = w.Write([]byte(tc.body))
			}))
			got, err := RecentJobs(context.Background(), "tok", "acme", "app", 10)
			if err == nil || got != nil {
				t.Fatalf("got %v %v, want nil and an error", got, err)
			}
			if statusCode(err) != tc.code {
				t.Errorf("status = %d, want %d", statusCode(err), tc.code)
			}
		})
	}
}

func TestRecentJobsEscapesOwnerAndRepo(t *testing.T) {
	var path string
	serveAPI(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path = r.URL.EscapedPath()
		_, _ = w.Write([]byte(`{"workflow_runs":[]}`))
	}))
	if _, err := RecentJobs(context.Background(), "tok", "a/b", "c d", 1); err != nil {
		t.Fatal(err)
	}
	if want := "/repos/a%2Fb/c%20d/actions/runs"; path != want {
		t.Errorf("path = %q, want %q", path, want)
	}
}

// runnersJSON is a /actions/runners answer of runners n..n+count-1.
func runnersJSON(total, from, count int) string {
	type runner struct {
		ID     int    `json:"id"`
		Name   string `json:"name"`
		Status string `json:"status"`
		Busy   bool   `json:"busy"`
	}
	var rs []runner
	for i := from; i < from+count; i++ {
		rs = append(rs, runner{ID: i, Name: fmt.Sprintf("r%d", i), Status: "online", Busy: i%2 == 0})
	}
	b, _ := json.Marshal(map[string]any{"total_count": total, "runners": rs})
	return string(b)
}

func TestListRunnersScopes(t *testing.T) {
	tests := []struct {
		name, owner, repo, wantPath string
	}{
		{"repository", "acme", "app", "/repos/acme/app/actions/runners"},
		{"organization", "acme", "", "/orgs/acme/actions/runners"},
		{"escaped", "a/b", "c d", "/repos/a%2Fb/c%20d/actions/runners"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var path string
			serveAPI(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				requireGitHubHeaders(t, r)
				if got := r.Header.Get("Authorization"); got != "Bearer tok" {
					t.Errorf("Authorization = %q", got)
				}
				if q := r.URL.Query(); q.Get("per_page") != "100" || q.Get("page") != "1" {
					t.Errorf("query = %v, want per_page=100 page=1", q)
				}
				path = r.URL.EscapedPath()
				_, _ = w.Write([]byte(runnersJSON(2, 1, 2)))
			}))
			got, err := ListRunners(context.Background(), "tok", tc.owner, tc.repo)
			if err != nil {
				t.Fatal(err)
			}
			if path != tc.wantPath {
				t.Errorf("path = %q, want %q", path, tc.wantPath)
			}
			want := []Runner{{ID: 1, Name: "r1", Status: "online"}, {ID: 2, Name: "r2", Status: "online", Busy: true}}
			if fmt.Sprint(got) != fmt.Sprint(want) {
				t.Errorf("runners = %+v, want %+v", got, want)
			}
		})
	}
}

func TestListRunnersPagination(t *testing.T) {
	tests := []struct {
		name      string
		total     int
		wantPages int
	}{
		{"none", 0, 1},
		{"one short page", 40, 1},
		// total_count tells a full last page from a first of several.
		{"exactly one full page", 100, 1},
		{"two pages", 150, 2},
		{"three pages", 201, 3},
		{"capped at 1000 runners", 5000, 10},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var pages []string
			serveAPI(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				pages = append(pages, r.URL.Query().Get("page"))
				var page int
				_, _ = fmt.Sscan(r.URL.Query().Get("page"), &page)
				from := (page-1)*100 + 1
				_, _ = w.Write([]byte(runnersJSON(tc.total, from, max(0, min(100, tc.total-from+1)))))
			}))
			got, err := ListRunners(context.Background(), "tok", "acme", "")
			if err != nil {
				t.Fatal(err)
			}
			if len(pages) != tc.wantPages {
				t.Errorf("requested pages %v, want %d", pages, tc.wantPages)
			}
			if want := min(tc.total, 1000); len(got) != want {
				t.Fatalf("got %d runners, want %d", len(got), want)
			}
			for i, r := range got {
				if r.ID != int64(i+1) {
					t.Fatalf("runner %d has ID %d: pages were not appended in order", i, r.ID)
				}
			}
		})
	}
}

func TestListRunnersErrors(t *testing.T) {
	t.Run("first page", func(t *testing.T) {
		serveAPI(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusForbidden)
			_, _ = w.Write([]byte(`{"message":"Resource not accessible by integration"}`))
		}))
		got, err := ListRunners(context.Background(), "tok", "acme", "app")
		if statusCode(err) != http.StatusForbidden || got != nil {
			t.Errorf("got %v %v, want nil and a 403", got, err)
		}
	})
	t.Run("second page", func(t *testing.T) {
		serveAPI(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Query().Get("page") == "2" {
				w.WriteHeader(http.StatusServiceUnavailable)
				return
			}
			_, _ = w.Write([]byte(runnersJSON(150, 1, 100)))
		}))
		got, err := ListRunners(context.Background(), "tok", "acme", "app")
		if statusCode(err) != http.StatusServiceUnavailable || got != nil {
			t.Errorf("got %d runners, err %v: a partial listing must not look complete", len(got), err)
		}
	})
	t.Run("malformed JSON", func(t *testing.T) {
		serveAPI(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			_, _ = w.Write([]byte(`{"runners":`))
		}))
		if _, err := ListRunners(context.Background(), "tok", "acme", "app"); err == nil {
			t.Error("expected a decode error")
		}
	})
	t.Run("canceled", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		serveAPI(t, http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
			t.Error("a canceled context still sent a request")
		}))
		if _, err := ListRunners(ctx, "tok", "acme", "app"); !errors.Is(err, context.Canceled) {
			t.Errorf("err = %v, want context.Canceled", err)
		}
	})
}

func TestRunJobID(t *testing.T) {
	serveAPI(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requireGitHubHeaders(t, r)
		if r.URL.Path != "/repos/acme/app/actions/runs/5/jobs" {
			t.Errorf("path = %s", r.URL.Path)
		}
		if q := r.URL.Query(); q.Get("filter") != "all" || q.Get("per_page") != "100" {
			t.Errorf("query = %v, want filter=all (earlier attempts too) per_page=100", q)
		}
		_, _ = w.Write([]byte(`{"jobs":[
			{"id":10,"runner_name":"pitlane-a"},
			{"id":11,"runner_name":"other"},
			{"id":12,"runner_name":"pitlane-a"}]}`))
	}))
	ctx := context.Background()
	// A retried job ran on the same runner name: the latest one wins.
	if id, err := RunJobID(ctx, "tok", "acme", "app", 5, "pitlane-a"); err != nil || id != 12 {
		t.Errorf("pitlane-a: %d %v, want 12", id, err)
	}
	if id, err := RunJobID(ctx, "tok", "acme", "app", 5, "other"); err != nil || id != 11 {
		t.Errorf("other: %d %v, want 11", id, err)
	}
	_, err := RunJobID(ctx, "tok", "acme", "app", 5, "nobody")
	if err == nil || !strings.Contains(err.Error(), "nobody") || !strings.Contains(err.Error(), "run 5") {
		t.Errorf("unknown runner: err = %v", err)
	}
}

func TestRunJobIDAPIError(t *testing.T) {
	serveAPI(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	if _, err := RunJobID(context.Background(), "tok", "acme", "app", 5, "r"); statusCode(err) != http.StatusNotFound {
		t.Errorf("err = %v, want a 404", err)
	}
}

func TestJobCreatedAtAPIErrorAndZeroDates(t *testing.T) {
	t.Run("API error", func(t *testing.T) {
		serveAPI(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusBadGateway)
		}))
		if _, err := JobCreatedAt(context.Background(), "tok", "acme", "app", 5, "r"); statusCode(err) != http.StatusBadGateway {
			t.Errorf("err = %v, want a 502", err)
		}
	})
	t.Run("job without created_at is not a match", func(t *testing.T) {
		serveAPI(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			_, _ = w.Write([]byte(`{"jobs":[{"runner_name":"r"}]}`))
		}))
		if _, err := JobCreatedAt(context.Background(), "tok", "acme", "app", 5, "r"); err == nil {
			t.Error("expected an error for a job with no creation date")
		}
	})
}

// Even when every page is full, JobCreatedAt stops after maxJobPages.
func TestJobCreatedAtPageLimit(t *testing.T) {
	var pages int
	serveAPI(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		pages++
		_, _ = w.Write([]byte(`{"jobs":[` + strings.Repeat(`{"runner_name":"other","created_at":"2026-01-02T10:00:00Z"},`, jobsPerPage-1) +
			`{"runner_name":"other","created_at":"2026-01-02T10:00:00Z"}]}`))
	}))
	if _, err := JobCreatedAt(context.Background(), "tok", "acme", "app", 5, "nobody"); err == nil {
		t.Fatal("expected an error")
	}
	if pages != maxJobPages {
		t.Errorf("read %d pages, want the limit of %d", pages, maxJobPages)
	}
}

func TestJobLog(t *testing.T) {
	serveAPI(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/repos/acme/api/actions/runs/1/jobs":
			_, _ = w.Write([]byte(`{"jobs":[{"id":7,"runner_name":"pitlane-a"}]}`))
		case "/repos/acme/api/actions/jobs/7/logs":
			if got := r.Header.Get("Authorization"); got != "Bearer tok" {
				t.Errorf("Authorization = %q", got)
			}
			if got := r.Header.Get("Accept"); got != "application/vnd.github+json" {
				t.Errorf("Accept = %q", got)
			}
			if got := r.Header.Get("User-Agent"); got != "pitlane" {
				t.Errorf("User-Agent = %q", got)
			}
			http.Redirect(w, r, "/blob/log.txt?sig=abc", http.StatusFound)
		case "/blob/log.txt":
			_, _ = io.WriteString(w, "line 1\nline 2\n")
		default:
			t.Errorf("unexpected request %s", r.URL)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	rc, err := JobLog(context.Background(), "tok", "acme", "api", 1, "pitlane-a")
	if err != nil {
		t.Fatal(err)
	}
	defer rc.Close()
	b, err := io.ReadAll(rc)
	if err != nil {
		t.Fatal(err)
	}
	if string(b) != "line 1\nline 2\n" {
		t.Errorf("log = %q", b)
	}
}

func TestJobLogErrors(t *testing.T) {
	t.Run("unknown runner", func(t *testing.T) {
		serveAPI(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			_, _ = w.Write([]byte(`{"jobs":[]}`))
		}))
		if _, err := JobLog(context.Background(), "tok", "acme", "api", 1, "nobody"); err == nil {
			t.Error("expected an error")
		}
	})
	t.Run("logs expired", func(t *testing.T) {
		serveAPI(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if strings.HasSuffix(r.URL.Path, "/logs") {
				w.WriteHeader(http.StatusGone)
				return
			}
			_, _ = w.Write([]byte(`{"jobs":[{"id":7,"runner_name":"pitlane-a"}]}`))
		}))
		rc, err := JobLog(context.Background(), "tok", "acme", "api", 1, "pitlane-a")
		if err == nil || rc != nil {
			t.Fatalf("got %v %v, want an error", rc, err)
		}
		if !strings.Contains(err.Error(), "410") {
			t.Errorf("error %q does not give the status", err)
		}
	})
}

func TestWithoutQuery(t *testing.T) {
	t.Parallel()
	t.Run("other errors pass through", func(t *testing.T) {
		t.Parallel()
		err := errors.New("boom")
		if got := withoutQuery(err); got != err {
			t.Errorf("got %v", got)
		}
	})
	t.Run("query and fragment dropped", func(t *testing.T) {
		t.Parallel()
		in := &url.Error{Op: "Get", URL: "https://blob.example/x/log.txt?sig=SECRET#frag", Err: io.EOF}
		got := withoutQuery(in)
		var ue *url.Error
		if !errors.As(got, &ue) {
			t.Fatalf("got %T", got)
		}
		if ue.URL != "https://blob.example/x/log.txt" || ue.Op != "Get" || !errors.Is(got, io.EOF) {
			t.Errorf("got %+v", ue)
		}
	})
	t.Run("unparsable URL is not echoed", func(t *testing.T) {
		t.Parallel()
		in := &url.Error{Op: "Get", URL: "http://[::1/log?sig=SECRET", Err: io.EOF}
		got := withoutQuery(in)
		if strings.Contains(got.Error(), "SECRET") {
			t.Errorf("signature leaked: %v", got)
		}
	})
}
