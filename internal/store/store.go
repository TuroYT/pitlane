// Package store persists Pitlane's configuration, targets, job history and
// sessions in a single SQLite database. Secrets are encrypted at rest with a
// master key kept next to the database.
package store

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"database/sql"
	"encoding/base64"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	_ "modernc.org/sqlite"
)

// Store wraps the SQLite database.
type Store struct {
	db   *sql.DB
	aead cipher.AEAD
}

var migrations = []string{
	`CREATE TABLE settings (key TEXT PRIMARY KEY, value TEXT NOT NULL)`,
	`CREATE TABLE targets (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		kind TEXT NOT NULL,             -- "repo" or "org"
		owner TEXT NOT NULL,
		repo TEXT NOT NULL DEFAULT '',
		installation_id INTEGER NOT NULL DEFAULT 0,
		enabled INTEGER NOT NULL DEFAULT 1,
		min_runners INTEGER NOT NULL DEFAULT 0,
		max_runners INTEGER NOT NULL DEFAULT 4,
		scale_set_name TEXT NOT NULL DEFAULT 'pitlane',
		scale_set_id INTEGER NOT NULL DEFAULT 0,
		created_at INTEGER NOT NULL,
		UNIQUE(kind, owner, repo)
	)`,
	`CREATE TABLE jobs (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		target_id INTEGER NOT NULL,
		job_id TEXT NOT NULL UNIQUE,
		run_id INTEGER NOT NULL DEFAULT 0,
		owner TEXT NOT NULL DEFAULT '',
		repo TEXT NOT NULL DEFAULT '',
		workflow_ref TEXT NOT NULL DEFAULT '',
		job_name TEXT NOT NULL DEFAULT '',
		event TEXT NOT NULL DEFAULT '',
		runner_name TEXT NOT NULL DEFAULT '',
		slot INTEGER NOT NULL DEFAULT -1,
		queued_at INTEGER NOT NULL DEFAULT 0,
		started_at INTEGER NOT NULL DEFAULT 0,
		finished_at INTEGER NOT NULL DEFAULT 0,
		result TEXT NOT NULL DEFAULT ''
	)`,
	`CREATE INDEX jobs_queued ON jobs(queued_at DESC)`,
	`CREATE TABLE sessions (token TEXT PRIMARY KEY, login TEXT NOT NULL, expires_at INTEGER NOT NULL)`,
	`ALTER TABLE targets ADD COLUMN strict INTEGER NOT NULL DEFAULT 0`,
	`ALTER TABLE targets ADD COLUMN max_job_minutes INTEGER NOT NULL DEFAULT 360`,
	// Runners Pitlane registered on GitHub and has not seen removed yet.
	`CREATE TABLE runners (
		runner_id INTEGER PRIMARY KEY,
		target_id INTEGER NOT NULL,
		name TEXT NOT NULL,
		created_at INTEGER NOT NULL
	)`,
	// Full workflow reference of a job ("owner/repo/.github/workflows/ci.yml@refs/heads/main"),
	// which keeps the branch, tag or pull request the job ran for.
	`ALTER TABLE jobs ADD COLUMN workflow_full_ref TEXT NOT NULL DEFAULT ''`,
}

// Open opens (and migrates) the database stored in dataDir.
func Open(dataDir string) (*Store, error) {
	if err := os.MkdirAll(dataDir, 0o700); err != nil {
		return nil, err
	}
	key, err := loadOrCreateKey(filepath.Join(dataDir, "master.key"))
	if err != nil {
		return nil, fmt.Errorf("master key: %w", err)
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	dsn := "file:" + filepath.Join(dataDir, "pitlane.db") + "?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)&_pragma=foreign_keys(1)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	s := &Store{db: db, aead: aead}
	if err := s.migrate(); err != nil {
		db.Close()
		return nil, fmt.Errorf("migrate: %w", err)
	}
	return s, nil
}

// Close closes the database.
func (s *Store) Close() error { return s.db.Close() }

func (s *Store) migrate() error {
	if _, err := s.db.Exec(`CREATE TABLE IF NOT EXISTS schema_version (v INTEGER NOT NULL)`); err != nil {
		return err
	}
	var v int
	if err := s.db.QueryRow(`SELECT COALESCE(MAX(v), 0) FROM schema_version`).Scan(&v); err != nil {
		return err
	}
	for i := v; i < len(migrations); i++ {
		tx, err := s.db.Begin()
		if err != nil {
			return err
		}
		if _, err := tx.Exec(migrations[i]); err != nil {
			tx.Rollback()
			return fmt.Errorf("migration %d: %w", i+1, err)
		}
		if _, err := tx.Exec(`INSERT INTO schema_version (v) VALUES (?)`, i+1); err != nil {
			tx.Rollback()
			return err
		}
		if err := tx.Commit(); err != nil {
			return err
		}
	}
	return nil
}

func loadOrCreateKey(path string) ([]byte, error) {
	if b, err := os.ReadFile(path); err == nil {
		key, err := base64.StdEncoding.DecodeString(strings.TrimSpace(string(b)))
		if err != nil || len(key) != 32 {
			return nil, errors.New("invalid master key file " + path)
		}
		return key, nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		return nil, err
	}
	if err := os.WriteFile(path, []byte(base64.StdEncoding.EncodeToString(key)+"\n"), 0o600); err != nil {
		return nil, err
	}
	return key, nil
}

// ---------------------------------------------------------------- settings

// Get returns a setting or def when unset.
func (s *Store) Get(key, def string) string {
	var v string
	if err := s.db.QueryRow(`SELECT value FROM settings WHERE key = ?`, key).Scan(&v); err != nil {
		return def
	}
	return v
}

// GetInt returns an integer setting or def.
func (s *Store) GetInt(key string, def int) int {
	v, err := strconv.Atoi(s.Get(key, ""))
	if err != nil {
		return def
	}
	return v
}

// Set stores a setting.
func (s *Store) Set(key, value string) error {
	_, err := s.db.Exec(`INSERT INTO settings (key, value) VALUES (?, ?)
		ON CONFLICT(key) DO UPDATE SET value = excluded.value`, key, value)
	return err
}

// SetSecret encrypts and stores a setting.
func (s *Store) SetSecret(key, value string) error {
	nonce := make([]byte, s.aead.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return err
	}
	sealed := s.aead.Seal(nonce, nonce, []byte(value), []byte(key))
	return s.Set(key, "enc:"+base64.StdEncoding.EncodeToString(sealed))
}

// GetSecret decrypts a setting stored with SetSecret.
func (s *Store) GetSecret(key string) (string, error) {
	raw := s.Get(key, "")
	if raw == "" {
		return "", nil
	}
	b, err := base64.StdEncoding.DecodeString(strings.TrimPrefix(raw, "enc:"))
	if err != nil {
		return "", err
	}
	ns := s.aead.NonceSize()
	if len(b) < ns {
		return "", errors.New("ciphertext too short")
	}
	plain, err := s.aead.Open(nil, b[:ns], b[ns:], []byte(key))
	if err != nil {
		return "", fmt.Errorf("decrypt %s: %w", key, err)
	}
	return string(plain), nil
}

// ----------------------------------------------------------------- targets

// Target is a repository or organisation that Pitlane provides runners for.
type Target struct {
	ID             int64  `json:"id"`
	Kind           string `json:"kind"` // "repo" or "org"
	Owner          string `json:"owner"`
	Repo           string `json:"repo,omitempty"`
	InstallationID int64  `json:"installationId"`
	Enabled        bool   `json:"enabled"`
	MinRunners     int    `json:"minRunners"`
	MaxRunners     int    `json:"maxRunners"`
	ScaleSetName   string `json:"scaleSetName"`
	ScaleSetID     int    `json:"scaleSetId"`
	// Strict isolation: no cache shared with other jobs, slot wiped before
	// and after each job. Default for public repositories.
	Strict bool `json:"strict"`
	// A runner busy for longer is killed (0 = unlimited).
	MaxJobMinutes int `json:"maxJobMinutes"`
}

// DefaultMaxJobMinutes is GitHub's own job time limit on hosted runners.
const DefaultMaxJobMinutes = 360

// FullName returns "owner/repo" or "owner".
func (t Target) FullName() string {
	if t.Kind == "repo" {
		return t.Owner + "/" + t.Repo
	}
	return t.Owner
}

// URL returns the GitHub URL the scale set is registered against.
func (t Target) URL() string { return "https://github.com/" + t.FullName() }

const targetCols = `id, kind, owner, repo, installation_id, enabled, min_runners, max_runners, scale_set_name, scale_set_id, strict, max_job_minutes`

func scanTarget(sc interface{ Scan(...any) error }) (Target, error) {
	var t Target
	var enabled, strict int
	err := sc.Scan(&t.ID, &t.Kind, &t.Owner, &t.Repo, &t.InstallationID, &enabled, &t.MinRunners, &t.MaxRunners, &t.ScaleSetName, &t.ScaleSetID, &strict, &t.MaxJobMinutes)
	t.Enabled, t.Strict = enabled == 1, strict == 1
	return t, err
}

// Targets lists all targets.
func (s *Store) Targets() ([]Target, error) {
	rows, err := s.db.Query(`SELECT ` + targetCols + ` FROM targets ORDER BY owner, repo`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Target
	for rows.Next() {
		t, err := scanTarget(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// Target fetches one target.
func (s *Store) Target(id int64) (Target, error) {
	return scanTarget(s.db.QueryRow(`SELECT `+targetCols+` FROM targets WHERE id = ?`, id))
}

// CreateTarget inserts a target and returns it.
func (s *Store) CreateTarget(t Target) (Target, error) {
	if t.ScaleSetName == "" {
		t.ScaleSetName = "pitlane"
	}
	if t.MaxRunners <= 0 {
		t.MaxRunners = 4
	}
	t.MaxJobMinutes = max(0, t.MaxJobMinutes)
	res, err := s.db.Exec(`INSERT INTO targets (kind, owner, repo, installation_id, enabled, min_runners, max_runners, scale_set_name, strict, max_job_minutes, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		t.Kind, t.Owner, t.Repo, t.InstallationID, boolInt(t.Enabled), t.MinRunners, t.MaxRunners, t.ScaleSetName, boolInt(t.Strict), t.MaxJobMinutes, time.Now().Unix())
	if err != nil {
		return t, err
	}
	t.ID, _ = res.LastInsertId()
	return t, nil
}

// UpdateTarget saves the mutable fields of a target.
func (s *Store) UpdateTarget(t Target) error {
	_, err := s.db.Exec(`UPDATE targets SET enabled = ?, min_runners = ?, max_runners = ?, scale_set_name = ?, scale_set_id = ?, installation_id = ?, strict = ?, max_job_minutes = ? WHERE id = ?`,
		boolInt(t.Enabled), t.MinRunners, t.MaxRunners, t.ScaleSetName, t.ScaleSetID, t.InstallationID, boolInt(t.Strict), max(0, t.MaxJobMinutes), t.ID)
	return err
}

// DeleteTarget removes a target.
func (s *Store) DeleteTarget(id int64) error {
	_, err := s.db.Exec(`DELETE FROM targets WHERE id = ?`, id)
	return err
}

func boolInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

// -------------------------------------------------------------------- jobs

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
			queued_at = CASE WHEN excluded.queued_at != 0 THEN excluded.queued_at ELSE queued_at END,
			started_at = CASE WHEN excluded.started_at != 0 THEN excluded.started_at ELSE started_at END,
			finished_at = CASE WHEN excluded.finished_at != 0 THEN excluded.finished_at ELSE finished_at END,
			result = CASE WHEN excluded.result != '' THEN excluded.result ELSE result END`,
		j.TargetID, j.JobID, j.RunID, j.Owner, j.Repo, j.WorkflowRef, j.JobName, j.Event, j.RunnerName, j.Slot, j.QueuedAt, j.StartedAt, j.FinishedAt, j.Result, j.WorkflowFullRef)
	return err
}

// SetJobSlot records which slot ran the runner that picked a job.
func (s *Store) SetJobSlot(runnerName string, slot int) error {
	_, err := s.db.Exec(`UPDATE jobs SET slot = ? WHERE runner_name = ?`, slot, runnerName)
	return err
}

// JobByRunner returns the job a runner picked up.
func (s *Store) JobByRunner(ctx context.Context, runner string) (Job, error) {
	var j Job
	err := s.db.QueryRowContext(ctx, `SELECT id, target_id, job_id, run_id, owner, repo, workflow_ref, job_name, event, runner_name, slot, queued_at, started_at, finished_at, result, workflow_full_ref
		FROM jobs WHERE runner_name = ? ORDER BY id DESC LIMIT 1`, runner).
		Scan(&j.ID, &j.TargetID, &j.JobID, &j.RunID, &j.Owner, &j.Repo, &j.WorkflowRef, &j.JobName, &j.Event, &j.RunnerName, &j.Slot, &j.QueuedAt, &j.StartedAt, &j.FinishedAt, &j.Result, &j.WorkflowFullRef)
	_, j.Ref = WorkflowRefParts(j.WorkflowFullRef)
	return j, err
}

// Jobs returns the most recent jobs.
func (s *Store) Jobs(ctx context.Context, limit int) ([]Job, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id, target_id, job_id, run_id, owner, repo, workflow_ref, job_name, event, runner_name, slot, queued_at, started_at, finished_at, result, workflow_full_ref
		FROM jobs ORDER BY queued_at DESC, id DESC LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Job
	for rows.Next() {
		var j Job
		if err := rows.Scan(&j.ID, &j.TargetID, &j.JobID, &j.RunID, &j.Owner, &j.Repo, &j.WorkflowRef, &j.JobName, &j.Event, &j.RunnerName, &j.Slot, &j.QueuedAt, &j.StartedAt, &j.FinishedAt, &j.Result, &j.WorkflowFullRef); err != nil {
			return nil, err
		}
		_, j.Ref = WorkflowRefParts(j.WorkflowFullRef)
		out = append(out, j)
	}
	return out, rows.Err()
}

// PruneJobs deletes jobs older than the given age.
func (s *Store) PruneJobs(olderThan time.Duration) error {
	_, err := s.db.Exec(`DELETE FROM jobs WHERE queued_at < ?`, time.Now().Add(-olderThan).Unix())
	return err
}

// ---------------------------------------------------------------- sessions

// CreateSession stores a session token for login.
func (s *Store) CreateSession(token, login string, ttl time.Duration) error {
	_, err := s.db.Exec(`INSERT INTO sessions (token, login, expires_at) VALUES (?, ?, ?)`, token, login, time.Now().Add(ttl).Unix())
	return err
}

// SessionLogin returns the login of a valid session.
func (s *Store) SessionLogin(token string) (string, bool) {
	var login string
	var exp int64
	if err := s.db.QueryRow(`SELECT login, expires_at FROM sessions WHERE token = ?`, token).Scan(&login, &exp); err != nil {
		return "", false
	}
	if time.Now().Unix() > exp {
		s.db.Exec(`DELETE FROM sessions WHERE token = ?`, token)
		return "", false
	}
	return login, true
}

// DeleteSession removes a session.
func (s *Store) DeleteSession(token string) error {
	_, err := s.db.Exec(`DELETE FROM sessions WHERE token = ?`, token)
	return err
}

// ----------------------------------------------------------------- runners

// RegisteredRunner is a runner Pitlane registered on GitHub (JIT config).
type RegisteredRunner struct {
	ID        int64
	TargetID  int64
	Name      string
	CreatedAt int64
}

// AddRunner records a runner registered on GitHub.
func (s *Store) AddRunner(r RegisteredRunner) error {
	if r.CreatedAt == 0 {
		r.CreatedAt = time.Now().Unix()
	}
	_, err := s.db.Exec(`INSERT INTO runners (runner_id, target_id, name, created_at) VALUES (?, ?, ?, ?)
		ON CONFLICT(runner_id) DO UPDATE SET target_id = excluded.target_id, name = excluded.name`, r.ID, r.TargetID, r.Name, r.CreatedAt)
	return err
}

// DeleteRunner forgets a runner once it is gone from GitHub.
func (s *Store) DeleteRunner(id int64) error {
	_, err := s.db.Exec(`DELETE FROM runners WHERE runner_id = ?`, id)
	return err
}

// RunnerIDByName returns the GitHub ID of a registered runner (0 if unknown).
func (s *Store) RunnerIDByName(name string) int64 {
	var id int64
	_ = s.db.QueryRow(`SELECT runner_id FROM runners WHERE name = ? ORDER BY created_at DESC LIMIT 1`, name).Scan(&id)
	return id
}

// RegisteredRunners lists the runners registered for a target.
func (s *Store) RegisteredRunners(targetID int64) ([]RegisteredRunner, error) {
	rows, err := s.db.Query(`SELECT runner_id, target_id, name, created_at FROM runners WHERE target_id = ? ORDER BY created_at`, targetID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []RegisteredRunner
	for rows.Next() {
		var r RegisteredRunner
		if err := rows.Scan(&r.ID, &r.TargetID, &r.Name, &r.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// ------------------------------------------------------------------ metrics

// JobResultCounts counts the finished jobs in the history by result.
func (s *Store) JobResultCounts(ctx context.Context) (map[string]int64, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT result, COUNT(*) FROM jobs WHERE result != '' GROUP BY result`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]int64{}
	for rows.Next() {
		var r string
		var n int64
		if err := rows.Scan(&r, &n); err != nil {
			return nil, err
		}
		out[r] = n
	}
	return out, rows.Err()
}

// JobTimings returns, in seconds, the run time of jobs finished since the
// given Unix time and the queue time of jobs started since then.
func (s *Store) JobTimings(ctx context.Context, since int64) (durations, waits []float64, err error) {
	rows, err := s.db.QueryContext(ctx, `SELECT queued_at, started_at, finished_at FROM jobs WHERE started_at >= ? OR finished_at >= ?`, since, since)
	if err != nil {
		return nil, nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var q, st, f int64
		if err := rows.Scan(&q, &st, &f); err != nil {
			return nil, nil, err
		}
		if st > 0 && f >= st && f >= since {
			durations = append(durations, float64(f-st))
		}
		if q > 0 && st >= q && st >= since {
			waits = append(waits, float64(st-q))
		}
	}
	return durations, waits, rows.Err()
}
