package store

import (
	"crypto/sha256"
	"encoding/hex"
	"strings"
	"time"
)

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
