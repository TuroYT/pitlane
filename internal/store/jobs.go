package store

import (
	"context"
	"database/sql"
	"strings"
	"time"
)

// Job is one GitHub Actions job handled by a Pitlane runner.
type Job struct {
	ID          int64  `json:"id"`
	TargetID    int64  `json:"targetId"`
	JobID       string `json:"jobId"`
	RunID       int64  `json:"runId"`
	Owner       string `json:"owner"`
	Repo        string `json:"repo"`
	WorkflowRef string `json:"workflowRef"`
	JobName     string `json:"jobName"`
	Event       string `json:"event"`
	RunnerName  string `json:"runnerName"`
	Slot        int    `json:"slot"`
	QueuedAt    int64  `json:"queuedAt"`
	StartedAt   int64  `json:"startedAt"`
	FinishedAt  int64  `json:"finishedAt"`
	Result      string `json:"result"`
	// WorkflowFullRef is the full workflow reference GitHub sent with the job,
	// e.g. "owner/repo/.github/workflows/ci.yml@refs/heads/main" (empty for
	// jobs recorded before it was stored). WorkflowRef keeps the file name.
	WorkflowFullRef string `json:"workflowFullRef"`
	// Ref is the git ref of WorkflowFullRef: "refs/heads/main",
	// "refs/pull/42/merge", "refs/tags/v1"… Derived, not stored.
	Ref string `json:"ref"`
	// Usage is what the job used (nil: not measured).
	Usage *JobUsage `json:"usage"`
}

// WorkflowRefParts splits a full workflow reference into the workflow file
// path ("owner/repo/.github/workflows/ci.yml") and the git ref ("refs/heads/main").
func WorkflowRefParts(full string) (path, ref string) {
	// Repository and workflow paths never contain "@", branch names may.
	if i := strings.Index(full, "@"); i >= 0 {
		return full[:i], full[i+1:]
	}
	return full, ""
}

// UpsertJob inserts or updates a job keyed by its GitHub job id. Zero values
// never overwrite existing data.
func (s *Store) UpsertJob(j Job) error {
	_, err := s.db.Exec(`INSERT INTO jobs (target_id, job_id, run_id, owner, repo, workflow_ref, job_name, event, runner_name, slot, queued_at, started_at, finished_at, result, workflow_full_ref)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(job_id) DO UPDATE SET
			run_id = CASE WHEN excluded.run_id != 0 THEN excluded.run_id ELSE run_id END,
			workflow_ref = CASE WHEN excluded.workflow_ref != '' THEN excluded.workflow_ref ELSE workflow_ref END,
			workflow_full_ref = CASE WHEN excluded.workflow_full_ref != '' THEN excluded.workflow_full_ref ELSE workflow_full_ref END,
			job_name = CASE WHEN excluded.job_name != '' THEN excluded.job_name ELSE job_name END,
			runner_name = CASE WHEN excluded.runner_name != '' THEN excluded.runner_name ELSE runner_name END,
			slot = CASE WHEN excluded.slot >= 0 THEN excluded.slot ELSE slot END,
			queued_at = CASE WHEN excluded.queued_at = 0 THEN queued_at WHEN queued_at = 0 THEN excluded.queued_at ELSE MIN(queued_at, excluded.queued_at) END,
			started_at = CASE WHEN excluded.started_at != 0 THEN excluded.started_at ELSE started_at END,
			finished_at = CASE WHEN excluded.finished_at != 0 THEN excluded.finished_at ELSE finished_at END,
			result = CASE WHEN excluded.result != '' THEN excluded.result ELSE result END`,
		j.TargetID, j.JobID, j.RunID, j.Owner, j.Repo, j.WorkflowRef, j.JobName, j.Event, j.RunnerName, j.Slot, j.QueuedAt, j.StartedAt, j.FinishedAt, j.Result, j.WorkflowFullRef)
	return err
}

const queryJobByRunner = jobColumns + ` WHERE runner_name = ? ORDER BY id DESC LIMIT 1`

// JobByRunner returns the job a runner picked up.
func (s *Store) JobByRunner(ctx context.Context, runner string) (Job, error) {
	var j Job
	var u usageScan
	err := s.db.QueryRowContext(ctx, queryJobByRunner, runner).
		Scan(append(j.dest(), u.dest()...)...)
	_, j.Ref = WorkflowRefParts(j.WorkflowFullRef)
	j.Usage = u.usage()
	return j, err
}

const queryInFlightJobs = `SELECT target_id, job_id, runner_name, started_at FROM jobs j
	WHERE finished_at = 0 AND started_at > 0 AND runner_name != ''
	AND EXISTS (SELECT 1 FROM runners r WHERE r.name = j.runner_name AND r.target_id = j.target_id)`

// InFlightJobs returns the jobs that started and have not finished on a
// runner that is still recorded as registered on GitHub. Used at startup to
// find the jobs of runners that died while Pitlane was stopped.
func (s *Store) InFlightJobs(ctx context.Context) ([]Job, error) {
	rows, err := s.db.QueryContext(ctx, queryInFlightJobs)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Job
	for rows.Next() {
		var j Job
		if err := rows.Scan(&j.TargetID, &j.JobID, &j.RunnerName, &j.StartedAt); err != nil {
			return nil, err
		}
		out = append(out, j)
	}
	return out, rows.Err()
}

// Jobs returns the most recent jobs.
func (s *Store) Jobs(ctx context.Context, limit int) ([]Job, error) {
	jobs, _, err := s.JobsPage(ctx, JobFilter{}, limit, 0)
	return jobs, err
}

const jobColumns = `SELECT id, target_id, job_id, run_id, owner, repo, workflow_ref, job_name, event, runner_name, slot, queued_at, started_at, finished_at, result, workflow_full_ref, ` + usageColumns + ` FROM jobs`

// dest returns the scan destinations of jobColumns before the usage ones.
func (j *Job) dest() []any {
	return []any{&j.ID, &j.TargetID, &j.JobID, &j.RunID, &j.Owner, &j.Repo, &j.WorkflowRef, &j.JobName, &j.Event, &j.RunnerName, &j.Slot, &j.QueuedAt, &j.StartedAt, &j.FinishedAt, &j.Result, &j.WorkflowFullRef}
}

// JobFilter narrows a job listing. Empty fields match everything.
type JobFilter struct {
	Owner, Repo string // repository (both set) or nothing
	Result      string // "running" (not finished yet) or a result, case-insensitive
	Ref         string // substring of the branch, tag or pull request ref, case-insensitive
}

func (f JobFilter) where() (string, []any) {
	var conds []string
	var args []any
	if f.Owner != "" && f.Repo != "" {
		conds = append(conds, "owner = ? COLLATE NOCASE AND repo = ? COLLATE NOCASE")
		args = append(args, f.Owner, f.Repo)
	}
	switch r := strings.ToLower(strings.TrimSpace(f.Result)); r {
	case "":
	case "running":
		conds = append(conds, "finished_at = 0")
	default:
		conds = append(conds, "finished_at != 0 AND lower(result) = ?")
		args = append(args, r)
	}
	if ref := strings.ToLower(strings.TrimSpace(f.Ref)); ref != "" {
		// Only the part after "@" (refs/heads/main, refs/pull/12/merge…).
		conds = append(conds, "instr(workflow_full_ref, '@') > 0 AND instr(lower(substr(workflow_full_ref, instr(workflow_full_ref, '@') + 1)), ?) > 0")
		args = append(args, ref)
	}
	if len(conds) == 0 {
		return "", nil
	}
	return " WHERE " + strings.Join(conds, " AND "), args
}

// queries returns the count and page queries of a job listing, and their
// arguments (the page query takes LIMIT and OFFSET after them).
func (f JobFilter) queries() (count, page string, args []any) {
	where, args := f.where()
	// where is built from constant fragments only; values are bound parameters.
	return `SELECT count(*) FROM jobs` + where,
		jobColumns + where + ` ORDER BY COALESCE(NULLIF(queued_at, 0), started_at) DESC, id DESC LIMIT ? OFFSET ?`, //nolint:gosec // G202: constant fragments, bound values
		args
}

// JobsPage returns one page of the most recent jobs matching f, and how many
// jobs match in total.
func (s *Store) JobsPage(ctx context.Context, f JobFilter, limit, offset int) ([]Job, int, error) {
	count, page, args := f.queries()
	var total int
	if err := s.db.QueryRowContext(ctx, count, args...).Scan(&total); err != nil {
		return nil, 0, err
	}
	rows, err := s.db.QueryContext(ctx, page, append(args, limit, offset)...)
	if err != nil {
		return nil, 0, err
	}
	jobs, err := scanJobs(rows)
	return jobs, total, err
}

// JobRepos lists the repositories ("owner/repo") that have jobs.
func (s *Store) JobRepos(ctx context.Context) ([]string, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT DISTINCT owner || '/' || repo FROM jobs WHERE owner != '' AND repo != '' ORDER BY 1 COLLATE NOCASE`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []string{}
	for rows.Next() {
		var r string
		if err := rows.Scan(&r); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

func scanJobs(rows *sql.Rows) ([]Job, error) {
	defer rows.Close()
	var out []Job
	for rows.Next() {
		var j Job
		var u usageScan
		if err := rows.Scan(append(j.dest(), u.dest()...)...); err != nil {
			return nil, err
		}
		_, j.Ref = WorkflowRefParts(j.WorkflowFullRef)
		j.Usage = u.usage()
		out = append(out, j)
	}
	return out, rows.Err()
}

// PruneJobs deletes jobs older than the given age.
func (s *Store) PruneJobs(olderThan time.Duration) error {
	// A job's latest known date decides; a job without any date is kept
	// (queued_at alone was 0 when GitHub sent no queue time, which deleted
	// the whole recent history on every pass).
	_, err := s.db.Exec(`DELETE FROM jobs WHERE MAX(queued_at, started_at, finished_at) > 0
		AND MAX(queued_at, started_at, finished_at) < ?`, time.Now().Add(-olderThan).Unix())
	return err
}
