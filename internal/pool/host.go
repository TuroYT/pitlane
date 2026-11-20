package pool

import (
	"os"
	"syscall"
)

// hostEnv is what the pool reads from the machine Pitlane runs on, outside
// the Docker API. realHost is the real one; tests point the paths at
// temporary files and replace the functions.
type hostEnv struct {
	dockerEnv  string // exists when Pitlane runs in a Docker container
	dockerRoot string // Docker's data directory, when visible from Pitlane
	selfExe    string // the running binary, still the right file after a self-update replaced it on disk
	binfmtDir  string // binfmt_misc mount point
	meminfo    string // /proc/meminfo
	memoryMax  string // memory.max of Pitlane's own cgroup

	hostname   func() (string, error)
	executable func() (string, error)
	statfs     func(path string) (total, free int64, err error)
}

var realHost = &hostEnv{
	dockerEnv:  "/.dockerenv",
	dockerRoot: "/var/lib/docker",
	selfExe:    "/proc/self/exe",
	binfmtDir:  "/proc/sys/fs/binfmt_misc",
	meminfo:    "/proc/meminfo",
	memoryMax:  "/sys/fs/cgroup/memory.max",
	hostname:   os.Hostname,
	executable: os.Executable,
	statfs:     statfs,
}

// host returns the machine the pool runs on.
func (p *Pool) host() *hostEnv {
	if p.env != nil {
		return p.env
	}
	return realHost
}

// inDocker reports whether Pitlane runs in a Docker container.
func (h *hostEnv) inDocker() bool {
	_, err := os.Stat(h.dockerEnv)
	return err == nil
}

// statfs returns the total and available bytes of the filesystem of path.
func statfs(path string) (total, free int64, err error) {
	var st syscall.Statfs_t
	if err := syscall.Statfs(path, &st); err != nil {
		return 0, 0, err
	}
	return int64(st.Blocks) * st.Bsize, int64(st.Bavail) * st.Bsize, nil //nolint:gosec // G115: block counts of a real filesystem fit in int64
}
