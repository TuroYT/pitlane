package pool

import (
	"bufio"
	"bytes"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"
)

var (
	hostMemMu   sync.Mutex
	hostMemAt   time.Time
	hostMemLast int64
)

// hostMemory returns the memory Pitlane's jobs share, cached for 30 s: the
// MemTotal of /proc/meminfo (the LXC container's under lxcfs), lowered to
// the memory.max of Pitlane's cgroup when Pitlane runs on the host or in an
// LXC container (inside a Docker container, that limit is Pitlane's own,
// not its slots'). 0 when unknown.
func hostMemory() int64 {
	hostMemMu.Lock()
	defer hostMemMu.Unlock()
	if time.Since(hostMemAt) < 30*time.Second {
		return hostMemLast
	}
	_, docker := os.Stat("/.dockerenv")
	hostMemLast = memoryLimit(readFile("/proc/meminfo"), readFile("/sys/fs/cgroup/memory.max"), docker == nil)
	hostMemAt = time.Now()
	return hostMemLast
}

func readFile(name string) []byte {
	b, _ := os.ReadFile(name) //nolint:gosec // G304: fixed system files
	return b
}

// memoryLimit combines /proc/meminfo and a cgroup memory.max into the
// memory available to jobs (0 when unknown).
func memoryLimit(meminfo, cgroupMax []byte, inDocker bool) int64 {
	var total int64
	sc := bufio.NewScanner(bytes.NewReader(meminfo))
	for sc.Scan() {
		f := strings.Fields(sc.Text())
		if len(f) >= 2 && f[0] == "MemTotal:" {
			if v, err := strconv.ParseInt(f[1], 10, 64); err == nil && v > 0 {
				total = v << 10
			}
		}
	}
	if inDocker {
		return total
	}
	if v, err := strconv.ParseInt(strings.TrimSpace(string(cgroupMax)), 10, 64); err == nil && v > 0 && (total == 0 || v < total) {
		total = v
	}
	return total
}
