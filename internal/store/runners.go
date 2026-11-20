package store

import (
	"time"
)

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
