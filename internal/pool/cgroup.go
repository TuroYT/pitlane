package pool

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
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

// ------------------------------------------------------------- cgroup files

// cgroupFS locates and reads container cgroups.
type cgroupFS struct {
	root string // cgroup v2 mount point
	proc string // procfs mount point
}

var hostCgroups = cgroupFS{root: "/sys/fs/cgroup", proc: "/proc"}

var containerID = regexp.MustCompile(`^[0-9a-f]{64}$`)

// find returns the cgroup directory of a container, or "" when it cannot be
// found (or the host is not on cgroup v2). pid is the container's main
// process as Docker reports it (0 when unknown).
func (fs cgroupFS) find(id string, pid int) string {
	if !containerID.MatchString(id) {
		return ""
	}
	if _, err := os.Stat(filepath.Join(fs.root, "cgroup.controllers")); err != nil {
		return "" // not cgroup v2 (or not mounted here)
	}
	// The main process's own cgroup works with any cgroup driver and parent,
	// as long as Pitlane shares the host's PID and cgroup namespaces. A slot
	// daemon moves itself into a child cgroup ("…/init"): cut the path at
	// the container's own directory.
	if pid > 0 {
		if b, err := os.ReadFile(filepath.Join(fs.proc, strconv.Itoa(pid), "cgroup")); err == nil {
			for _, line := range strings.Split(string(b), "\n") {
				rel, ok := strings.CutPrefix(line, "0::")
				if !ok {
					continue
				}
				parts := strings.Split(strings.Trim(rel, "/"), "/")
				for i, part := range parts {
					if strings.Contains(part, id) {
						dir := filepath.Join(append([]string{fs.root}, parts[:i+1]...)...)
						if strings.HasPrefix(dir, filepath.Clean(fs.root)+"/") && isCgroup(dir) {
							return dir
						}
						break
					}
				}
			}
		}
	}
	// Default layouts: systemd cgroup driver, then cgroupfs.
	for _, rel := range []string{"system.slice/docker-" + id + ".scope", "docker/" + id} {
		if dir := filepath.Join(fs.root, rel); isCgroup(dir) {
			return dir
		}
	}
	return ""
}

func isCgroup(dir string) bool {
	_, err := os.Stat(filepath.Join(dir, "cgroup.procs")) //nolint:gosec // G703: dir is under the cgroup root, named by a validated container ID
	return err == nil
}

// cgSample is one reading of a cgroup's counters (-1: unknown).
type cgSample struct {
	memCurrent  int64
	cpuUsec     int64
	cpuSomeUsec int64
	ioFullUsec  int64
	memFullUsec int64
	anon        int64 // memory.stat "anon"
}

func readInt(file string) int64 {
	b, err := os.ReadFile(file) //nolint:gosec // G304: a file of a cgroup Pitlane located
	if err != nil {
		return -1
	}
	v, err := strconv.ParseInt(strings.TrimSpace(string(b)), 10, 64)
	if err != nil || v < 0 {
		return -1
	}
	return v
}

// keyedValue returns the value of key in a flat-keyed file such as cpu.stat
// ("usage_usec 123").
func keyedValue(file, key string) int64 {
	b, err := os.ReadFile(file) //nolint:gosec // G304: a file of a cgroup Pitlane located
	if err != nil {
		return -1
	}
	sc := bufio.NewScanner(bytes.NewReader(b))
	for sc.Scan() {
		if f := strings.Fields(sc.Text()); len(f) == 2 && f[0] == key {
			if v, err := strconv.ParseInt(f[1], 10, 64); err == nil && v >= 0 {
				return v
			}
		}
	}
	return -1
}

// pressureTotal returns the total= stall time (microseconds) of the "some"
// or "full" line of a PSI file such as io.pressure.
func pressureTotal(file, kind string) int64 {
	b, err := os.ReadFile(file) //nolint:gosec // G304: a file of a cgroup Pitlane located
	if err != nil {
		return -1 // PSI disabled (psi=0) or not readable
	}
	for _, line := range strings.Split(string(b), "\n") {
		f := strings.Fields(line)
		if len(f) == 0 || f[0] != kind {
			continue
		}
		for _, kv := range f[1:] {
			if v, ok := strings.CutPrefix(kv, "total="); ok {
				if n, err := strconv.ParseInt(v, 10, 64); err == nil && n >= 0 {
					return n
				}
			}
		}
	}
	return -1
}

func readSample(dir string) cgSample {
	return cgSample{
		memCurrent:  readInt(filepath.Join(dir, "memory.current")),
		cpuUsec:     keyedValue(filepath.Join(dir, "cpu.stat"), "usage_usec"),
		cpuSomeUsec: pressureTotal(filepath.Join(dir, "cpu.pressure"), "some"),
		ioFullUsec:  pressureTotal(filepath.Join(dir, "io.pressure"), "full"),
		memFullUsec: pressureTotal(filepath.Join(dir, "memory.pressure"), "full"),
		anon:        keyedValue(filepath.Join(dir, "memory.stat"), "anon"),
	}
}

func (s cgSample) known() bool {
	return s.memCurrent >= 0 || s.cpuUsec >= 0 || s.cpuSomeUsec >= 0 || s.ioFullUsec >= 0 || s.memFullUsec >= 0 || s.anon >= 0
}

// ------------------------------------------------------------------ tracker

// cgTracker follows one container's cgroup over a job.
type cgTracker struct {
	dir   string
	base  cgSample // counters at the job's start (zero for a new container)
	last  cgSample // last successful reading
	seen  bool     // at least one successful reading
	max   int64    // highest memory.current seen
	anon  int64    // highest memory.stat anon seen
	peak  *os.File // memory.peak, when it tells the job's peak
	peakV int64    // highest value read from peak (-1: none)
	fresh bool     // created for the job (a runner), not a long-lived slot
}

// newTracker starts following the cgroup in dir. fresh means the cgroup was
// created for the job (a runner): its counters and memory.peak already
// cover the job alone. Otherwise (a long-lived slot) the counters are read
// now as a baseline and memory.peak is reset for this tracker: since Linux
// 6.12, writing to memory.peak resets it to the current usage for reads
// through the same open file. On older kernels the write fails and the peak
// is the highest memory.current sampled during the job.
func newTracker(dir string, fresh bool) *cgTracker {
	t := &cgTracker{dir: dir, max: -1, anon: -1, peakV: -1, fresh: fresh}
	file := filepath.Join(dir, "memory.peak")
	if fresh {
		if f, err := os.Open(file); err == nil { //nolint:gosec // G304: a file of a cgroup Pitlane located
			t.peak = f
		}
	} else if f, err := os.OpenFile(file, os.O_RDWR, 0); err == nil { //nolint:gosec // G304: same
		if _, err := f.WriteString("reset\n"); err == nil {
			t.peak = f
		} else {
			_ = f.Close()
		}
	}
	s := readSample(dir)
	if !fresh {
		t.base = s
	}
	t.record(s)
	return t
}

func (t *cgTracker) record(s cgSample) {
	if !s.known() {
		return // the cgroup is gone (container exited) or unreadable: keep the last reading
	}
	if !t.seen {
		t.last, t.seen = s, true
	} else {
		// A counter that became unreadable keeps its last value.
		for _, f := range []struct {
			dst *int64
			v   int64
		}{
			{&t.last.memCurrent, s.memCurrent}, {&t.last.cpuUsec, s.cpuUsec}, {&t.last.cpuSomeUsec, s.cpuSomeUsec},
			{&t.last.ioFullUsec, s.ioFullUsec}, {&t.last.memFullUsec, s.memFullUsec}, {&t.last.anon, s.anon},
		} {
			if f.v >= 0 {
				*f.dst = f.v
			}
		}
	}
	t.max = max(t.max, s.memCurrent)
	t.anon = max(t.anon, s.anon)
	if t.peak != nil {
		buf := make([]byte, 32)
		n, err := t.peak.ReadAt(buf, 0)
		if err == nil || (errors.Is(err, io.EOF) && n > 0) {
			if v, err := strconv.ParseInt(strings.TrimSpace(string(buf[:n])), 10, 64); err == nil {
				t.peakV = max(t.peakV, v)
			}
		}
	}
}

func (t *cgTracker) sample() { t.record(readSample(t.dir)) }

// rebase restarts the measurement when the job actually starts (a warm
// runner can wait a long time for its job). CPU and pressure deltas start
// from now. A runner keeps its peak since its creation (memory.peak cannot
// be reset on every kernel, and an idle runner uses little memory); a
// slot's peak is reset again.
func (t *cgTracker) rebase() {
	s := readSample(t.dir)
	if !s.known() {
		return
	}
	t.base, t.last, t.seen = s, s, true
	if !t.fresh {
		t.max, t.anon, t.peakV = s.memCurrent, s.anon, -1
		if t.peak != nil {
			if _, err := t.peak.WriteAt([]byte("reset\n"), 0); err != nil {
				_ = t.peak.Close()
				t.peak = nil
			}
		}
	}
	t.record(s)
}

func (t *cgTracker) close() {
	if t.peak != nil {
		_ = t.peak.Close()
		t.peak = nil
	}
}

// usage returns what the container used since the tracker started.
func (t *cgTracker) usage() CgroupUsage {
	u := unknownUsage()
	if !t.seen {
		return u
	}
	delta := func(last, base int64) int64 {
		if last < 0 || base < 0 {
			return -1
		}
		return max(0, last-base)
	}
	u.MemPeak = max(t.peakV, t.max)
	u.AnonPeak = t.anon
	u.CPUUsec = delta(t.last.cpuUsec, t.base.cpuUsec)
	u.CPUSomeUsec = delta(t.last.cpuSomeUsec, t.base.cpuSomeUsec)
	u.IOFullUsec = delta(t.last.ioFullUsec, t.base.ioFullUsec)
	u.MemFullUsec = delta(t.last.memFullUsec, t.base.memFullUsec)
	return u
}

// -------------------------------------------------------------------- meter

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
