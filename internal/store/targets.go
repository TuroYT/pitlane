package store

import (
	"context"
	"database/sql"
	"time"
)

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

// kindRepo is the Kind of a repository target (the other one is "org").
const kindRepo = "repo"

// Defaults of a new target, the same as the column defaults of the targets
// table.
const (
	defaultScaleSetName = "pitlane"
	defaultMaxRunners   = 4
)

// FullName returns "owner/repo" or "owner".
func (t Target) FullName() string {
	if t.Kind == kindRepo {
		return t.Owner + "/" + t.Repo
	}
	return t.Owner
}

// URL returns the GitHub URL the scale set is registered against.
func (t Target) URL() string { return "https://github.com/" + t.FullName() }

const targetCols = `id, kind, owner, repo, installation_id, enabled, min_runners, max_runners, scale_set_name, scale_set_id, strict, max_job_minutes, strict_required, strict_forced_at`

// scanTarget reads the targetCols of a row.
func scanTarget(sc rowScanner) (Target, error) {
	var t Target
	var enabled, strict int
	err := sc.Scan(&t.ID, &t.Kind, &t.Owner, &t.Repo, &t.InstallationID, &enabled, &t.MinRunners, &t.MaxRunners, &t.ScaleSetName, &t.ScaleSetID, &strict, &t.MaxJobMinutes, &t.StrictRequired, &t.StrictForcedAt)
	t.Enabled, t.Strict = enabled == 1, strict == 1
	return t, err
}

// Targets lists all targets.
func (s *Store) Targets() ([]Target, error) {
	return queryRows(context.Background(), s.db, nil, scanTarget, `SELECT `+targetCols+` FROM targets ORDER BY owner, repo`)
}

// Target fetches one target.
func (s *Store) Target(id int64) (Target, error) {
	return scanTarget(s.db.QueryRow(`SELECT `+targetCols+` FROM targets WHERE id = ?`, id))
}

const queryCreateTarget = `INSERT INTO targets (kind, owner, repo, installation_id, enabled, min_runners, max_runners, scale_set_name, strict, max_job_minutes, strict_required, created_at)
	VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`

// CreateTarget inserts a target and returns it.
func (s *Store) CreateTarget(t Target) (Target, error) {
	if t.ScaleSetName == "" {
		t.ScaleSetName = defaultScaleSetName
	}
	if t.MaxRunners <= 0 {
		t.MaxRunners = defaultMaxRunners
	}
	t.MaxJobMinutes = max(0, t.MaxJobMinutes)
	res, err := s.db.Exec(queryCreateTarget,
		t.Kind, t.Owner, t.Repo, t.InstallationID, boolInt(t.Enabled), t.MinRunners, t.MaxRunners, t.ScaleSetName, boolInt(t.Strict), t.MaxJobMinutes, t.StrictRequired, time.Now().Unix())
	if err != nil {
		return t, err
	}
	t.ID, err = res.LastInsertId()
	return t, err
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
	ctx := context.Background()
	err = s.withTx(ctx, func(tx *sql.Tx) error {
		if reason != "" {
			if flipped, err = forceStrict(ctx, tx, id); err != nil {
				return err
			}
		}
		_, err := tx.ExecContext(ctx, `UPDATE targets SET strict_required = ? WHERE id = ?`, reason, id)
		return err
	})
	if err != nil {
		return false, err
	}
	return flipped, nil
}

// forceStrict switches a target to strict and timestamps it, reporting
// whether it was not strict yet.
func forceStrict(ctx context.Context, tx *sql.Tx, id int64) (bool, error) {
	res, err := tx.ExecContext(ctx, `UPDATE targets SET strict = 1, strict_forced_at = ? WHERE id = ? AND strict = 0`, time.Now().Unix(), id)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n > 0, err
}

// queryUpdateTarget never writes strict off while it is required, whatever
// the caller read before (the requirement may have been set since).
const queryUpdateTarget = `UPDATE targets SET enabled = ?, min_runners = ?, max_runners = ?, scale_set_name = ?, scale_set_id = ?, installation_id = ?,
	strict = CASE WHEN strict_required != '' THEN 1 ELSE ? END, max_job_minutes = ? WHERE id = ?`

// UpdateTarget saves the mutable fields of a target (not the strict
// requirement, see SetStrictRequirement).
func (s *Store) UpdateTarget(t Target) error {
	_, err := s.db.Exec(queryUpdateTarget,
		boolInt(t.Enabled), t.MinRunners, t.MaxRunners, t.ScaleSetName, t.ScaleSetID, t.InstallationID, boolInt(t.Strict), max(0, t.MaxJobMinutes), t.ID)
	return err
}

// DeleteTarget removes a target.
func (s *Store) DeleteTarget(id int64) error {
	_, err := s.db.Exec(`DELETE FROM targets WHERE id = ?`, id)
	return err
}

// boolInt is how SQLite stores a boolean.
func boolInt(b bool) int {
	if b {
		return 1
	}
	return 0
}
