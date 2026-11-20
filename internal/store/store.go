// Package store persists Pitlane's configuration, targets, job history and
// sessions in a single SQLite database. Secrets are encrypted at rest with a
// master key kept next to the database.
package store

import (
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

// Files and modes of the data directory. Everything in it is private to the
// user running Pitlane: it holds the master key, the database and job logs.
const (
	dbFile                      = "pitlane.db"
	keyFile                     = "master.key"
	privateDirMode  os.FileMode = 0o700
	privateFileMode os.FileMode = 0o600
	// groupOtherWrite are the permission bits that let someone other than
	// the owner plant or replace files in a directory.
	groupOtherWrite os.FileMode = 0o022
)

// dsnPragmas are set on every connection: WAL lets readers run while a job
// is written, busy_timeout (ms) waits for a lock instead of failing.
const dsnPragmas = "?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)&_pragma=foreign_keys(1)"

// dbFiles are the files SQLite keeps for the database in WAL mode.
var dbFiles = []string{dbFile, dbFile + "-wal", dbFile + "-shm"}

// Store wraps the SQLite database.
type Store struct {
	db   *sql.DB
	aead cipher.AEAD // seals the secret settings, see SetSecret
}

// Open opens (and migrates) the database stored in dataDir, creating the
// directory, the database and the master key when missing.
func Open(dataDir string) (*Store, error) {
	if err := os.MkdirAll(dataDir, privateDirMode); err != nil {
		return nil, err
	}
	if err := restrictDataDir(dataDir); err != nil {
		return nil, err
	}
	aead, err := newAEAD(filepath.Join(dataDir, keyFile))
	if err != nil {
		return nil, err
	}
	db, err := sql.Open("sqlite", "file:"+filepath.Join(dataDir, dbFile)+dsnPragmas)
	if err != nil {
		return nil, err
	}
	// A single connection serializes every access, as SQLite allows only
	// one writer anyway. Transactions must therefore never use s.db (see
	// withTx).
	db.SetMaxOpenConns(1)
	s := &Store{db: db, aead: aead}
	if err := s.migrate(); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("migrate: %w", err)
	}
	if err := restrictDBFiles(dataDir); err != nil {
		_ = db.Close()
		return nil, err
	}
	return s, nil
}

// Close closes the database.
func (s *Store) Close() error { return s.db.Close() }

// restrictDBFiles makes the database files private: SQLite creates them with
// the process umask.
func restrictDBFiles(dataDir string) error {
	for _, f := range dbFiles {
		if err := os.Chmod(filepath.Join(dataDir, f), privateFileMode); err != nil && !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("restrict %s: %w", f, err)
		}
	}
	return nil
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
// checked by restrictDBFiles), as long as nobody else can write to it (plant
// or replace files). Anything else fails.
func restrictDataDir(dataDir string) error {
	err := chmod(dataDir, privateDirMode) //nolint:gosec // G302: a directory needs the x bit
	if err == nil {
		return nil
	}
	uid, mode, serr := dirInfo(dataDir)
	switch {
	case serr != nil:
		return fmt.Errorf("restrict data directory: %w (and cannot check its owner: %v)", err, serr)
	case uid == os.Geteuid():
		return fmt.Errorf("restrict data directory: %w", err)
	case mode.Perm()&groupOtherWrite != 0:
		return fmt.Errorf("data directory %s belongs to another user (uid %d) and is writable by its group or others (%v): "+
			"make it owned by the user running Pitlane, or at least not group/world-writable", dataDir, uid, mode.Perm())
	}
	slog.Warn("cannot restrict the data directory to its owner: it belongs to another user, its files stay private (0600)",
		"dir", dataDir, "owner", uid, "mode", mode.Perm().String(), "error", err)
	return nil
}
