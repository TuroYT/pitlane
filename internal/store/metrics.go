package store

import (
	"context"
)

// queryJobMemoryPeaks reads the jobs_finished index.
const queryJobMemoryPeaks = `SELECT COALESCE(runner_mem_peak, 0) + COALESCE(slot_mem_peak, 0) FROM jobs
	WHERE finished_at >= ? AND (runner_mem_peak IS NOT NULL OR slot_mem_peak IS NOT NULL)`

// JobMemoryPeaks returns the memory peak (runner plus slot, bytes) of the
// measured jobs finished since the given Unix time.
func (s *Store) JobMemoryPeaks(ctx context.Context, since int64) ([]float64, error) {
	return queryRows(ctx, s.db, []float64{}, func(sc rowScanner) (float64, error) {
		var v int64
		err := sc.Scan(&v)
		return float64(v), err
	}, queryJobMemoryPeaks, since)
}

// JobResultCounts counts the finished jobs in the history by result.
func (s *Store) JobResultCounts(ctx context.Context) (map[string]int64, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT result, COUNT(*) FROM jobs WHERE result != '' GROUP BY result`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]int64{}
	for rows.Next() {
		var r string
		var n int64
		if err := rows.Scan(&r, &n); err != nil {
			return nil, err
		}
		out[r] = n
	}
	return out, rows.Err()
}

const queryJobTimings = `SELECT queued_at, started_at, finished_at FROM jobs WHERE started_at >= ? OR finished_at >= ?`

// JobTimings returns, in seconds, the run time of jobs finished since the
// given Unix time and the queue time of jobs started since then.
func (s *Store) JobTimings(ctx context.Context, since int64) (durations, waits []float64, err error) {
	rows, err := s.db.QueryContext(ctx, queryJobTimings, since, since)
	if err != nil {
		return nil, nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var q, st, f int64
		if err := rows.Scan(&q, &st, &f); err != nil {
			return nil, nil, err
		}
		if st > 0 && f >= st && f >= since {
			durations = append(durations, float64(f-st))
		}
		if q > 0 && st >= q && st >= since {
			waits = append(waits, float64(st-q))
		}
	}
	return durations, waits, rows.Err()
}
