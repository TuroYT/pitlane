package pool

import (
	"context"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

const (
	runnerCID = "1111111111111111111111111111111111111111111111111111111111111111"
	slotCID   = "2222222222222222222222222222222222222222222222222222222222222222"
)

// fakeCgroup creates a cgroup directory with the given counters.
func fakeCgroup(t *testing.T, dir string, mem, peak, cpu, cpuSome, ioFull, memFull int64) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	setCgroup(t, dir, mem, peak, cpu, cpuSome, ioFull, memFull)
}

func setCgroup(t *testing.T, dir string, mem, peak, cpu, cpuSome, ioFull, memFull int64) {
	t.Helper()
	psi := func(some, full int64) string {
		return "some avg10=0.00 avg60=0.00 avg300=0.00 total=" + strconv.FormatInt(some, 10) +
			"\nfull avg10=0.00 avg60=0.00 avg300=0.00 total=" + strconv.FormatInt(full, 10) + "\n"
	}
	files := map[string]string{
		"cgroup.procs":    "1\n",
		"memory.current":  strconv.FormatInt(mem, 10) + "\n",
		"cpu.stat":        "usage_usec " + strconv.FormatInt(cpu, 10) + "\nuser_usec 1\nsystem_usec 1\n",
		"cpu.pressure":    psi(cpuSome, 0),
		"io.pressure":     psi(0, ioFull),
		"memory.pressure": psi(0, memFull),
	}
	if peak >= 0 {
		files["memory.peak"] = strconv.FormatInt(peak, 10) + "\n"
	}
	for name, content := range files {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

func fakeRoot(t *testing.T) cgroupFS {
	t.Helper()
	root := filepath.Join(t.TempDir(), "cgroup")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "cgroup.controllers"), []byte("cpu io memory pids\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	return cgroupFS{root: root, proc: filepath.Join(t.TempDir(), "proc")}
}

func TestCgroupFindLayouts(t *testing.T) {
	fs := fakeRoot(t)
	systemd := filepath.Join(fs.root, "system.slice", "docker-"+runnerCID+".scope")
	cgroupfs := filepath.Join(fs.root, "docker", slotCID)
	fakeCgroup(t, systemd, 1, 1, 1, 1, 1, 1)
	fakeCgroup(t, cgroupfs, 1, 1, 1, 1, 1, 1)
	if got := fs.find(runnerCID, 0); got != systemd {
		t.Errorf("systemd driver: got %q, want %q", got, systemd)
	}
	if got := fs.find(slotCID, 0); got != cgroupfs {
		t.Errorf("cgroupfs driver: got %q, want %q", got, cgroupfs)
	}
	if got := fs.find(strings.Repeat("3", 64), 0); got != "" {
		t.Errorf("unknown container: got %q", got)
	}
	if got := fs.find("../../etc", 0); got != "" {
		t.Errorf("invalid id accepted: %q", got)
	}
}

func TestCgroupFindFromProc(t *testing.T) {
	fs := fakeRoot(t)
	// A custom parent, and a slot daemon that moved itself into "init".
	dir := filepath.Join(fs.root, "lxc.payload", "custom", "docker-"+slotCID+".scope")
	fakeCgroup(t, dir, 1, 1, 1, 1, 1, 1)
	if err := os.MkdirAll(filepath.Join(fs.proc, "42"), 0o755); err != nil {
		t.Fatal(err)
	}
	content := "0::/lxc.payload/custom/docker-" + slotCID + ".scope/init\n"
	if err := os.WriteFile(filepath.Join(fs.proc, "42", "cgroup"), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := fs.find(slotCID, 42); got != dir {
		t.Errorf("got %q, want %q", got, dir)
	}
	// A process not found in /proc falls back to the default layouts.
	if got := fs.find(slotCID, 43); got != "" {
		t.Errorf("got %q, want nothing", got)
	}
}

func TestCgroupFindNotV2(t *testing.T) {
	fs := cgroupFS{root: t.TempDir(), proc: t.TempDir()} // no cgroup.controllers
	fakeCgroup(t, filepath.Join(fs.root, "docker", runnerCID), 1, 1, 1, 1, 1, 1)
	if got := fs.find(runnerCID, 0); got != "" {
		t.Errorf("cgroup v1 / unmounted: got %q", got)
	}
}

func TestMeterRunnerAndSlot(t *testing.T) {
	fs := fakeRoot(t)
	runner := filepath.Join(fs.root, "system.slice", "docker-"+runnerCID+".scope")
	slot := filepath.Join(fs.root, "system.slice", "docker-"+slotCID+".scope")
	fakeCgroup(t, runner, 10, 10, 0, 0, 0, 0)
	// A long-lived slot: lifetime peak 9 GB, counters far from zero. The
	// fake memory.peak is a regular file: the "reset" write lands in it, so
	// it is overwritten below as the kernel would report it.
	fakeCgroup(t, slot, 1000, 9_000_000_000, 5_000_000, 100, 200, 300)
	m := newMeter(runner, slot, 0)
	setCgroup(t, runner, 500, 1_800_000_000, 40_000_000, 1_000_000, 2_000_000, 0)
	setCgroup(t, slot, 3000, 2_000_000_000, 8_000_000, 600, 1200, 300)
	m.sample()
	// The runner exits: its cgroup disappears, the last reading is kept.
	if err := os.RemoveAll(runner); err != nil {
		t.Fatal(err)
	}
	setCgroup(t, slot, 2500, 2_000_000_000, 9_000_000, 700, 1300, 300)
	u := m.Stop()
	want := CgroupUsage{MemPeak: 1_800_000_000, CPUUsec: 40_000_000, CPUSomeUsec: 1_000_000, IOFullUsec: 2_000_000, MemFullUsec: 0, AnonPeak: -1}
	if u.Runner != want {
		t.Errorf("runner: got %+v, want %+v", u.Runner, want)
	}
	wantSlot := CgroupUsage{MemPeak: 2_000_000_000, CPUUsec: 4_000_000, CPUSomeUsec: 600, IOFullUsec: 1100, MemFullUsec: 0, AnonPeak: -1}
	if u.Slot != wantSlot {
		t.Errorf("slot: got %+v, want %+v", u.Slot, wantSlot)
	}
	if !u.Known() || u.Wall <= 0 {
		t.Errorf("usage: %+v", u)
	}
	if again := m.Stop(); again != u {
		t.Errorf("second Stop: %+v", again)
	}
}

// Without memory.peak (kernel < 5.19), or when it cannot be reset (kernel
// < 6.12), the slot's peak is the highest memory.current sampled.
func TestMeterSampledPeakFallback(t *testing.T) {
	fs := fakeRoot(t)
	slot := filepath.Join(fs.root, "docker", slotCID)
	fakeCgroup(t, slot, 1000, -1, 0, 0, 0, 0)
	m := newMeter("", slot, 0)
	for _, mem := range []int64{4000, 7000, 2000} {
		setCgroup(t, slot, mem, -1, 0, 0, 0, 0)
		m.sample()
	}
	if u := m.Stop(); u.Slot.MemPeak != 7000 || u.Runner.Known() {
		t.Errorf("got %+v", u)
	}

	// memory.peak present but read-only: the reset fails, it is ignored
	// (it holds the slot's lifetime peak).
	if os.Geteuid() == 0 {
		t.Skip("root ignores file permissions")
	}
	fakeCgroup(t, slot, 1000, 9_000_000_000, 0, 0, 0, 0)
	if err := os.Chmod(filepath.Join(slot, "memory.peak"), 0o444); err != nil {
		t.Fatal(err)
	}
	m = newMeter("", slot, 0)
	setCgroup(t, slot, 5000, -1, 0, 0, 0, 0)
	m.sample()
	if u := m.Stop(); u.Slot.MemPeak != 5000 {
		t.Errorf("read-only memory.peak: got %+v", u.Slot)
	}
}

func TestMeterUnreadable(t *testing.T) {
	if newMeter("", "", 0) != nil {
		t.Fatal("no cgroup must give no meter")
	}
	var m *Meter
	if u := m.Stop(); u.Known() {
		t.Errorf("nil meter: %+v", u)
	}
	// Directories that exist but whose files cannot be read (or vanished).
	dir := t.TempDir()
	m = newMeter(filepath.Join(dir, "gone"), dir, 0)
	m.sample()
	if u := m.Stop(); u.Known() {
		t.Errorf("unreadable files: %+v", u)
	}
	if os.Geteuid() != 0 {
		fs := fakeRoot(t)
		slot := filepath.Join(fs.root, "docker", slotCID)
		fakeCgroup(t, slot, 1, 1, 1, 1, 1, 1)
		for _, f := range []string{"memory.current", "memory.peak", "cpu.stat", "cpu.pressure", "io.pressure", "memory.pressure"} {
			if err := os.Chmod(filepath.Join(slot, f), 0); err != nil {
				t.Fatal(err)
			}
		}
		m = newMeter("", slot, 0)
		if u := m.Stop(); u.Known() {
			t.Errorf("permission denied: %+v", u)
		}
	}
}

// PSI disabled (psi=0): pressure is unknown, the rest is measured.
func TestMeterWithoutPSI(t *testing.T) {
	fs := fakeRoot(t)
	runner := filepath.Join(fs.root, "docker", runnerCID)
	fakeCgroup(t, runner, 100, 100, 0, 0, 0, 0)
	for _, f := range []string{"cpu.pressure", "io.pressure", "memory.pressure"} {
		if err := os.Remove(filepath.Join(runner, f)); err != nil {
			t.Fatal(err)
		}
	}
	m := newMeter(runner, "", 0)
	u := m.Stop()
	if u.Runner.MemPeak != 100 || u.Runner.CPUUsec != 0 || u.Runner.CPUSomeUsec != -1 || u.Runner.IOFullUsec != -1 || u.Slot.Known() {
		t.Errorf("got %+v", u)
	}
}

func TestPressureTotal(t *testing.T) {
	f := filepath.Join(t.TempDir(), "io.pressure")
	if err := os.WriteFile(f, []byte("some avg10=1.00 avg60=0.50 avg300=0.10 total=123\nfull avg10=0.00 avg60=0.00 avg300=0.00 total=45\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := pressureTotal(f, "some"); got != 123 {
		t.Errorf("some = %d", got)
	}
	if got := pressureTotal(f, "full"); got != 45 {
		t.Errorf("full = %d", got)
	}
	if got := pressureTotal(f+".missing", "full"); got != -1 {
		t.Errorf("missing = %d", got)
	}
}

// A warm runner waits before its job: Begin restarts the deltas (and the
// slot's peak) at the job's start.
func TestMeterBegin(t *testing.T) {
	fs := fakeRoot(t)
	runner := filepath.Join(fs.root, "docker", runnerCID)
	slot := filepath.Join(fs.root, "docker", slotCID)
	fakeCgroup(t, runner, 10, 10, 0, 0, 0, 0)
	fakeCgroup(t, slot, 1000, 1000, 5_000_000, 0, 0, 0)
	m := newMeter(runner, slot, 0)
	// Idle wait: the slot did something else (a previous cleanup's tail).
	setCgroup(t, runner, 50, 60, 1_000_000, 10, 0, 0)
	setCgroup(t, slot, 4000, 4000, 6_000_000, 0, 0, 0)
	m.sample()
	setCgroup(t, slot, 1500, 4000, 6_000_000, 0, 0, 0)
	m.Begin()
	setCgroup(t, runner, 500, 900, 3_000_000, 30, 0, 0)
	setCgroup(t, slot, 2000, 2000, 6_500_000, 0, 0, 0)
	u := m.Stop()
	if u.Runner.CPUUsec != 2_000_000 || u.Runner.CPUSomeUsec != 20 || u.Runner.MemPeak != 900 {
		t.Errorf("runner: %+v", u.Runner)
	}
	if u.Slot.CPUUsec != 500_000 || u.Slot.MemPeak != 2000 {
		t.Errorf("slot: %+v", u.Slot)
	}
	var nilMeter *Meter
	nilMeter.Begin() // no-op
}

func setAnon(t *testing.T, dir string, anon int64) {
	t.Helper()
	content := "anon " + strconv.FormatInt(anon, 10) + "\nfile 9000000000\nshmem 0\n"
	if err := os.WriteFile(filepath.Join(dir, "memory.stat"), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// A slot's memory.peak counts the page cache (image layers, build
// contexts); its anonymous memory is what the job really needed. Current
// reports the runner's memory plus the slot's anonymous memory.
func TestMeterAnonPeakAndCurrent(t *testing.T) {
	fs := fakeRoot(t)
	runner := filepath.Join(fs.root, "docker", runnerCID)
	slot := filepath.Join(fs.root, "docker", slotCID)
	fakeCgroup(t, runner, 100, 100, 0, 0, 0, 0)
	fakeCgroup(t, slot, 1000, 1000, 0, 0, 0, 0)
	setAnon(t, slot, 200)
	m := newMeter(runner, slot, 0)
	m.Begin()
	setCgroup(t, runner, 900, 900, 0, 0, 0, 0)
	setCgroup(t, slot, 6000, 6000, 0, 0, 0, 0)
	setAnon(t, slot, 1500)
	m.sample()
	if got := m.Current(); got != 900+1500 {
		t.Errorf("Current = %d, want %d", got, 900+1500)
	}
	setAnon(t, slot, 700)
	u := m.Stop()
	if u.Slot.AnonPeak != 1500 || u.Slot.MemPeak != 6000 {
		t.Errorf("slot: %+v", u.Slot)
	}
	if u.Runner.AnonPeak != -1 {
		t.Errorf("runner without memory.stat: %+v", u.Runner)
	}
	var nilMeter *Meter
	if nilMeter.Current() != 0 {
		t.Error("nil meter must use nothing")
	}
}

// MeasureJob finds both cgroups through a fake Docker API and registers the
// meter for admission.
func TestMeasureJob(t *testing.T) {
	p := newTestPool("owner", SlotState{Index: 1, Status: statusBusy})
	p.cgroups = fakeRoot(t)
	fakeCgroup(t, filepath.Join(p.cgroups.root, "docker", runnerCID), 100, 100, 0, 0, 0, 0)
	fakeCgroup(t, filepath.Join(p.cgroups.root, "docker", slotCID), 1000, 1000, 0, 0, 0, 0)
	newDockerAPI(t, p, map[string]http.HandlerFunc{
		"GET /containers/runner/json":         reply(map[string]any{"Id": runnerCID}),
		"GET /containers/pitlane-slot-1/json": reply(map[string]any{"Id": slotCID}),
	})
	m := p.MeasureJob(context.Background(), "runner", 1)
	if m == nil || p.meters[1] != m {
		t.Fatal("meter not registered")
	}
	defer m.Stop()
	if got := m.Current(); got != 1100 {
		t.Fatalf("current: %d", got)
	}
}
