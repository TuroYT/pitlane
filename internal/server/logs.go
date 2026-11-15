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

// --------------------------------------------------------------- jobs, logs

func (s *Server) handleJobs(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	limit, _ := strconv.Atoi(q.Get("limit"))
	if limit <= 0 || limit > 1000 {
		limit = 200
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
	if len(f.Ref) > 200 || len(f.Result) > 20 {
		writeErr(w, http.StatusBadRequest, "filter too long")
		return
	}
	jobs, total, err := s.store.JobsPage(r.Context(), f, limit, max(0, offset))
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
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
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, repos)
}

type flushWriter struct {
	w http.ResponseWriter
	f http.Flusher
}

func (fw flushWriter) Write(p []byte) (int, error) {
	n, err := fw.w.Write(p)
	fw.f.Flush()
	return n, err
}

func (s *Server) handleLogs(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if !validName(name) {
		writeErr(w, http.StatusBadRequest, "invalid runner name")
		return
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("X-Accel-Buffering", "no")
	if id, ok := s.mgr.RunnerContainer(name); ok {
		fl, _ := w.(http.Flusher)
		var out io.Writer = w
		if fl != nil {
			out = flushWriter{w, fl}
		}
		_ = s.pool.Logs(r.Context(), id, r.URL.Query().Get("follow") == "1", out)
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
	name := r.PathValue("name")
	if !validName(name) {
		writeErr(w, http.StatusBadRequest, "invalid runner name")
		return
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("X-Accel-Buffering", "no")
	if id, ok := s.mgr.RunnerContainer(name); ok {
		fl, _ := w.(http.Flusher)
		var out io.Writer = w
		if fl != nil {
			out = flushWriter{w, fl}
		}
		_ = s.pool.TailJobOutput(r.Context(), id, out)
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
			writeErr(w, http.StatusBadGateway, err.Error())
			return
		}
	}
	rc, err := gh.JobLog(r.Context(), token, job.Owner, job.Repo, job.RunID, name)
	if err != nil {
		writeErr(w, http.StatusBadGateway, err.Error())
		return
	}
	defer rc.Close()
	_, _ = io.Copy(w, rc)
}

// handleGitHubJob redirects to the page of the job a runner ran on GitHub.
// GitHub's scale set messages do not carry the job's numeric ID, so it is
// looked up once in the run's job list (by runner name) and cached. When it
// cannot be found, it falls back to the run page.
func (s *Server) handleGitHubJob(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if !validName(name) {
		writeErr(w, http.StatusBadRequest, "invalid runner name")
		return
	}
	job, err := s.store.JobByRunner(r.Context(), name)
	if err != nil || job.RunID == 0 || !validName(job.Owner) || !validName(job.Repo) {
		writeErr(w, http.StatusNotFound, "no job recorded for this runner")
		return
	}
	runURL := fmt.Sprintf("https://github.com/%s/%s/actions/runs/%d", job.Owner, job.Repo, job.RunID)
	if v, ok := s.jobIDs.Load(name); ok {
		http.Redirect(w, r, fmt.Sprintf("%s/job/%d", runURL, v.(int64)), http.StatusFound)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
	defer cancel()
	t, err := s.store.Target(job.TargetID)
	if err == nil {
		var tok string
		if tok, err = s.token(ctx, t); err == nil {
			var id int64
			if id, err = gh.RunJobID(ctx, tok, job.Owner, job.Repo, job.RunID, name); err == nil {
				s.jobIDs.Store(name, id)
				http.Redirect(w, r, fmt.Sprintf("%s/job/%d", runURL, id), http.StatusFound)
				return
			}
		}
	}
	s.log.Debug("github job page not found, opening the run", "runner", name, "err", err)
	http.Redirect(w, r, runURL, http.StatusFound)
}
