package store

import (
	"strconv"
)

// Get returns a setting, or def when it is unset or cannot be read.
func (s *Store) Get(key, def string) string {
	var v string
	if err := s.db.QueryRow(`SELECT value FROM settings WHERE key = ?`, key).Scan(&v); err != nil {
		return def
	}
	return v
}

// GetInt returns an integer setting, or def when it is unset or not an
// integer.
func (s *Store) GetInt(key string, def int) int {
	v, err := strconv.Atoi(s.Get(key, ""))
	if err != nil {
		return def
	}
	return v
}

// Set stores a setting, replacing its previous value.
func (s *Store) Set(key, value string) error {
	_, err := s.db.Exec(`INSERT INTO settings (key, value) VALUES (?, ?)
		ON CONFLICT(key) DO UPDATE SET value = excluded.value`, key, value)
	return err
}
