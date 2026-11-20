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

func (c *ContainerUsage) derive(wallMs int64) {
	pct := func(v *int64) *float64 {
		if v == nil || wallMs <= 0 {
			return nil
		}
		p := min(100, float64(*v)/float64(wallMs*1000)*100)
		return &p
	}
	c.CPUPressure, c.IOPressure, c.MemPressure = pct(c.CPUSomeUsec), pct(c.IOFullUsec), pct(c.MemFullUsec)
}

func (u *JobUsage) derive() {
	u.Runner.derive(u.WallMs)
	u.Slot.derive(u.WallMs)
	u.MemPeak = nil
	if u.Runner.MemPeak != nil || u.Slot.MemPeak != nil {
		var n int64
		for _, v := range []*int64{u.Runner.MemPeak, u.Slot.MemPeak} {
			if v != nil {
				n += *v
			}
		}
		u.MemPeak = &n
	}
}

// usageColumns are the stored fields of a JobUsage, in column order.
const usageColumns = `usage_wall_ms, runner_mem_peak, runner_cpu_usec, runner_cpu_some_usec, runner_io_full_usec, runner_mem_full_usec,
	slot_mem_peak, slot_cpu_usec, slot_cpu_some_usec, slot_io_full_usec, slot_mem_full_usec, slot_anon_peak`

// usageScan holds the nullable usage columns of a row.
type usageScan [12]sql.NullInt64

func (u *usageScan) dest() []any {
	out := make([]any, len(u))
	for i := range u {
		out[i] = &u[i]
	}
	return out
}

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

const querySetJobUsage = `UPDATE jobs SET usage_wall_ms = ?,
	runner_mem_peak = ?, runner_cpu_usec = ?, runner_cpu_some_usec = ?, runner_io_full_usec = ?, runner_mem_full_usec = ?,
	slot_mem_peak = ?, slot_cpu_usec = ?, slot_cpu_some_usec = ?, slot_io_full_usec = ?, slot_mem_full_usec = ?, slot_anon_peak = ?
	WHERE id = (SELECT id FROM jobs WHERE runner_name = ? ORDER BY id DESC LIMIT 1)`

// SetJobUsage records what the job a runner picked up used. It returns
// false when no job with that runner is recorded (yet).
func (s *Store) SetJobUsage(ctx context.Context, runner string, u JobUsage) (bool, error) {
	n := func(v *int64) any {
		if v == nil {
			return nil
		}
		return *v
	}
	res, err := s.db.ExecContext(ctx, querySetJobUsage,
		u.WallMs, n(u.Runner.MemPeak), n(u.Runner.CPUUsec), n(u.Runner.CPUSomeUsec), n(u.Runner.IOFullUsec), n(u.Runner.MemFullUsec),
		n(u.Slot.MemPeak), n(u.Slot.CPUUsec), n(u.Slot.CPUSomeUsec), n(u.Slot.IOFullUsec), n(u.Slot.MemFullUsec), n(u.Slot.AnonPeak), runner)
	if err != nil {
		return false, err
	}
	rows, err := res.RowsAffected()
	return rows > 0, err
}

// JobMemory is the memory a finished job needed, as the admission predicts
// it: the runner's peak plus the slot's anonymous peak (a quarter of the
// slot's memory.peak for jobs measured before anon was recorded: most of
// it is page cache).
const jobMemoryExpr = `COALESCE(runner_mem_peak, 0) + COALESCE(slot_anon_peak, slot_mem_peak / 4, 0)`

// MemoryHistory returns the memory (see jobMemoryExpr) of the last limit
// measured jobs, newest first, that were not cancelled: of one job (owner,
// repository, workflow file and job name) when workflow and job are set,
// of a repository when only owner and repo are, of every job otherwise.
func (s *Store) MemoryHistory(ctx context.Context, owner, repo, workflow, job string, limit int) ([]int64, error) {
	where, args := `(runner_mem_peak IS NOT NULL OR slot_mem_peak IS NOT NULL) AND finished_at > 0 AND result != 'canceled'`, []any{}
	switch {
	case workflow != "" || job != "":
		where += ` AND owner = ? AND repo = ? AND workflow_ref = ? AND job_name = ?`
		args = append(args, owner, repo, workflow, job)
	case repo != "":
		where += ` AND owner = ? AND repo = ?`
		args = append(args, owner, repo)
	}
	args = append(args, limit)
	rows, err := s.db.QueryContext(ctx, `SELECT `+jobMemoryExpr+` FROM jobs WHERE `+where+` ORDER BY id DESC LIMIT ?`, args...) //nolint:gosec // G202: fixed SQL fragments only
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []int64
	for rows.Next() {
		var v int64
		if err := rows.Scan(&v); err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, rows.Err()
}
