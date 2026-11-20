package pool

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// testHost returns a host whose files live in a temporary directory (none
// exist yet) and whose functions fail until a test replaces them.
func testHost(t *testing.T) *hostEnv {
	t.Helper()
	dir := t.TempDir()
	fail := func() (string, error) { return "", errors.New("not faked") }
	return &hostEnv{
		dockerEnv:  filepath.Join(dir, ".dockerenv"),
		dockerRoot: filepath.Join(dir, "docker"),
		selfExe:    filepath.Join(dir, "exe"),
		binfmtDir:  filepath.Join(dir, "binfmt_misc"),
		meminfo:    filepath.Join(dir, "meminfo"),
		memoryMax:  filepath.Join(dir, "memory.max"),
		hostname:   fail,
		executable: fail,
		statfs:     func(string) (int64, int64, error) { return 0, 0, errors.New("not faked") },
	}
}

func TestDiskSpaceUsesDockerRoot(t *testing.T) {
	p := newTestPool("owner")
	p.env = testHost(t)
	var asked string
	p.env.statfs = func(path string) (int64, int64, error) { asked = path; return 100, 40, nil }
	p.cfg.StateFile = filepath.Join(t.TempDir(), "slots.json")
	if _, _, err := p.DiskSpace(); err != nil || asked != filepath.Dir(p.cfg.StateFile) {
		t.Fatalf("without Docker's directory: %q %v", asked, err)
	}
	if err := os.Mkdir(p.env.dockerRoot, 0o755); err != nil {
		t.Fatal(err)
	}
	total, free, err := p.DiskSpace()
	if err != nil || total != 100 || free != 40 || asked != p.env.dockerRoot {
		t.Fatalf("got %d %d %v on %q", total, free, err, asked)
	}
}

func TestHostMemory(t *testing.T) {
	h := testHost(t)
	write(t, h.meminfo, "MemTotal:       8000000 kB\n")
	write(t, h.memoryMax, "4096000000\n")
	if got := h.memory(); got != 4096000000 {
		t.Fatalf("cgroup limit must lower MemTotal: %d", got)
	}
	write(t, h.dockerEnv, "")
	if got := h.memory(); got != 8000000<<10 {
		t.Fatalf("inside Docker the cgroup limit is Pitlane's own: %d", got)
	}
}

func TestMemoryProbeCaches(t *testing.T) {
	now := time.Unix(1000, 0)
	reads := 0
	m := &memoryProbe{read: func() int64 { reads++; return int64(reads) }, now: func() time.Time { return now }}
	first, second := m.total(), m.total()
	if first != 1 || second != 1 {
		t.Fatal("a reading must be reused")
	}
	now = now.Add(hostMemoryTTL)
	if m.total() != 2 {
		t.Fatal("a reading must expire after hostMemoryTTL")
	}
}

func TestMemoryProbeReadsFirstWithAZeroClock(t *testing.T) {
	reads := 0
	m := &memoryProbe{read: func() int64 { reads++; return 42 }, now: func() time.Time { return time.Time{} }}
	if got := m.total(); got != 42 || reads != 1 {
		t.Fatalf("first call: %d after %d reads, want 42 after 1", got, reads)
	}
}

func TestSelfContainer(t *testing.T) {
	p := newTestPool("owner")
	p.env = testHost(t)
	if p.selfContainer(context.Background()) != nil {
		t.Fatal("on the host there is no container")
	}

	p = newTestPool("owner")
	p.env = testHost(t)
	write(t, p.env.dockerEnv, "")
	p.env.hostname = func() (string, error) { return "abc123", nil }
	p.env.executable = func() (string, error) { return "/pitlane", nil }
	newDockerAPI(t, p, map[string]http.HandlerFunc{
		"GET /containers/abc123/json": reply(map[string]any{
			"Id": "abc123full", "Name": "/pitlane", "Image": "sha256:img", "Config": map[string]any{},
			"NetworkSettings": map[string]any{"Networks": map[string]any{
				"pitlane-slot-0": map[string]any{"IPAddress": "10.0.0.2"},
				"compose_b":      map[string]any{"IPAddress": "10.1.0.2"},
				"compose_a":      map[string]any{"IPAddress": "10.2.0.2"},
			}},
		}),
	})
	self := p.selfContainer(context.Background())
	if self == nil || self.ID != "abc123full" || self.Exe != "/pitlane" || self.Network != "compose_a" || self.Target != "pitlane" {
		t.Fatalf("self: %+v", self)
	}
}

func TestRestoreBinfmtKeepsUnchangedHandlers(t *testing.T) {
	p := newTestPool("owner")
	p.env = testHost(t)
	p.cfg.StateFile = filepath.Join(t.TempDir(), "slots.json")
	magic := "7f454c460201010000000000000000000200b700" // aarch64, as the kernel prints it
	if hostMachine() == "b700" {
		magic = "7f454c460201010000000000000000000200f300" // not native on this host
	}
	def := qemuDef("qemu-aarch64", magic)
	write(t, filepath.Join(p.env.binfmtDir, "register"), "")
	write(t, filepath.Join(p.env.binfmtDir, "qemu-aarch64"), def)
	b, _ := json.Marshal(map[string]string{"qemu-aarch64": def})
	write(t, p.binfmtStateFile(), string(b))
	p.qemuWant = true
	p.restoreBinfmt(context.Background())
	if p.qemuWant || p.binfmt["qemu-aarch64"] != def {
		t.Fatalf("handlers not restored: want=%v %v", p.qemuWant, p.binfmt)
	}
}
