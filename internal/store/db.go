package store

import (
	"context"
	"database/sql"
)

// rowScanner is what *sql.Row and *sql.Rows have in common, so one scan
// function serves both a single-row lookup and a listing.
type rowScanner interface {
	Scan(dest ...any) error
}

// queryRows runs query and appends each row, read with scan, to out. out
// decides what an empty result is: nil, or an empty slice for the listings
// the API encodes as [] rather than null. Rows are always closed.
func queryRows[T any](ctx context.Context, db *sql.DB, out []T, scan func(rowScanner) (T, error), query string, args ...any) ([]T, error) {
	rows, err := db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		v, err := scan(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return out, nil
}

// withTx runs fn in a transaction, committed when fn returns nil and rolled
// back otherwise. fn must only use tx: the pool has a single connection (see
// Open), so a query on s.db would wait for the transaction forever.
func (s *Store) withTx(ctx context.Context, fn func(*sql.Tx) error) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }() // no-op after Commit
	if err := fn(tx); err != nil {
		return err
	}
	return tx.Commit()
}
