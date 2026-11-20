package server

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/TuroYT/pitlane/internal/gh"
	"github.com/TuroYT/pitlane/internal/store"
)

// Job list page sizes (?limit=) and filter bounds.
const (
	defaultJobsPage = 200
	maxJobsPage     = 1000
	maxRefFilter    = 200
	maxResultFilter = 20
)

// gitHubJobTimeout bounds the GitHub lookup of a job's page.
const gitHubJobTimeout = 15 * time.Second

func (s *Server) handleJobs(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	limit, _ := strconv.Atoi(q.Get("limit"))
	if limit <= 0 || limit > maxJobsPage {
		limit = defaultJobsPage
	}
	offset, _ := strconv.Atoi(q.Get("offset"))
	f := store.JobFilter{Result: q.Get("result"), Ref: q.Get("ref")}
	if repo := q.Get("repo"); repo != "" {
		owner, name, ok := strings.Cut(repo, "/")
		if !ok || !validName(owner) || !validName(name) {
			writeErr(w, http.StatusBadRequest, "repo must be owner/name")
			return
		}
		f.Owner, f.Repo = owner, name
	}
	if len(f.Ref) > maxRefFilter || len(f.Result) > maxResultFilter {
		writeErr(w, http.StatusBadRequest, "filter too long")
		return
	}
	jobs, total, err := s.store.JobsPage(r.Context(), f, limit, max(0, offset))
	if err != nil {
		s.internalErr(w, r, http.StatusInternalServerError, "jobs_list", "cannot list the jobs", err)
		return
	}
	if jobs == nil {
		jobs = []store.Job{}
	}
	// The body stays a plain array (older callers); the total matching jobs
	// is in a header, for pagination.
	w.Header().Set("X-Total-Count", strconv.Itoa(total))
	writeJSON(w, jobs)
}

func (s *Server) handleJobRepos(w http.ResponseWriter, r *http.Request) {
	repos, err := s.store.JobRepos(r.Context())
	if err != nil {
		s.internalErr(w, r, http.StatusInternalServerError, "job_repos", "cannot list the job repositories", err)
		return
	}
	writeJSON(w, repos)
}

// flushWriter flushes every write, so a streamed log reaches the browser
// as it comes.
type flushWriter struct {
	w http.ResponseWriter
	f http.Flusher
}

func (fw flushWriter) Write(p []byte) (int, error) {
	n, err := fw.w.Write(p)
	fw.f.Flush()
	return n, err
}

// streamWriter is w, flushed after every write when it can be.
func streamWriter(w http.ResponseWriter) io.Writer {
	if fl, ok := w.(http.Flusher); ok {
		return flushWriter{w, fl}
	}
	return w
}

// runnerName returns the {name} path value, or answers 400 and returns false
// when it is not a valid runner name.
func runnerName(w http.ResponseWriter, r *http.Request) (string, bool) {
	name := r.PathValue("name")
	if !validName(name) {
		writeErr(w, http.StatusBadRequest, "invalid runner name")
		return "", false
	}
	return name, true
}

// handleLogs streams a runner's own log: live from its container while it
// runs, from the archived file afterwards.
func (s *Server) handleLogs(w http.ResponseWriter, r *http.Request) {
	name, ok := runnerName(w, r)
	if !ok {
		return
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("X-Accel-Buffering", "no")
	if id, ok := s.mgr.RunnerContainer(name); ok {
		_ = s.pool.Logs(r.Context(), id, r.URL.Query().Get("follow") == "1", streamWriter(w))
		return
	}
	f, err := os.Open(s.mgr.LogFile(name))
	if err != nil {
		writeErr(w, http.StatusNotFound, "no logs for this runner")
		return
	}
	defer func() { _ = f.Close() }() // read only
	_, _ = io.Copy(w, f)
}

// handleJobLog streams the step output of the job a runner ran: live from the
// runner while it runs, from the GitHub API once it is done.
func (s *Server) handleJobLog(w http.ResponseWriter, r *http.Request) {
	name, ok := runnerName(w, r)
	if !ok {
		return
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("X-Accel-Buffering", "no")
	if id, ok := s.mgr.RunnerContainer(name); ok {
		_ = s.pool.TailJobOutput(r.Context(), id, streamWriter(w))
		return
	}
	job, err := s.store.JobByRunner(r.Context(), name)
	if err != nil || job.RunID == 0 {
		writeErr(w, http.StatusNotFound, "no job recorded for this runner")
		return
	}
	token := s.cfg.PAT
	if token == "" {
		t, err := s.store.Target(job.TargetID)
		if err != nil {
			writeErr(w, http.StatusNotFound, "repository is no longer enabled")
			return
		}
		if token, err = s.token(r.Context(), t); err != nil {
			s.internalErr(w, r, http.StatusBadGateway, "github_token", "cannot get a GitHub token for this repository", err)
			return
		}
	}
	rc, err := gh.JobLog(r.Context(), token, job.Owner, job.Repo, job.RunID, name)
	if err != nil {
		s.internalErr(w, r, http.StatusBadGateway, "github_job_log", "cannot fetch the job log from GitHub", err)
		return
	}
	defer rc.Close()
	_, _ = io.Copy(w, rc)
}

// handleGitHubJob redirects to the page of the job a runner ran on GitHub.
// GitHub's scale set messages do not carry the job's numeric ID, so it is
// looked up once in the run's job list (by runner name) and cached, only for
// a job the store has and only once GitHub found it (the cache is bounded,
// see jobIDCache). When it cannot be found, it falls back to the run page.
func (s *Server) handleGitHubJob(w http.ResponseWriter, r *http.Request) {
	name, ok := runnerName(w, r)
	if !ok {
		return
	}
	job, err := s.store.JobByRunner(r.Context(), name)
	if err != nil || job.RunID == 0 || !validName(job.Owner) || !validName(job.Repo) {
		writeErr(w, http.StatusNotFound, "no job recorded for this runner")
		return
	}
	runURL := fmt.Sprintf("https://github.com/%s/%s/actions/runs/%d", job.Owner, job.Repo, job.RunID)
	id, err := s.gitHubJobID(r.Context(), job, name)
	if err != nil {
		s.log.Debug("github job page not found, opening the run", "runner", name, "err", err)
		http.Redirect(w, r, runURL, http.StatusFound)
		return
	}
	http.Redirect(w, r, fmt.Sprintf("%s/job/%d", runURL, id), http.StatusFound)
}

// gitHubJobID returns GitHub's numeric ID of the job a runner ran: from the
// cache, else asked to GitHub and cached once found.
func (s *Server) gitHubJobID(ctx context.Context, job store.Job, runner string) (int64, error) {
	key := jobKey{runner, job.RunID}
	if id, ok := s.jobIDs.get(key); ok {
		return id, nil
	}
	ctx, cancel := context.WithTimeout(ctx, gitHubJobTimeout)
	defer cancel()
	t, err := s.store.Target(job.TargetID)
	if err != nil {
		return 0, fmt.Errorf("target %d: %w", job.TargetID, err)
	}
	tok, err := s.token(ctx, t)
	if err != nil {
		return 0, err
	}
	id, err := gh.RunJobID(ctx, tok, job.Owner, job.Repo, job.RunID, runner)
	if err != nil {
		return 0, err
	}
	s.jobIDs.put(key, id)
	return id, nil
}
