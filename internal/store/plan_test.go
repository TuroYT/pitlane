package store

import (
	"strings"
	"testing"
)

// TestJobQueriesUseIndexes checks, with EXPLAIN QUERY PLAN, that the job
// queries made for every job, for each page of the history and for each
// metrics scrape never read the whole jobs table.
func TestJobQueriesUseIndexes(t *testing.T) {
	s := open(t)
	page := func(f JobFilter) string {
		_, q, _ := f.queries()
		return q
	}
	count := func(f JobFilter) string {
		q, _, _ := f.queries()
		return q
	}
	repo := JobFilter{Owner: "acme", Repo: "api"}
	cases := []struct {
		name, query string
		want        string // index the plan must use
		sorted      bool   // the plan must not sort (rows come in order from the index)
	}{
		{"history page", page(JobFilter{}), "jobs_recent2", true},
		{"repository page", page(repo), "jobs_repo2", true},
		{"repository and result page", page(JobFilter{Owner: "acme", Repo: "api", Result: "failed"}), "jobs_repo2", true},
		{"repository count", count(repo), "jobs_repo2", false},
		{"running count", count(JobFilter{Result: "running"}), "jobs_finished", false},
		{"job by runner", queryJobByRunner, "jobs_runner", true},
		{"usage by runner", querySetJobUsage, "jobs_runner", false},
		{"in-flight jobs", queryInFlightJobs, "jobs_finished", false},
		{"timings", queryJobTimings, "jobs_started", false},
		{"memory peaks", queryJobMemoryPeaks, "jobs_finished", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			n := strings.Count(c.query, "?")
			rows, err := s.db.Query("EXPLAIN QUERY PLAN "+c.query, make([]any, n)...)
			if err != nil {
				t.Fatal(err)
			}
			defer rows.Close()
			var plan []string
			for rows.Next() {
				var id, parent, notused int
				var detail string
				if err := rows.Scan(&id, &parent, &notused, &detail); err != nil {
					t.Fatal(err)
				}
				plan = append(plan, detail)
			}
			if err := rows.Err(); err != nil {
				t.Fatal(err)
			}
			all := strings.Join(plan, "\n")
			for _, line := range plan {
				// A SCAN through an index is an ordered read that LIMIT stops.
				if (line == "SCAN jobs" || line == "SCAN j") || (strings.HasPrefix(line, "SCAN j") && !strings.Contains(line, "INDEX")) {
					t.Errorf("full scan of jobs:\n%s", all)
				}
			}
			if !strings.Contains(all, c.want) {
				t.Errorf("plan does not use %s:\n%s", c.want, all)
			}
			if c.sorted && strings.Contains(all, "TEMP B-TREE") {
				t.Errorf("plan sorts rows instead of reading them in index order:\n%s", all)
			}
		})
	}
}
