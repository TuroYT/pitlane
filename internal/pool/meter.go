package pool

import (
	"context"
	"sync"
	"time"
)

// Per-job resource usage, read from cgroup v2. A job runs in two
// containers: its runner, created for it, and its slot, the long-lived
// Docker-in-Docker daemon that runs its `services:` and `docker build`. Both
// are measured as deltas between the job's start (Meter.Begin; the runner's
// start when it is not seen) and the runner's exit, except the runner's
// memory peak, which covers its whole life. Nothing here may fail a job: when the cgroup
// files cannot be found or read (Docker Desktop, rootless Docker, Pitlane in
// a container without the host's /sys/fs/cgroup, cgroup v1), the job simply
// has no usage.

// UsageSampleInterval is how often a running job's cgroups are read.
const UsageSampleInterval = 2 * time.Second

// CgroupUsage is what one container used during a job. Each field is -1
// when it is unknown.
type CgroupUsage struct {
	MemPeak     int64 // bytes, highest memory use (page cache included)
	CPUUsec     int64 // CPU time, microseconds
	CPUSomeUsec int64 // time some tasks waited for a CPU (cpu.pressure "some")
	IOFullUsec  int64 // time all tasks were stalled on I/O (io.pressure "full")
	MemFullUsec int64 // time all tasks were stalled on memory (memory.pressure "full")
	// AnonPeak is the highest anonymous memory sampled (memory.stat
	// "anon"): what the container really needed, without the page cache
	// that inflates a slot's memory.peak (image layers, build contexts).
	AnonPeak int64
}

// Known reports whether anything was measured.
func (u CgroupUsage) Known() bool {
	return u.MemPeak >= 0 || u.CPUUsec >= 0 || u.CPUSomeUsec >= 0 || u.IOFullUsec >= 0 || u.MemFullUsec >= 0 || u.AnonPeak >= 0
}

func unknownUsage() CgroupUsage {
	return CgroupUsage{MemPeak: -1, CPUUsec: -1, CPUSomeUsec: -1, IOFullUsec: -1, MemFullUsec: -1, AnonPeak: -1}
}

// JobUsage is what a job used: its runner, its slot, and how long it ran.
type JobUsage struct {
	Wall   time.Duration
	Runner CgroupUsage
	Slot   CgroupUsage
}

// Known reports whether anything was measured.
func (u JobUsage) Known() bool { return u.Runner.Known() || u.Slot.Known() }

// Meter measures one job while its runner runs. A nil Meter measures
// nothing.
type Meter struct {
	start  time.Time
	mu     sync.Mutex
	runner *cgTracker // nil: not found
	slot   *cgTracker // nil: not found
	stop   chan struct{}
	done   chan struct{}
	once   sync.Once
	usage  JobUsage
}

// newMeter starts measuring the given cgroup directories ("" = unknown);
// it returns nil when both are unknown. interval 0 disables background
// sampling (tests sample by hand).
func newMeter(runnerDir, slotDir string, interval time.Duration) *Meter {
	if runnerDir == "" && slotDir == "" {
		return nil
	}
	m := &Meter{start: time.Now(), stop: make(chan struct{}), done: make(chan struct{})}
	if runnerDir != "" {
		m.runner = newTracker(runnerDir, true)
	}
	if slotDir != "" {
		m.slot = newTracker(slotDir, false)
	}
	if interval <= 0 {
		close(m.done)
		return m
	}
	go func() {
		defer close(m.done)
		tick := time.NewTicker(interval)
		defer tick.Stop()
		for {
			select {
			case <-m.stop:
				return
			case <-tick.C:
				m.sample()
			}
		}
	}()
	return m
}

// Begin marks the start of the runner's job: the wall time, CPU time and
// pressure are counted from now on, and the slot's memory peak too.
// Without it (a job whose start was not seen) the whole runner lifetime is
// measured.
func (m *Meter) Begin() {
	if m == nil {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.start = time.Now()
	if m.runner != nil {
		m.runner.rebase()
	}
	if m.slot != nil {
		m.slot.rebase()
	}
}

// Current returns what the job uses now (bytes, 0 when unknown or nil):
// the runner's memory.current plus the slot's anonymous memory (its
// memory.current when anon is unknown), as of the last sample.
func (m *Meter) Current() int64 {
	if m == nil {
		return 0
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	var n int64
	if m.runner != nil && m.runner.seen {
		n += max(0, m.runner.last.memCurrent)
	}
	if m.slot != nil && m.slot.seen {
		v := m.slot.last.anon
		if v < 0 {
			v = m.slot.last.memCurrent
		}
		n += max(0, v)
	}
	return n
}

func (m *Meter) sample() {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.runner != nil {
		m.runner.sample()
	}
	if m.slot != nil {
		m.slot.sample()
	}
}

// Stop ends the measurement and returns the job's usage. Call it once the
// runner exited, before the slot is cleaned (the cleanup is not the job's).
// The runner's cgroup is usually gone by then: its last reading (at most
// UsageSampleInterval old) is used. Further calls return the same usage.
func (m *Meter) Stop() JobUsage {
	if m == nil {
		return JobUsage{Runner: unknownUsage(), Slot: unknownUsage()}
	}
	m.once.Do(func() {
		select {
		case <-m.done:
		default:
			close(m.stop)
			<-m.done
		}
		m.sample()
		m.mu.Lock()
		defer m.mu.Unlock()
		m.usage = JobUsage{Wall: time.Since(m.start), Runner: unknownUsage(), Slot: unknownUsage()}
		if m.runner != nil {
			m.usage.Runner = m.runner.usage()
			m.runner.close()
		}
		if m.slot != nil {
			m.usage.Slot = m.slot.usage()
			m.slot.close()
		}
	})
	return m.usage
}

// MeasureJob starts measuring the job of a runner container that has just
// started in a slot. It returns nil (no measurement, never an error) when
// the cgroups cannot be found.
func (p *Pool) MeasureJob(ctx context.Context, runnerID string, slot int) *Meter {
	fs := p.cgroups
	if fs.root == "" {
		fs = hostCgroups
	}
	dir := func(name string) string {
		info, err := p.cli.ContainerInspect(ctx, name)
		if err != nil {
			return ""
		}
		pid := 0
		if info.State != nil {
			pid = info.State.Pid
		}
		return fs.find(info.ID, pid)
	}
	m := newMeter(dir(runnerID), dir(slotName(slot)), UsageSampleInterval)
	if m != nil {
		// Admission counts what the job uses when it exceeds its prediction.
		p.mu.Lock()
		if p.meters == nil {
			p.meters = map[int]*Meter{}
		}
		p.meters[slot] = m
		p.mu.Unlock()
	}
	if m == nil {
		p.cgroupWarn.Do(func() {
			p.log.Info("container cgroups not readable (not cgroup v2, or Pitlane cannot see the host's /sys/fs/cgroup): per-job resource usage is not recorded")
		})
	}
	return m
}
