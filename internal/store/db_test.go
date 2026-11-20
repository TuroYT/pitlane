package store

import (
	"context"
	"database/sql"
	"errors"
	"testing"
)

func TestWithTxRollsBackOnError(t *testing.T) {
	s := open(t)
	boom := errors.New("boom")
	err := s.withTx(context.Background(), func(tx *sql.Tx) error {
		if _, err := tx.Exec(`INSERT INTO settings (key, value) VALUES ('k', 'v')`); err != nil {
			return err
		}
		return boom
	})
	if !errors.Is(err, boom) {
		t.Fatalf("withTx returned %v, want %v", err, boom)
	}
	if v := s.Get("k", "unset"); v != "unset" {
		t.Fatalf("setting written by a failed transaction: %q", v)
	}
}

func TestQueryRowsEmpty(t *testing.T) {
	s := open(t)
	scan := func(sc rowScanner) (string, error) {
		var v string
		err := sc.Scan(&v)
		return v, err
	}
	const q = `SELECT value FROM settings WHERE key = 'missing'`
	if out, err := queryRows(context.Background(), s.db, nil, scan, q); err != nil || out != nil {
		t.Fatalf("nil out: got %#v, %v", out, err)
	}
	if out, err := queryRows(context.Background(), s.db, []string{}, scan, q); err != nil || out == nil || len(out) != 0 {
		t.Fatalf("empty out: got %#v, %v", out, err)
	}
}
