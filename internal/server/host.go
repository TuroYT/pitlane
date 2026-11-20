package server

import (
	"os"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

// ------------------------------------------------------------------- state

type hostStats struct {
	CPUs       int     `json:"cpus"`
	CPUPercent float64 `json:"cpuPercent"` // 0-100, whole machine/container`
	MemTotal   int64   `json:"memTotal"`
	MemAvail   int64   `json:"memAvailable"`
	DiskTotal  int64   `json:"diskTotal"`
	DiskFree   int64   `json:"diskFree"`
}

// cpuSampler measures real CPU usage. It reads the cgroup v2 counter of the
// container Pitlane runs in (correct inside LXC/Docker, where /proc/loadavg
// reports the whole host) and falls back to /proc/stat.
type cpuSampler struct {
	mu       sync.Mutex
	lastUse  float64 // CPU-seconds
	lastWall time.Time
	percent  float64
}

var cpu = &cpuSampler{}

func cpuSeconds() (float64, bool) {
	if b, err := os.ReadFile("/sys/fs/cgroup/cpu.stat"); err == nil {
		for _, line := range strings.Split(string(b), "\n") {
			if f := strings.Fields(line); len(f) == 2 && f[0] == "usage_usec" {
				v, err := strconv.ParseFloat(f[1], 64)
				return v / 1e6, err == nil
			}
		}
	}
	if b, err := os.ReadFile("/proc/stat"); err == nil {
		f := strings.Fields(strings.SplitN(string(b), "\n", 2)[0])
		if len(f) > 8 && f[0] == "cpu" {
			var busy float64
			for i, x := range f[1:9] {
				if i == 3 || i == 4 { // idle, iowait
					continue
				}
				v, _ := strconv.ParseFloat(x, 64)
				busy += v
			}
			return busy / 100, true // USER_HZ
		}
	}
	return 0, false
}

func (c *cpuSampler) sample() float64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	use, ok := cpuSeconds()
	now := time.Now()
	if !ok {
		return 0
	}
	if !c.lastWall.IsZero() {
		if dt := now.Sub(c.lastWall).Seconds(); dt >= 1 {
			c.percent = min(100, max(0, (use-c.lastUse)/dt/float64(runtime.NumCPU())*100))
			c.lastUse, c.lastWall = use, now
		}
	} else {
		c.lastUse, c.lastWall = use, now
	}
	return c.percent
}

func readHost(dataDir string) hostStats {
	h := hostStats{CPUs: runtime.NumCPU(), CPUPercent: cpu.sample()}
	if b, err := os.ReadFile("/proc/meminfo"); err == nil {
		for _, line := range strings.Split(string(b), "\n") {
			f := strings.Fields(line)
			if len(f) < 2 {
				continue
			}
			v, _ := strconv.ParseInt(f[1], 10, 64)
			switch f[0] {
			case "MemTotal:":
				h.MemTotal = v * 1024
			case "MemAvailable:":
				h.MemAvail = v * 1024
			}
		}
	}
	dir := "/var/lib/docker"
	if _, err := os.Stat(dir); err != nil {
		dir = dataDir
	}
	var st syscall.Statfs_t
	if err := syscall.Statfs(dir, &st); err == nil {
		h.DiskTotal = int64(st.Blocks) * st.Bsize //nolint:gosec // G115: block counts of a real filesystem fit in int64
		h.DiskFree = int64(st.Bavail) * st.Bsize  //nolint:gosec // G115: same
	}
	return h
}
