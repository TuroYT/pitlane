package store

import (
	"context"
	"database/sql"
)

// JobUsage is what a job used, measured from the cgroups of its runner
// container and of its slot over the runner's lifetime.
type JobUsage struct {
	WallMs int64 `json:"wallMs"`
	// MemPeak is the runner's peak plus the slot's (bytes): an upper bound
	// of the memory the job needed (both peaks need not coincide). Nil
	// when neither is known. Derived, not stored.
	MemPeak *int64         `json:"memPeak"`
	Runner  ContainerUsage `json:"runner"`
	Slot    ContainerUsage `json:"slot"`
}

// ContainerUsage is what one container used during a job. Nil: unknown.
type ContainerUsage struct {
	MemPeak     *int64 `json:"memPeak"`     // bytes, page cache included
	CPUUsec     *int64 `json:"cpuUsec"`     // CPU time
	CPUSomeUsec *int64 `json:"cpuSomeUsec"` // cpu.pressure "some" stall time
	IOFullUsec  *int64 `json:"ioFullUsec"`  // io.pressure "full" stall time
	MemFullUsec *int64 `json:"memFullUsec"` // memory.pressure "full" stall time
	AnonPeak    *int64 `json:"anonPeak"`    // bytes, highest anonymous memory (slot only)
	// Stall times as a percentage of the wall time. Derived, not stored.
	CPUPressure *float64 `json:"cpuPressurePct"`
	IOPressure  *float64 `json:"ioPressurePct"`
	MemPressure *float64 `json:"memPressurePct"`
}

// usecPerMs converts the wall time (ms) to the unit of the stall times (µs).
const usecPerMs = 1000

// derive computes the stall percentages of c over a wall time.
func (c *ContainerUsage) derive(wallMs int64) {
	pct := func(v *int64) *float64 {
		if v == nil || wallMs <= 0 {
			return nil
		}
		p := min(100, float64(*v)/float64(wallMs*usecPerMs)*100)
		return &p
	}
	c.CPUPressure, c.IOPressure, c.MemPressure = pct(c.CPUSomeUsec), pct(c.IOFullUsec), pct(c.MemFullUsec)
}

// derive computes the fields of u that are not stored.
func (u *JobUsage) derive() {
	u.Runner.derive(u.WallMs)
	u.Slot.derive(u.WallMs)
	u.MemPeak = sumKnown(u.Runner.MemPeak, u.Slot.MemPeak)
}

// sumKnown adds up the known values, nil when none is.
func sumKnown(vs ...*int64) *int64 {
	var sum *int64
	for _, v := range vs {
		if v == nil {
			continue
		}
		if sum == nil {
			sum = new(int64)
		}
		*sum += *v
	}
	return sum
}

// usageColumns are the stored fields of a JobUsage, in column order: the
// order of JobUsage.values and usageScan.
const usageColumns = `usage_wall_ms, runner_mem_peak, runner_cpu_usec, runner_cpu_some_usec, runner_io_full_usec, runner_mem_full_usec,
	slot_mem_peak, slot_cpu_usec, slot_cpu_some_usec, slot_io_full_usec, slot_mem_full_usec, slot_anon_peak`

// usageColumnCount is the number of usageColumns.
const usageColumnCount = 12

// values returns the stored fields of u in usageColumns order, nil (NULL)
// for the unknown ones.
func (u JobUsage) values() []any {
	r, sl := u.Runner, u.Slot
	return []any{u.WallMs,
		nullable(r.MemPeak), nullable(r.CPUUsec), nullable(r.CPUSomeUsec), nullable(r.IOFullUsec), nullable(r.MemFullUsec),
		nullable(sl.MemPeak), nullable(sl.CPUUsec), nullable(sl.CPUSomeUsec), nullable(sl.IOFullUsec), nullable(sl.MemFullUsec), nullable(sl.AnonPeak)}
}

// nullable returns the value of v, or nil (NULL) when v is nil.
func nullable(v *int64) any {
	if v == nil {
		return nil
	}
	return *v
}

// usageScan holds the nullable usage columns of a row.
type usageScan [usageColumnCount]sql.NullInt64

// dest returns the scan destinations of usageColumns.
func (u *usageScan) dest() []any {
	out := make([]any, len(u))
	for i := range u {
		out[i] = &u[i]
	}
	return out
}

// usage returns the JobUsage of the scanned columns, nil for a job that was
// not measured (no wall time).
func (u *usageScan) usage() *JobUsage {
	if !u[0].Valid {
		return nil
	}
	v := func(i int) *int64 {
		if !u[i].Valid {
			return nil
		}
		n := u[i].Int64
		return &n
	}
	j := &JobUsage{WallMs: u[0].Int64,
		Runner: ContainerUsage{MemPeak: v(1), CPUUsec: v(2), CPUSomeUsec: v(3), IOFullUsec: v(4), MemFullUsec: v(5)},
		Slot:   ContainerUsage{MemPeak: v(6), CPUUsec: v(7), CPUSomeUsec: v(8), IOFullUsec: v(9), MemFullUsec: v(10), AnonPeak: v(11)}}
	j.derive()
	return j
}

// querySetJobUsage takes the usageColumns values, then the runner name. It
// updates the latest job recorded for the runner.
const querySetJobUsage = `UPDATE jobs SET usage_wall_ms = ?,
	runner_mem_peak = ?, runner_cpu_usec = ?, runner_cpu_some_usec = ?, runner_io_full_usec = ?, runner_mem_full_usec = ?,
	slot_mem_peak = ?, slot_cpu_usec = ?, slot_cpu_some_usec = ?, slot_io_full_usec = ?, slot_mem_full_usec = ?, slot_anon_peak = ?
	WHERE id = (SELECT id FROM jobs WHERE runner_name = ? ORDER BY id DESC LIMIT 1)`

// SetJobUsage records what the job a runner picked up used. It returns
// false when no job with that runner is recorded (yet).
func (s *Store) SetJobUsage(ctx context.Context, runner string, u JobUsage) (bool, error) {
	res, err := s.db.ExecContext(ctx, querySetJobUsage, append(u.values(), runner)...)
	if err != nil {
		return false, err
	}
	rows, err := res.RowsAffected()
	return rows > 0, err
}

// jobMemoryExpr is the memory a finished job needed, as the admission
// predicts it: the runner's peak plus the slot's anonymous peak (a quarter
// of the slot's memory.peak for jobs measured before anon was recorded:
// most of it is page cache).
const jobMemoryExpr = `COALESCE(runner_mem_peak, 0) + COALESCE(slot_anon_peak, slot_mem_peak / 4, 0)`

// MemoryHistory returns the memory (see jobMemoryExpr) of the last limit
// measured jobs, newest first, that were not cancelled: of one job (owner,
// repository, workflow file and job name) when workflow and job are set,
// of a repository when only owner and repo are, of every job otherwise.
func (s *Store) MemoryHistory(ctx context.Context, owner, repo, workflow, job string, limit int) ([]int64, error) {
	where, args := memoryHistoryWhere(owner, repo, workflow, job)
	query := `SELECT ` + jobMemoryExpr + ` FROM jobs WHERE ` + where + ` ORDER BY id DESC LIMIT ?` //nolint:gosec // G202: fixed SQL fragments only
	return queryRows(ctx, s.db, nil, func(sc rowScanner) (int64, error) {
		var v int64
		err := sc.Scan(&v)
		return v, err
	}, query, append(args, limit)...)
}

// memoryHistoryWhere returns the WHERE condition of MemoryHistory, built from
// fixed fragments, and its arguments.
func memoryHistoryWhere(owner, repo, workflow, job string) (string, []any) {
	const measured = `(runner_mem_peak IS NOT NULL OR slot_mem_peak IS NOT NULL) AND finished_at > 0 AND result != 'canceled'`
	switch {
	case workflow != "" || job != "":
		return measured + ` AND owner = ? AND repo = ? AND workflow_ref = ? AND job_name = ?`, []any{owner, repo, workflow, job}
	case repo != "":
		return measured + ` AND owner = ? AND repo = ?`, []any{owner, repo}
	}
	return measured, nil
}
