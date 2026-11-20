package server

import (
	"context"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/TuroYT/pitlane/internal/gh"
	"github.com/TuroYT/pitlane/internal/store"
)

type compareRow struct {
	Repo          string  `json:"repo"`
	Workflow      string  `json:"workflow"`
	Job           string  `json:"job"`
	HostedMedian  float64 `json:"hostedMedian"`
	HostedCount   int     `json:"hostedCount"`
	PitlaneMedian float64 `json:"pitlaneMedian"`
	PitlaneCount  int     `json:"pitlaneCount"`
}

type compareCache struct {
	mu   sync.Mutex
	at   time.Time
	rows []compareRow
}

func median(v []float64) float64 {
	if len(v) == 0 {
		return 0
	}
	sort.Float64s(v)
	return v[len(v)/2]
}

// handleCompare compares the median duration of each successful job on
// Pitlane with the same job on GitHub-hosted runners (from the Actions
// history of every enabled repository). Cached for 15 minutes.
func (s *Server) handleCompare(w http.ResponseWriter, r *http.Request) {
	c := &s.compare
	c.mu.Lock()
	defer c.mu.Unlock()
	if r.URL.Query().Get("refresh") != "1" && time.Since(c.at) < 15*time.Minute && c.rows != nil {
		writeJSON(w, c.rows)
		return
	}
	targets, err := s.store.Targets()
	if err != nil {
		s.internalErr(w, r, http.StatusInternalServerError, "targets_list", "cannot list the targets", err)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 90*time.Second)
	defer cancel()
	type key struct{ repo, wf, job string }
	type acc struct{ hosted, pitlane []float64 }
	groups := map[key]*acc{}
	var mu sync.Mutex
	var wg sync.WaitGroup
	for _, t := range targets {
		if t.Kind != "repo" || !t.Enabled {
			continue
		}
		wg.Add(1)
		go func(t store.Target) {
			defer wg.Done()
			tok, err := s.token(ctx, t)
			if err != nil {
				return
			}
			jobs, err := gh.RecentJobs(ctx, tok, t.Owner, t.Repo, 60)
			if err != nil {
				return
			}
			mu.Lock()
			defer mu.Unlock()
			for _, j := range jobs {
				k := key{t.FullName(), j.Workflow, j.Name}
				a := groups[k]
				if a == nil {
					a = &acc{}
					groups[k] = a
				}
				switch {
				case strings.HasPrefix(j.RunnerName, "pitlane-"):
					a.pitlane = append(a.pitlane, j.Seconds)
				case strings.HasPrefix(j.RunnerName, "GitHub Actions"):
					a.hosted = append(a.hosted, j.Seconds)
				}
			}
		}(t)
	}
	wg.Wait()
	rows := []compareRow{}
	for k, a := range groups {
		if len(a.pitlane) == 0 && len(a.hosted) == 0 {
			continue
		}
		rows = append(rows, compareRow{Repo: k.repo, Workflow: k.wf, Job: k.job,
			HostedMedian: median(a.hosted), HostedCount: len(a.hosted), PitlaneMedian: median(a.pitlane), PitlaneCount: len(a.pitlane)})
	}
	sort.Slice(rows, func(i, j int) bool {
		if rows[i].Repo != rows[j].Repo {
			return rows[i].Repo < rows[j].Repo
		}
		return rows[i].Workflow+rows[i].Job < rows[j].Workflow+rows[j].Job
	})
	c.rows, c.at = rows, time.Now()
	writeJSON(w, rows)
}
