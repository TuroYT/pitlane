package store

import (
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
