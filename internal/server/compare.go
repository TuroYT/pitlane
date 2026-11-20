package server

import (
	"cmp"
	"context"
	"net/http"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/TuroYT/pitlane/internal/gh"
	"github.com/TuroYT/pitlane/internal/store"
)

const (
	// compareTTL is how long a comparison is served from the cache.
	compareTTL = 15 * time.Minute
	// compareTimeout bounds the GitHub calls of one comparison.
	compareTimeout = 90 * time.Second
	// compareRuns is how many recent workflow runs are read per repository.
	compareRuns = 60
	// Runner name prefixes telling Pitlane's runners from GitHub-hosted ones.
	pitlaneRunnerPrefix = "pitlane-"
	hostedRunnerPrefix  = "GitHub Actions"
)

// compareRow is the median duration of one job on GitHub-hosted runners and
// on Pitlane, in seconds, with the number of runs each median comes from.
type compareRow struct {
	Repo          string  `json:"repo"`
	Workflow      string  `json:"workflow"`
	Job           string  `json:"job"`
	HostedMedian  float64 `json:"hostedMedian"`
	HostedCount   int     `json:"hostedCount"`
	PitlaneMedian float64 `json:"pitlaneMedian"`
	PitlaneCount  int     `json:"pitlaneCount"`
}

// repoJobs are the recent successful jobs of one repository.
type repoJobs struct {
	Repo string // owner/name
	Jobs []gh.JobRun
}

// handleCompare compares the median duration of each successful job on
// Pitlane with the same job on GitHub-hosted runners (from the Actions
// history of every enabled repository). Cached for compareTTL; ?refresh=1
// skips the cache.
func (s *Server) handleCompare(w http.ResponseWriter, r *http.Request) {
	refresh := r.URL.Query().Get("refresh") == "1"
	rows, err := s.compare.get(refresh, func() ([]compareRow, bool, error) {
		targets, err := s.store.Targets()
		if err != nil {
			return nil, false, err
		}
		ctx, cancel := context.WithTimeout(r.Context(), compareTimeout)
		defer cancel()
		rows := compareRows(s.recentRepoJobs(ctx, targets))
		// Past the deadline or with the client gone, some repositories may
		// be missing: answer what was read, but do not cache it.
		return rows, ctx.Err() == nil, nil
	})
	if err != nil {
		s.internalErr(w, r, http.StatusInternalServerError, "targets_list", "cannot list the targets", err)
		return
	}
	writeJSON(w, rows)
}

// recentRepoJobs reads the recent jobs of every enabled repository target
// from GitHub, in parallel. A repository GitHub cannot answer for is left
// out of the comparison.
func (s *Server) recentRepoJobs(ctx context.Context, targets []store.Target) []repoJobs {
	repos := slices.DeleteFunc(slices.Clone(targets), func(t store.Target) bool { return t.Kind != "repo" || !t.Enabled })
	out := make([]repoJobs, len(repos))
	var wg sync.WaitGroup
	for i, t := range repos {
		wg.Go(func() {
			jobs, err := s.recentJobs(ctx, t)
			if err != nil {
				s.log.Debug("compare: cannot read the recent jobs", "target", t.FullName(), "error", err)
				return
			}
			out[i] = repoJobs{Repo: t.FullName(), Jobs: jobs}
		})
	}
	wg.Wait()
	return out
}

// recentJobs returns the recent successful jobs of a repository target.
func (s *Server) recentJobs(ctx context.Context, t store.Target) ([]gh.JobRun, error) {
	if s.jobHistory != nil {
		return s.jobHistory(ctx, t)
	}
	tok, err := s.token(ctx, t)
	if err != nil {
		return nil, err
	}
	return gh.RecentJobs(ctx, tok, t.Owner, t.Repo, compareRuns)
}

// compareRows groups jobs by repository, workflow and job name and gives the
// median duration of each group on GitHub-hosted runners and on Pitlane.
// Jobs that ran elsewhere (other self-hosted or larger runners) are left
// out, and so is a group with none on either side. Rows are sorted by
// repository, then by workflow, then by job name.
func compareRows(repos []repoJobs) []compareRow {
	type key struct{ repo, workflow, job string }
	type durations struct{ hosted, pitlane []float64 }
	groups := map[key]*durations{}
	for _, rj := range repos {
		for _, j := range rj.Jobs {
			k := key{rj.Repo, j.Workflow, j.Name}
			d := groups[k]
			if d == nil {
				d = &durations{}
				groups[k] = d
			}
			switch {
			case strings.HasPrefix(j.RunnerName, pitlaneRunnerPrefix):
				d.pitlane = append(d.pitlane, j.Seconds)
			case strings.HasPrefix(j.RunnerName, hostedRunnerPrefix):
				d.hosted = append(d.hosted, j.Seconds)
			}
		}
	}
	rows := []compareRow{}
	for k, d := range groups {
		if len(d.pitlane) == 0 && len(d.hosted) == 0 {
			continue
		}
		rows = append(rows, compareRow{Repo: k.repo, Workflow: k.workflow, Job: k.job,
			HostedMedian: median(d.hosted), HostedCount: len(d.hosted), PitlaneMedian: median(d.pitlane), PitlaneCount: len(d.pitlane)})
	}
	slices.SortFunc(rows, func(a, b compareRow) int {
		return cmp.Or(cmp.Compare(a.Repo, b.Repo), cmp.Compare(a.Workflow, b.Workflow), cmp.Compare(a.Job, b.Job))
	})
	return rows
}

// median returns the upper median of v (0 when empty), leaving v unchanged.
func median(v []float64) float64 {
	if len(v) == 0 {
		return 0
	}
	return slices.Sorted(slices.Values(v))[len(v)/2]
}

// compareCache keeps the last comparison for compareTTL. The zero value is
// ready to use.
type compareCache struct {
	// mu is held while a comparison is computed, so concurrent requests wait
	// for it instead of asking GitHub again.
	mu   sync.Mutex
	at   time.Time
	rows []compareRow // nil until a comparison succeeds
}

// get returns the cached rows while they are fresh and refresh is false,
// else the rows compute returns, cached when compute succeeds and says they
// are complete (keep).
func (c *compareCache) get(refresh bool, compute func() (rows []compareRow, keep bool, err error)) ([]compareRow, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if !refresh && c.rows != nil && time.Since(c.at) < compareTTL {
		return c.rows, nil
	}
	rows, keep, err := compute()
	if err != nil {
		return nil, err
	}
	if keep {
		c.rows, c.at = rows, time.Now()
	}
	return rows, nil
}
