package pool

import (
	"bufio"
	"bytes"
	"errors"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
)

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
	if dir := fs.fromProc(id, pid); dir != "" {
		return dir
	}
	// Default layouts: systemd cgroup driver, then cgroupfs.
	for _, rel := range []string{"system.slice/docker-" + id + ".scope", "docker/" + id} {
		if dir := filepath.Join(fs.root, rel); isCgroup(dir) {
			return dir
		}
	}
	return ""
}

// fromProc finds the cgroup of container id from its main process pid (0:
// unknown). It works with any cgroup driver and parent, as long as Pitlane
// shares the host's PID and cgroup namespaces. A slot daemon moves itself
// into a child cgroup ("…/init"): the path is cut at the container's own
// directory.
func (fs cgroupFS) fromProc(id string, pid int) string {
	if pid <= 0 {
		return ""
	}
	b, err := os.ReadFile(filepath.Join(fs.proc, strconv.Itoa(pid), "cgroup"))
	if err != nil {
		return ""
	}
	for _, line := range strings.Split(string(b), "\n") {
		rel, ok := strings.CutPrefix(line, "0::")
		if !ok {
			continue
		}
		if dir := fs.containerDir(id, rel); dir != "" {
			return dir
		}
	}
	return ""
}

// containerDir returns the directory of the first component of the cgroup
// path rel naming container id, when it is a cgroup under the root.
func (fs cgroupFS) containerDir(id, rel string) string {
	parts := strings.Split(strings.Trim(rel, "/"), "/")
	for i, part := range parts {
		if !strings.Contains(part, id) {
			continue
		}
		dir := filepath.Join(append([]string{fs.root}, parts[:i+1]...)...)
		if strings.HasPrefix(dir, filepath.Clean(fs.root)+"/") && isCgroup(dir) {
			return dir
		}
		return ""
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
