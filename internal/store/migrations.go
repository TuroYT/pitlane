package store

import (
	"fmt"
)

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
	// Sessions now store the SHA-256 of their token: sessions stored with a
	// plaintext token are dropped (their users sign in again).
	`DELETE FROM sessions`,
	// Why the target must run strict ("" = not required or not known yet),
	// and when Pitlane switched it to strict on its own (0 = never).
	`ALTER TABLE targets ADD COLUMN strict_required TEXT NOT NULL DEFAULT ''`,
	`ALTER TABLE targets ADD COLUMN strict_forced_at INTEGER NOT NULL DEFAULT 0`,
	// Full workflow reference of a job ("owner/repo/.github/workflows/ci.yml@refs/heads/main"),
	// which keeps the branch, tag or pull request the job ran for.
	`ALTER TABLE jobs ADD COLUMN workflow_full_ref TEXT NOT NULL DEFAULT ''`,
	// Resources a job used, from the cgroups of its runner container and of
	// its slot (NULL: not measured). Wall time in milliseconds, memory peaks
	// in bytes, CPU time and pressure stall totals in microseconds.
	`ALTER TABLE jobs ADD COLUMN usage_wall_ms INTEGER`,
	`ALTER TABLE jobs ADD COLUMN runner_mem_peak INTEGER`,
	`ALTER TABLE jobs ADD COLUMN runner_cpu_usec INTEGER`,
	`ALTER TABLE jobs ADD COLUMN runner_cpu_some_usec INTEGER`,
	`ALTER TABLE jobs ADD COLUMN runner_io_full_usec INTEGER`,
	`ALTER TABLE jobs ADD COLUMN runner_mem_full_usec INTEGER`,
	`ALTER TABLE jobs ADD COLUMN slot_mem_peak INTEGER`,
	`ALTER TABLE jobs ADD COLUMN slot_cpu_usec INTEGER`,
	`ALTER TABLE jobs ADD COLUMN slot_cpu_some_usec INTEGER`,
	`ALTER TABLE jobs ADD COLUMN slot_io_full_usec INTEGER`,
	`ALTER TABLE jobs ADD COLUMN slot_mem_full_usec INTEGER`,
	// Users and roles (users.go). Logins compare case-insensitively, like
	// on GitHub.
	`CREATE TABLE users (
		login      TEXT PRIMARY KEY COLLATE NOCASE,
		role       TEXT NOT NULL,
		created_at INTEGER NOT NULL DEFAULT 0,
		created_by TEXT NOT NULL DEFAULT ''
	)`,
	// Every login of the former allow list (the allowed_users setting,
	// comma-separated) becomes an admin: everyone keeps the rights they had.
	`WITH RECURSIVE split(login, rest) AS (
		SELECT '', COALESCE((SELECT value FROM settings WHERE key = 'allowed_users'), '') || ','
		UNION ALL
		SELECT trim(substr(rest, 1, instr(rest, ',') - 1)), substr(rest, instr(rest, ',') + 1) FROM split WHERE rest <> ''
	)
	INSERT OR IGNORE INTO users (login, role, created_at, created_by)
	SELECT ltrim(login, '@'), 'admin', CAST(strftime('%s', 'now') AS INTEGER), 'migration' FROM split WHERE ltrim(login, '@') <> ''`,
	// Organization and team membership rules, checked at sign-in.
	`CREATE TABLE access_rules (
		id         INTEGER PRIMARY KEY AUTOINCREMENT,
		org        TEXT NOT NULL COLLATE NOCASE,
		team       TEXT NOT NULL DEFAULT '' COLLATE NOCASE,
		role       TEXT NOT NULL,
		created_at INTEGER NOT NULL DEFAULT 0,
		created_by TEXT NOT NULL DEFAULT '',
		UNIQUE (org, team)
	)`,
	// The rules a session's user matched at sign-in ("org" or "org/team").
	`ALTER TABLE sessions ADD COLUMN memberships TEXT NOT NULL DEFAULT ''`,
	// Admin actions.
	`CREATE TABLE audit (
		id      INTEGER PRIMARY KEY AUTOINCREMENT,
		at      INTEGER NOT NULL,
		actor   TEXT NOT NULL,
		action  TEXT NOT NULL,
		subject TEXT NOT NULL DEFAULT '',
		detail  TEXT NOT NULL DEFAULT ''
	)`,
	`CREATE INDEX audit_at ON audit(at DESC, id DESC)`,
	// Indexes for the job history (paginated, newest first, filtered by
	// repository), the runner lookups made for every job and the metrics
	// windows. jobs_recent replaces jobs_queued, which lacked the id tie-break.
	`CREATE INDEX jobs_recent ON jobs(queued_at DESC, id DESC)`,
	`DROP INDEX jobs_queued`,
	`CREATE INDEX jobs_repo ON jobs(owner COLLATE NOCASE, repo COLLATE NOCASE, queued_at DESC, id DESC)`,
	`CREATE INDEX jobs_runner ON jobs(runner_name)`,
	`CREATE INDEX jobs_started ON jobs(started_at)`,
	`CREATE INDEX jobs_finished ON jobs(finished_at)`,
	// Highest anonymous memory (memory.stat anon) of a job's slot: its
	// memory.peak also counts the page cache (image layers, build contexts).
	`ALTER TABLE jobs ADD COLUMN slot_anon_peak INTEGER`,
	// Memory history of a job, for the admission's prediction.
	`CREATE INDEX jobs_by_name ON jobs(owner, repo, workflow_ref, job_name, id DESC)`,
	// The history is ordered by the queue time, or by the start when GitHub
	// sent none (queued_at = 0), so those jobs do not sink to the bottom.
	`CREATE INDEX jobs_recent2 ON jobs(COALESCE(NULLIF(queued_at, 0), started_at) DESC, id DESC)`,
	`CREATE INDEX jobs_repo2 ON jobs(owner COLLATE NOCASE, repo COLLATE NOCASE, COALESCE(NULLIF(queued_at, 0), started_at) DESC, id DESC)`,
	`DROP INDEX jobs_recent`,
	`DROP INDEX jobs_repo`,
}

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
