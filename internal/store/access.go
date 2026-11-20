package store

import (
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
)

// AccessRule grants a role to the members of a GitHub organization, or of
// one of its teams (Team is the team's slug, "" for the whole
// organization).
type AccessRule struct {
	ID        int64  `json:"id"`
	Org       string `json:"org"`
	Team      string `json:"team"`
	Role      string `json:"role"`
	CreatedAt int64  `json:"createdAt"`
	CreatedBy string `json:"createdBy"`
}

// Subject is what a session records when its user matches the rule: "org"
// or "org/team", in lower case.
func (r AccessRule) Subject() string {
	if r.Team == "" {
		return strings.ToLower(r.Org)
	}
	return strings.ToLower(r.Org + "/" + r.Team)
}

// AccessRules lists the rules, by organization then team.
func (s *Store) AccessRules() ([]AccessRule, error) {
	rows, err := s.db.Query(`SELECT id, org, team, role, created_at, created_by FROM access_rules ORDER BY org COLLATE NOCASE, team COLLATE NOCASE`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []AccessRule{}
	for rows.Next() {
		var r AccessRule
		if err := rows.Scan(&r.ID, &r.Org, &r.Team, &r.Role, &r.CreatedAt, &r.CreatedBy); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// PutAccessRule adds a rule, or changes the role of the rule for the same
// organization and team.
func (s *Store) PutAccessRule(r AccessRule) (AccessRule, error) {
	if !ValidRole(r.Role) {
		return r, fmt.Errorf("invalid role %q", r.Role)
	}
	if r.CreatedAt == 0 {
		r.CreatedAt = time.Now().Unix()
	}
	err := s.db.QueryRow(`INSERT INTO access_rules (org, team, role, created_at, created_by) VALUES (?, ?, ?, ?, ?)
		ON CONFLICT(org, team) DO UPDATE SET role = excluded.role RETURNING id, org, team, created_at, created_by`,
		r.Org, r.Team, r.Role, r.CreatedAt, r.CreatedBy).Scan(&r.ID, &r.Org, &r.Team, &r.CreatedAt, &r.CreatedBy)
	return r, err
}

// DeleteAccessRule removes a rule. Sessions it granted lose that role on
// their next request.
func (s *Store) DeleteAccessRule(id int64) (AccessRule, error) {
	var r AccessRule
	err := s.db.QueryRow(`DELETE FROM access_rules WHERE id = ? RETURNING id, org, team, role, created_at, created_by`, id).
		Scan(&r.ID, &r.Org, &r.Team, &r.Role, &r.CreatedAt, &r.CreatedBy)
	if errors.Is(err, sql.ErrNoRows) {
		return r, ErrNotFound
	}
	return r, err
}
