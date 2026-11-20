// Package store persists Pitlane's configuration, targets, job history and
// sessions in a single SQLite database. Secrets are encrypted at rest with a
// master key kept next to the database.
package store

import (
	"crypto/aes"
	"crypto/cipher"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"syscall"

	_ "modernc.org/sqlite"
)

// Store wraps the SQLite database.
type Store struct {
	db   *sql.DB
	aead cipher.AEAD
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
