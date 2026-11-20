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

// hostMemoryTTL is how long a reading of the host's memory is reused.
const hostMemoryTTL = 30 * time.Second

// memoryProbe caches a memory reading for hostMemoryTTL.
type memoryProbe struct {
	read func() int64
	now  func() time.Time

	mu   sync.Mutex
	at   time.Time // guarded by mu: when last was read
	last int64     // guarded by mu
}

func (m *memoryProbe) total() int64 {
	m.mu.Lock()
	defer m.mu.Unlock()
	now := m.now()
	if now.Sub(m.at) < hostMemoryTTL {
		return m.last
	}
	m.last, m.at = m.read(), now
	return m.last
}

var hostMem = &memoryProbe{read: realHost.memory, now: time.Now}

// hostMemory returns the memory Pitlane's jobs share (see hostEnv.memory),
// cached for hostMemoryTTL. 0 when unknown.
func hostMemory() int64 { return hostMem.total() }

// memory returns the MemTotal of /proc/meminfo (the LXC container's under
// lxcfs), lowered to the memory.max of Pitlane's cgroup when Pitlane runs on
// the host or in an LXC container (inside a Docker container, that limit is
// Pitlane's own, not its slots'). 0 when unknown.
func (h *hostEnv) memory() int64 {
	return memoryLimit(readFile(h.meminfo), readFile(h.memoryMax), h.inDocker())
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
