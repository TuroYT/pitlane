package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// Roles, from the least to the most privileged.
const (
	// RoleViewer reads everything but secrets and changes nothing.
	RoleViewer = "viewer"
	// RoleOperator is a viewer who can also pause or resume targets, check
	// for updates and pull the runner image.
	RoleOperator = "operator"
	// RoleAdmin can do everything.
	RoleAdmin = "admin"
)

// RoleRank orders the roles: 0 for no (or an unknown) role.
func RoleRank(role string) int {
	switch role {
	case RoleViewer:
		return 1
	case RoleOperator:
		return 2
	case RoleAdmin:
		return 3
	}
	return 0
}

// ValidRole reports whether role is one of the roles above.
func ValidRole(role string) bool { return RoleRank(role) > 0 }

// ErrLastAdmin refuses a change that would leave no admin.
var ErrLastAdmin = errors.New("there must always be at least one admin: add another admin first")

// ErrNotFound is returned for a user or rule that does not exist.
var ErrNotFound = errors.New("not found")

// User is a GitHub login with a role.
type User struct {
	Login     string `json:"login"`
	Role      string `json:"role"`
	CreatedAt int64  `json:"createdAt"`
	CreatedBy string `json:"createdBy"`
}

const userCols = `login, role, created_at, created_by`

// scanUser reads the userCols of a row.
func scanUser(sc rowScanner) (User, error) {
	var u User
	err := sc.Scan(&u.Login, &u.Role, &u.CreatedAt, &u.CreatedBy)
	return u, err
}

// Users lists the users, by login.
func (s *Store) Users() ([]User, error) {
	return queryRows(context.Background(), s.db, []User{}, scanUser, `SELECT `+userCols+` FROM users ORDER BY login COLLATE NOCASE`)
}

// User returns a user by login (case-insensitive).
func (s *Store) User(login string) (User, bool) {
	u, err := scanUser(s.db.QueryRow(`SELECT `+userCols+` FROM users WHERE login = ?`, login))
	return u, err == nil
}

// HasUsers reports whether at least one user exists (the first setup is
// done).
func (s *Store) HasUsers() bool {
	var n int
	_ = s.db.QueryRow(`SELECT COUNT(*) FROM users`).Scan(&n)
	return n > 0
}

// PutUser adds a user or changes its role. It returns the previous role
// ("" for a new user). Demoting the last admin fails with ErrLastAdmin.
func (s *Store) PutUser(u User) (previous string, err error) {
	if !ValidRole(u.Role) {
		return "", fmt.Errorf("invalid role %q", u.Role)
	}
	if u.CreatedAt == 0 {
		u.CreatedAt = time.Now().Unix()
	}
	err = s.updateUsers(func(tx *sql.Tx) error {
		if previous, err = userRole(tx, u.Login); err != nil {
			return err
		}
		if previous == RoleAdmin && u.Role != RoleAdmin {
			if err := lastAdminCheck(tx); err != nil {
				return err
			}
		}
		if previous != "" {
			_, err := tx.Exec(`UPDATE users SET role = ? WHERE login = ?`, u.Role, u.Login)
			return err
		}
		_, err := tx.Exec(`INSERT INTO users (`+userCols+`) VALUES (?, ?, ?, ?)`, u.Login, u.Role, u.CreatedAt, u.CreatedBy)
		return err
	})
	return previous, err
}

// userRole returns the role of a user, "" when there is no such user.
func userRole(tx *sql.Tx, login string) (string, error) {
	var role string
	err := tx.QueryRow(`SELECT role FROM users WHERE login = ?`, login).Scan(&role)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	return role, err
}

// DeleteUser removes a user and its sessions. Removing the last admin fails
// with ErrLastAdmin, an unknown user with ErrNotFound.
func (s *Store) DeleteUser(login string) (User, error) {
	var u User
	err := s.updateUsers(func(tx *sql.Tx) error {
		var err error
		u, err = scanUser(tx.QueryRow(`SELECT `+userCols+` FROM users WHERE login = ?`, login))
		if errors.Is(err, sql.ErrNoRows) {
			return ErrNotFound
		}
		if err != nil {
			return err
		}
		if u.Role == RoleAdmin {
			if err := lastAdminCheck(tx); err != nil {
				return err
			}
		}
		if _, err := tx.Exec(`DELETE FROM users WHERE login = ?`, u.Login); err != nil {
			return err
		}
		// A user granted access again later starts from a fresh sign-in.
		_, err = tx.Exec(`DELETE FROM sessions WHERE login = ? COLLATE NOCASE`, u.Login)
		return err
	})
	return u, err
}

// lastAdminCheck fails when the admin about to lose that role is the only
// one.
func lastAdminCheck(tx *sql.Tx) error {
	var n int
	if err := tx.QueryRow(`SELECT COUNT(*) FROM users WHERE role = ?`, RoleAdmin).Scan(&n); err != nil {
		return err
	}
	if n <= 1 {
		return ErrLastAdmin
	}
	return nil
}

// queryMirrorAdmins writes the admins, comma-separated, to the legacy
// allowed_users setting.
const queryMirrorAdmins = `INSERT INTO settings (key, value)
	SELECT 'allowed_users', COALESCE(group_concat(login, ','), '') FROM (SELECT login FROM users WHERE role = ? ORDER BY login COLLATE NOCASE) WHERE true
	ON CONFLICT(key) DO UPDATE SET value = excluded.value`

// updateUsers runs f in a transaction and, when it succeeds, mirrors the
// admins to the legacy allowed_users setting in the same transaction: an
// older Pitlane started on this database (a downgrade) lets the admins in,
// and nobody else.
func (s *Store) updateUsers(f func(tx *sql.Tx) error) error {
	return s.withTx(context.Background(), func(tx *sql.Tx) error {
		if err := f(tx); err != nil {
			return err
		}
		_, err := tx.Exec(queryMirrorAdmins, RoleAdmin)
		return err
	})
}
