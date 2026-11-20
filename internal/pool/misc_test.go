package pool

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestNewRestoresState(t *testing.T) {
	t.Setenv("DOCKER_HOST", "tcp://127.0.0.1:1") // New never connects
	state := filepath.Join(t.TempDir(), "slots.json")
	write(t, state, `{"0":{"owner":"acme","targetId":3,"externals":"sha256:x+shim1","lastUsed":42},"7":{"owner":"gone"}}`)
	p, err := New(Config{Slots: 2, StateFile: state}, slog.New(slog.NewTextHandler(&bytes.Buffer{}, nil)))
	if err != nil {
		t.Fatal(err)
	}
	if p.Docker() == nil {
		t.Fatal("no Docker client")
	}
	if got := p.restored[0]; got.Owner != "acme" || got.TargetID != 3 || got.LastUsed != 42 {
		t.Fatalf("slot 0 not restored: %+v", got)
	}
	if p.externals[0] != "sha256:x+shim1" {
		t.Fatalf("externals %v", p.externals)
	}
	if _, ok := p.restored[7]; ok {
		t.Fatal("the state of a slot beyond the count must be dropped (fail closed)")
	}
	b, _ := os.ReadFile(state)
	if strings.Contains(string(b), `"7"`) {
		t.Fatalf("the pruned state must be written back: %s", b)
	}
	// A slot of the table comes up with its restored owner.
	p.mu.Lock()
	toStart, _ := p.resizeLocked(2)
	p.mu.Unlock()
	if len(toStart) != 2 || p.slots[0].Owner != "acme" || p.slots[0].LastUsed != 42 || p.slots[1].Owner != UnknownOwner {
		t.Fatalf("slots %+v %+v", p.slots[0].SlotState, p.slots[1].SlotState)
	}
}

func TestPing(t *testing.T) {
	p := newTestPool("owner")
	pong := func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("OK")) }
	newDockerAPI(t, p, map[string]http.HandlerFunc{"GET /_ping": pong, "HEAD /_ping": pong})
	if err := p.Ping(context.Background()); err != nil {
		t.Fatal(err)
	}
	newDockerAPI(t, p, map[string]http.HandlerFunc{"GET /_ping": replyStatus(http.StatusInternalServerError), "HEAD /_ping": replyStatus(http.StatusInternalServerError)})
	if err := p.Ping(context.Background()); err == nil {
		t.Fatal("a daemon that does not answer must be reported")
	}
}

func TestRealStatfs(t *testing.T) {
	total, free, err := statfs(t.TempDir())
	if err != nil || total <= 0 || free < 0 || free > total {
		t.Fatalf("statfs: %d %d %v", total, free, err)
	}
	if _, _, err := statfs(filepath.Join(t.TempDir(), "missing")); err == nil {
		t.Fatal("a missing path must be an error")
	}
	if realHost.statfs == nil || realHost.hostname == nil || realHost.executable == nil {
		t.Fatal("the real host must read the machine")
	}
}

func TestHostMemoryCachedProbe(t *testing.T) {
	saved := hostMem
	t.Cleanup(func() { hostMem = saved })
	reads := 0
	hostMem = &memoryProbe{read: func() int64 { reads++; return 8 << 30 }, now: func() time.Time { return time.Unix(1, 0) }}
	p := newTestPool("owner") // no memTotal seam: the host's memory is read
	if got := hostMemory(); got != 8<<30 {
		t.Fatalf("hostMemory %d", got)
	}
	if got := p.hostTotal(); got != 8<<30 || reads != 1 {
		t.Fatalf("hostTotal %d after %d reads: the reading must be cached", got, reads)
	}
}

func TestOpenShim(t *testing.T) {
	p := newTestPool("owner")
	p.env = testHost(t)
	read := func() string {
		f, err := p.openShim()
		if err != nil {
			return "error"
		}
		defer f.Close()
		b := make([]byte, 16)
		n, _ := f.Read(b)
		return string(b[:n])
	}
	if got := read(); got != "error" {
		t.Fatalf("no binary at all: %q", got)
	}
	exe := filepath.Join(t.TempDir(), "pitlane")
	write(t, exe, "from executable")
	p.env.executable = func() (string, error) { return exe, nil }
	if got := read(); got != "from executable" {
		t.Fatalf("without /proc/self/exe the executable path is used: %q", got)
	}
	write(t, p.env.selfExe, "from self")
	if got := read(); got != "from self" {
		t.Fatalf("/proc/self/exe comes first, it survives a self-update: %q", got)
	}
	override := filepath.Join(t.TempDir(), "shim")
	write(t, override, "override")
	p.shimExe = override
	if got := read(); got != "override" {
		t.Fatalf("shimExe: %q", got)
	}
	p.shimExe = ""
	p.env.selfExe = filepath.Join(t.TempDir(), "none")
	p.env.executable = func() (string, error) { return "", errors.New("no") }
	if got := read(); got != "error" {
		t.Fatalf("%q", got)
	}
}

func TestMeasureJobWithoutCgroups(t *testing.T) {
	p := newTestPool("owner", SlotState{Index: 1, Status: statusBusy})
	var logs bytes.Buffer
	p.log = slog.New(slog.NewTextHandler(&logs, nil))
	p.cgroups = fakeRoot(t) // no container has a cgroup there
	newDockerAPI(t, p, map[string]http.HandlerFunc{
		"GET /containers/runner/json":         reply(map[string]any{"Id": runnerCID, "State": map[string]any{"Pid": 4242}}),
		"GET /containers/pitlane-slot-1/json": replyStatus(http.StatusNotFound),
	})
	for range 2 {
		if m := p.MeasureJob(context.Background(), "runner", 1); m != nil {
			t.Fatal("nothing to measure: no meter")
		}
	}
	if n := strings.Count(logs.String(), "cgroups not readable"); n != 1 {
		t.Fatalf("the warning must be logged once, was %d times", n)
	}
	if len(p.meters) != 0 {
		t.Fatalf("meters %v", p.meters)
	}
	// Stop on no meter is harmless.
	var m *Meter
	if u := m.Stop(); u.Runner.AnonPeak != -1 {
		t.Fatalf("usage of no meter: %+v", u)
	}
	m.Begin()
}

func TestLoadStateKeepsFileOnReadError(t *testing.T) {
	p := newTestPool("owner")
	p.cfg.StateFile = filepath.Join(t.TempDir(), "missing.json")
	p.loadState() // no file: nothing restored, no panic
	if len(p.restored) != 0 {
		t.Fatalf("restored %v", p.restored)
	}
}

func TestWriteJSONFileFailures(t *testing.T) {
	dir := t.TempDir()
	if err := writeJSONFile(filepath.Join(dir, "no-such-dir", "x.json"), 1); err == nil {
		t.Fatal("an unwritable directory must be an error")
	}
	if err := writeJSONFile(filepath.Join(dir, "x.json"), make(chan int)); err == nil {
		t.Fatal("a value that cannot be encoded must be an error")
	}
	if err := writeJSONFile(filepath.Join(dir, "x.json"), map[string]int{"a": 1}); err != nil {
		t.Fatal(err)
	}
	if info, err := os.Stat(filepath.Join(dir, "x.json")); err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("state files are private: %v %v", info, err)
	}
}
