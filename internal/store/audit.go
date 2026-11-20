package store

import (
	"context"
	"time"
)

// AuditEntry is one admin action.
type AuditEntry struct {
	ID      int64  `json:"id"`
	At      int64  `json:"at"`
	Actor   string `json:"actor"`
	Action  string `json:"action"`  // e.g. "target.update", "user.remove"
	Subject string `json:"subject"` // what it acted on, e.g. "acme/api"
	Detail  string `json:"detail"`  // what changed (never a secret)
}

// AddAudit records an admin action (At defaults to now).
func (s *Store) AddAudit(e AuditEntry) error {
	if e.At == 0 {
		e.At = time.Now().Unix()
	}
	_, err := s.db.Exec(`INSERT INTO audit (at, actor, action, subject, detail) VALUES (?, ?, ?, ?, ?)`, e.At, e.Actor, e.Action, e.Subject, e.Detail)
	return err
}

// AuditPage returns a page of the audit log, latest first, and the total
// number of entries.
func (s *Store) AuditPage(ctx context.Context, limit, offset int) ([]AuditEntry, int, error) {
	var total int
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM audit`).Scan(&total); err != nil {
		return nil, 0, err
	}
	rows, err := s.db.QueryContext(ctx, `SELECT id, at, actor, action, subject, detail FROM audit ORDER BY at DESC, id DESC LIMIT ? OFFSET ?`, limit, offset)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()
	out := []AuditEntry{}
	for rows.Next() {
		var e AuditEntry
		if err := rows.Scan(&e.ID, &e.At, &e.Actor, &e.Action, &e.Subject, &e.Detail); err != nil {
			return nil, 0, err
		}
		out = append(out, e)
	}
	return out, total, rows.Err()
}

// PruneAudit deletes audit entries older than the given age.
func (s *Store) PruneAudit(olderThan time.Duration) error {
	_, err := s.db.Exec(`DELETE FROM audit WHERE at < ?`, time.Now().Add(-olderThan).Unix())
	return err
}
