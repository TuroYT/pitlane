// Package store persists Pitlane's configuration, targets, job history and
// sessions in a single SQLite database. Secrets are encrypted at rest with a
// master key kept next to the database.
package store

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
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
}

// Open opens (and migrates) the database stored in dataDir.
func Open(dataDir string) (*Store, error) {
	if err := os.MkdirAll(dataDir, 0o700); err != nil {
		return nil, err
	}
	if err := restrictDataDir(dataDir); err != nil {
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
		_ = db.Close()
		return nil, fmt.Errorf("migrate: %w", err)
	}
	// SQLite creates its files with the process umask.
	for _, f := range []string{"pitlane.db", "pitlane.db-wal", "pitlane.db-shm"} {
		if err := os.Chmod(filepath.Join(dataDir, f), 0o600); err != nil && !errors.Is(err, os.ErrNotExist) {
			_ = db.Close()
			return nil, fmt.Errorf("restrict %s: %w", f, err)
		}
	}
	return s, nil
}

// Seams for tests.
var (
	chmod   = os.Chmod
	dirInfo = func(path string) (uid int, mode os.FileMode, err error) {
		info, err := os.Stat(path)
		if err != nil {
			return 0, 0, err
		}
		st, ok := info.Sys().(*syscall.Stat_t)
		if !ok {
			return 0, 0, errors.New("no owner information for " + path)
		}
		return int(st.Uid), info.Mode(), nil
	}
)

// restrictDataDir makes the data directory private to its owner: it holds
// the master key, the database and job logs, and MkdirAll keeps the mode of
// an existing directory. A directory Pitlane does not own (a volume or bind
// mount prepared by someone else, Pitlane running as a non-root user with
// group access) cannot be chmod'ed: that is only logged, since every file
// Pitlane writes in it is itself created 0600 (and the database files are
// checked below), as long as nobody else can write to it (plant or replace
// files). Anything else fails.
func restrictDataDir(dataDir string) error {
	err := chmod(dataDir, 0o700) //nolint:gosec // G302: a directory needs the x bit
	if err == nil {
		return nil
	}
	uid, mode, serr := dirInfo(dataDir)
	switch {
	case serr != nil:
		return fmt.Errorf("restrict data directory: %w (and cannot check its owner: %v)", err, serr)
	case uid == os.Geteuid():
		return fmt.Errorf("restrict data directory: %w", err)
	case mode.Perm()&0o022 != 0:
		return fmt.Errorf("data directory %s belongs to another user (uid %d) and is writable by its group or others (%v): "+
			"make it owned by the user running Pitlane, or at least not group/world-writable", dataDir, uid, mode.Perm())
	}
	slog.Warn("cannot restrict the data directory to its owner: it belongs to another user, its files stay private (0600)",
		"dir", dataDir, "owner", uid, "mode", mode.Perm().String(), "error", err)
	return nil
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
	// StrictRequired says why the target must run strict (a public
	// repository, a runner group open to public repositories); "" when it
	// need not, or as long as GitHub could not be asked. Strict cannot be
	// turned off while it is set.
	StrictRequired string `json:"strictRequired,omitempty"`
	// StrictForcedAt is when Pitlane switched the target to strict on its
	// own (Unix time, 0 = never).
	StrictForcedAt int64 `json:"strictForcedAt,omitempty"`
}

// StrictPublicRepo is the strict requirement of a public repository target.
const StrictPublicRepo = "public repository"

// StrictPublicRunnerGroup is the strict requirement of an organisation
// target whose runner group accepts jobs of public repositories.
const StrictPublicRunnerGroup = "its runner group allows public repositories"

// DefaultMaxJobMinutes is GitHub's own job time limit on hosted runners.
const DefaultMaxJobMinutes = 360

// DefaultMinRunners is the number of warm runners of a new target: one idle
// runner kept registered so a job starts in about a second. Existing targets
// keep their own value.
const DefaultMinRunners = 1

// DefaultStrictMinRunners is the number of warm runners of a new strict
// target: none, so a public repository has no idle runner registered and
// waiting for a pull request from a fork.
const DefaultStrictMinRunners = 0

// DefaultWarmRunners returns the warm runner count of a new target.
func DefaultWarmRunners(strict bool) int {
	if strict {
		return DefaultStrictMinRunners
	}
	return DefaultMinRunners
}

// FullName returns "owner/repo" or "owner".
func (t Target) FullName() string {
	if t.Kind == "repo" {
		return t.Owner + "/" + t.Repo
	}
	return t.Owner
}

// URL returns the GitHub URL the scale set is registered against.
func (t Target) URL() string { return "https://github.com/" + t.FullName() }

const targetCols = `id, kind, owner, repo, installation_id, enabled, min_runners, max_runners, scale_set_name, scale_set_id, strict, max_job_minutes, strict_required, strict_forced_at`

func scanTarget(sc interface{ Scan(...any) error }) (Target, error) {
	var t Target
	var enabled, strict int
	err := sc.Scan(&t.ID, &t.Kind, &t.Owner, &t.Repo, &t.InstallationID, &enabled, &t.MinRunners, &t.MaxRunners, &t.ScaleSetName, &t.ScaleSetID, &strict, &t.MaxJobMinutes, &t.StrictRequired, &t.StrictForcedAt)
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
	res, err := s.db.Exec(`INSERT INTO targets (kind, owner, repo, installation_id, enabled, min_runners, max_runners, scale_set_name, strict, max_job_minutes, strict_required, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		t.Kind, t.Owner, t.Repo, t.InstallationID, boolInt(t.Enabled), t.MinRunners, t.MaxRunners, t.ScaleSetName, boolInt(t.Strict), t.MaxJobMinutes, t.StrictRequired, time.Now().Unix())
	if err != nil {
		return t, err
	}
	t.ID, _ = res.LastInsertId()
	return t, nil
}

// SetScaleSetID records the scale set of a target, leaving its other fields
// alone (they may have changed since the caller read the target).
func (s *Store) SetScaleSetID(id int64, scaleSetID int) error {
	_, err := s.db.Exec(`UPDATE targets SET scale_set_id = ? WHERE id = ?`, scaleSetID, id)
	return err
}

// SetStrictRequirement records why a target must run strict ("" = it need
// not). A requirement switches a non-strict target to strict, which is
// reported (flipped) and timestamped; clearing it never turns strict off.
func (s *Store) SetStrictRequirement(id int64, reason string) (flipped bool, err error) {
	tx, err := s.db.Begin()
	if err != nil {
		return false, err
	}
	defer func() { _ = tx.Rollback() }() // no-op after Commit
	if reason != "" {
		res, err := tx.Exec(`UPDATE targets SET strict = 1, strict_forced_at = ? WHERE id = ? AND strict = 0`, time.Now().Unix(), id)
		if err != nil {
			return false, err
		}
		n, _ := res.RowsAffected()
		flipped = n > 0
	}
	if _, err := tx.Exec(`UPDATE targets SET strict_required = ? WHERE id = ?`, reason, id); err != nil {
		return false, err
	}
	return flipped, tx.Commit()
}

// UpdateTarget saves the mutable fields of a target (not the strict
// requirement, see SetStrictRequirement).
func (s *Store) UpdateTarget(t Target) error {
	// Strict cannot be written off while it is required, whatever the caller
	// read before (the requirement may have been set since).
	_, err := s.db.Exec(`UPDATE targets SET enabled = ?, min_runners = ?, max_runners = ?, scale_set_name = ?, scale_set_id = ?, installation_id = ?,
		strict = CASE WHEN strict_required != '' THEN 1 ELSE ? END, max_job_minutes = ? WHERE id = ?`,
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
	// Usage is what the job used (nil: not measured).
	Usage *JobUsage `json:"usage"`
}

// JobUsage is what a job used, measured from the cgroups of its runner
// container and of its slot over the runner's lifetime.
type JobUsage struct {
	WallMs int64 `json:"wallMs"`
	// MemPeak is the runner's peak plus the slot's (bytes): an upper bound
	// of the memory the job needed (both peaks need not coincide). Nil
	// when neither is known. Derived, not stored.
	MemPeak *int64         `json:"memPeak"`
	Runner  ContainerUsage `json:"runner"`
	Slot    ContainerUsage `json:"slot"`
}

// ContainerUsage is what one container used during a job. Nil: unknown.
type ContainerUsage struct {
	MemPeak     *int64 `json:"memPeak"`     // bytes, page cache included
	CPUUsec     *int64 `json:"cpuUsec"`     // CPU time
	CPUSomeUsec *int64 `json:"cpuSomeUsec"` // cpu.pressure "some" stall time
	IOFullUsec  *int64 `json:"ioFullUsec"`  // io.pressure "full" stall time
	MemFullUsec *int64 `json:"memFullUsec"` // memory.pressure "full" stall time
	AnonPeak    *int64 `json:"anonPeak"`    // bytes, highest anonymous memory (slot only)
	// Stall times as a percentage of the wall time. Derived, not stored.
	CPUPressure *float64 `json:"cpuPressurePct"`
	IOPressure  *float64 `json:"ioPressurePct"`
	MemPressure *float64 `json:"memPressurePct"`
}

func (c *ContainerUsage) derive(wallMs int64) {
	pct := func(v *int64) *float64 {
		if v == nil || wallMs <= 0 {
			return nil
		}
		p := min(100, float64(*v)/float64(wallMs*1000)*100)
		return &p
	}
	c.CPUPressure, c.IOPressure, c.MemPressure = pct(c.CPUSomeUsec), pct(c.IOFullUsec), pct(c.MemFullUsec)
}

func (u *JobUsage) derive() {
	u.Runner.derive(u.WallMs)
	u.Slot.derive(u.WallMs)
	u.MemPeak = nil
	if u.Runner.MemPeak != nil || u.Slot.MemPeak != nil {
		var n int64
		for _, v := range []*int64{u.Runner.MemPeak, u.Slot.MemPeak} {
			if v != nil {
				n += *v
			}
		}
		u.MemPeak = &n
	}
}

// usageColumns are the stored fields of a JobUsage, in column order.
const usageColumns = `usage_wall_ms, runner_mem_peak, runner_cpu_usec, runner_cpu_some_usec, runner_io_full_usec, runner_mem_full_usec,
	slot_mem_peak, slot_cpu_usec, slot_cpu_some_usec, slot_io_full_usec, slot_mem_full_usec, slot_anon_peak`

// usageScan holds the nullable usage columns of a row.
type usageScan [12]sql.NullInt64

func (u *usageScan) dest() []any {
	out := make([]any, len(u))
	for i := range u {
		out[i] = &u[i]
	}
	return out
}

func (u *usageScan) usage() *JobUsage {
	if !u[0].Valid {
		return nil
	}
	v := func(i int) *int64 {
		if !u[i].Valid {
			return nil
		}
		n := u[i].Int64
		return &n
	}
	j := &JobUsage{WallMs: u[0].Int64,
		Runner: ContainerUsage{MemPeak: v(1), CPUUsec: v(2), CPUSomeUsec: v(3), IOFullUsec: v(4), MemFullUsec: v(5)},
		Slot:   ContainerUsage{MemPeak: v(6), CPUUsec: v(7), CPUSomeUsec: v(8), IOFullUsec: v(9), MemFullUsec: v(10), AnonPeak: v(11)}}
	j.derive()
	return j
}

const querySetJobUsage = `UPDATE jobs SET usage_wall_ms = ?,
	runner_mem_peak = ?, runner_cpu_usec = ?, runner_cpu_some_usec = ?, runner_io_full_usec = ?, runner_mem_full_usec = ?,
	slot_mem_peak = ?, slot_cpu_usec = ?, slot_cpu_some_usec = ?, slot_io_full_usec = ?, slot_mem_full_usec = ?, slot_anon_peak = ?
	WHERE id = (SELECT id FROM jobs WHERE runner_name = ? ORDER BY id DESC LIMIT 1)`

// SetJobUsage records what the job a runner picked up used. It returns
// false when no job with that runner is recorded (yet).
func (s *Store) SetJobUsage(ctx context.Context, runner string, u JobUsage) (bool, error) {
	n := func(v *int64) any {
		if v == nil {
			return nil
		}
		return *v
	}
	res, err := s.db.ExecContext(ctx, querySetJobUsage,
		u.WallMs, n(u.Runner.MemPeak), n(u.Runner.CPUUsec), n(u.Runner.CPUSomeUsec), n(u.Runner.IOFullUsec), n(u.Runner.MemFullUsec),
		n(u.Slot.MemPeak), n(u.Slot.CPUUsec), n(u.Slot.CPUSomeUsec), n(u.Slot.IOFullUsec), n(u.Slot.MemFullUsec), n(u.Slot.AnonPeak), runner)
	if err != nil {
		return false, err
	}
	rows, err := res.RowsAffected()
	return rows > 0, err
}

const queryJobMemoryPeaks = `SELECT COALESCE(runner_mem_peak, 0) + COALESCE(slot_mem_peak, 0) FROM jobs
	WHERE finished_at >= ? AND (runner_mem_peak IS NOT NULL OR slot_mem_peak IS NOT NULL)`

// JobMemoryPeaks returns the memory peak (runner plus slot, bytes) of the
// measured jobs finished since the given Unix time.
func (s *Store) JobMemoryPeaks(ctx context.Context, since int64) ([]float64, error) {
	rows, err := s.db.QueryContext(ctx, queryJobMemoryPeaks, since)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []float64{}
	for rows.Next() {
		var v int64
		if err := rows.Scan(&v); err != nil {
			return nil, err
		}
		out = append(out, float64(v))
	}
	return out, rows.Err()
}

// JobMemory is the memory a finished job needed, as the admission predicts
// it: the runner's peak plus the slot's anonymous peak (a quarter of the
// slot's memory.peak for jobs measured before anon was recorded: most of
// it is page cache).
const jobMemoryExpr = `COALESCE(runner_mem_peak, 0) + COALESCE(slot_anon_peak, slot_mem_peak / 4, 0)`

// MemoryHistory returns the memory (see jobMemoryExpr) of the last limit
// measured jobs, newest first, that were not cancelled: of one job (owner,
// repository, workflow file and job name) when workflow and job are set,
// of a repository when only owner and repo are, of every job otherwise.
func (s *Store) MemoryHistory(ctx context.Context, owner, repo, workflow, job string, limit int) ([]int64, error) {
	where, args := `(runner_mem_peak IS NOT NULL OR slot_mem_peak IS NOT NULL) AND finished_at > 0 AND result != 'canceled'`, []any{}
	switch {
	case workflow != "" || job != "":
		where += ` AND owner = ? AND repo = ? AND workflow_ref = ? AND job_name = ?`
		args = append(args, owner, repo, workflow, job)
	case repo != "":
		where += ` AND owner = ? AND repo = ?`
		args = append(args, owner, repo)
	}
	args = append(args, limit)
	rows, err := s.db.QueryContext(ctx, `SELECT `+jobMemoryExpr+` FROM jobs WHERE `+where+` ORDER BY id DESC LIMIT ?`, args...) //nolint:gosec // G202: fixed SQL fragments only
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []int64
	for rows.Next() {
		var v int64
		if err := rows.Scan(&v); err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, rows.Err()
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
		jobColumns + where + ` ORDER BY queued_at DESC, id DESC LIMIT ? OFFSET ?`, //nolint:gosec // G202: constant fragments, bound values
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
	_, err := s.db.Exec(`DELETE FROM jobs WHERE queued_at < ?`, time.Now().Add(-olderThan).Unix())
	return err
}

// ---------------------------------------------------------------- sessions

// sessionKey is what the database keeps of a session token: its SHA-256, so
// a leaked database (or backup) does not leak usable sessions. Tokens are
// random, a plain hash is enough.
func sessionKey(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

// CreateSession stores a session token for login. memberships are the
// access rules ("org" or "org/team") the user matched at sign-in.
func (s *Store) CreateSession(token, login string, ttl time.Duration, memberships ...string) error {
	_, err := s.db.Exec(`INSERT INTO sessions (token, login, expires_at, memberships) VALUES (?, ?, ?, ?)`,
		sessionKey(token), login, time.Now().Add(ttl).Unix(), strings.Join(memberships, ","))
	return err
}

// Session is a valid sign-in.
type Session struct {
	Login       string
	Memberships []string
}

// Session returns a valid session.
func (s *Store) Session(token string) (Session, bool) {
	if token == "" {
		return Session{}, false
	}
	key := sessionKey(token)
	var login, memberships string
	var exp int64
	if err := s.db.QueryRow(`SELECT login, expires_at, memberships FROM sessions WHERE token = ?`, key).Scan(&login, &exp, &memberships); err != nil {
		return Session{}, false
	}
	if time.Now().Unix() > exp {
		_, _ = s.db.Exec(`DELETE FROM sessions WHERE token = ?`, key)
		return Session{}, false
	}
	out := Session{Login: login}
	if memberships != "" {
		out.Memberships = strings.Split(memberships, ",")
	}
	return out, true
}

// SessionLogin returns the login of a valid session.
func (s *Store) SessionLogin(token string) (string, bool) {
	sess, ok := s.Session(token)
	return sess.Login, ok
}

// DeleteSession removes a session.
func (s *Store) DeleteSession(token string) error {
	_, err := s.db.Exec(`DELETE FROM sessions WHERE token = ?`, sessionKey(token))
	return err
}

// PruneSessions removes expired sessions.
func (s *Store) PruneSessions() error {
	_, err := s.db.Exec(`DELETE FROM sessions WHERE expires_at < ?`, time.Now().Unix())
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

const queryJobTimings = `SELECT queued_at, started_at, finished_at FROM jobs WHERE started_at >= ? OR finished_at >= ?`

// JobTimings returns, in seconds, the run time of jobs finished since the
// given Unix time and the queue time of jobs started since then.
func (s *Store) JobTimings(ctx context.Context, since int64) (durations, waits []float64, err error) {
	rows, err := s.db.QueryContext(ctx, queryJobTimings, since, since)
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
