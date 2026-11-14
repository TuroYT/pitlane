package pool

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"
)

// fakeSlotOps records the scripts run in slots and returns a canned result.
type fakeSlotOps struct {
	mu      sync.Mutex
	scripts []string
	code    int
	err     error
	wipes   int
	wipeErr error
}

func (f *fakeSlotOps) exec(_ context.Context, _, script string) (string, int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.scripts = append(f.scripts, script)
	return "", f.code, f.err
}

func (f *fakeSlotOps) auditBinfmt(context.Context) {}

func (f *fakeSlotOps) wipe(context.Context, int) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.wipes++
	return f.wipeErr
}

func listDir(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, e := range entries {
		out = append(out, e.Name())
	}
	sort.Strings(out)
	return out
}

func fillWorkDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	for _, f := range []string{"repo/src/main.go", ".hidden", "_actions/checkout/x", pnpmStoreDir + "/v10/files/aa", "_temp/secret"} {
		write(t, filepath.Join(dir, f), "data")
	}
	return dir
}

func runSh(t *testing.T, script string) {
	t.Helper()
	if out, err := exec.Command("sh", "-c", script).CombinedOutput(); err != nil {
		t.Fatalf("%s: %v\n%s", script, err, out)
	}
}

func TestClearDir(t *testing.T) {
	dir := fillWorkDir(t)
	runSh(t, clearDir(dir, "_actions", pnpmStoreDir))
	if got := listDir(t, dir); strings.Join(got, ",") != pnpmStoreDir+",_actions" {
		t.Fatalf("release cleanup must keep only the caches, left %v", got)
	}
	dir = fillWorkDir(t)
	runSh(t, clearDir(dir))
	if got := listDir(t, dir); len(got) != 0 {
		t.Fatalf("wipe must empty the whole work dir, left %v", got)
	}
}

func TestPrepareWipeFailureTakesSlotOutOfService(t *testing.T) {
	ops := &fakeSlotOps{wipeErr: errors.New("volume in use")}
	p := newTestPool("owner", SlotState{Index: 0, Status: "ready", Owner: "other"})
	p.sops = ops
	l, ok := p.Acquire(context.Background(), "acme", 1, false)
	if !ok || !l.wipe {
		t.Fatalf("want a wipe, got %+v", l)
	}
	// Failure path only: success needs Docker (externals copy).
	if err := p.Prepare(context.Background(), l); !errors.Is(err, ErrSlotRebuilding) {
		t.Fatalf("failed wipe must report a rebuild, got %v", err)
	}
	if ops.wipes != 1 || len(ops.scripts) != 0 {
		t.Fatalf("owner change must rebuild from the host, not run scripts in the slot: wipes=%d scripts=%q", ops.wipes, ops.scripts)
	}
	s := p.slots[0]
	if !s.dirty || s.Owner != "other" || s.Status != "error" {
		t.Fatalf("failed wipe must keep the slot dirty with its previous owner, got %+v dirty=%v", s.SlotState, s.dirty)
	}
	if _, ok := p.Acquire(context.Background(), "other", 9, false); ok {
		t.Fatal("a slot whose wipe failed must not be handed out")
	}
}

func TestReleaseStrictRebuildsFromHost(t *testing.T) {
	ops := &fakeSlotOps{}
	p := newTestPool("owner", SlotState{Index: 0, Status: "busy", Owner: StrictOwner})
	p.sops = ops
	p.externals[0] = "sha256:x"
	p.Release(context.Background(), Lease{Slot: 0, Owner: StrictOwner, Strict: true})
	if s := p.slots[0]; ops.wipes != 1 || len(ops.scripts) != 0 || s.Owner != "" || s.Status != "ready" || s.dirty {
		t.Fatalf("strict release: wipes=%d scripts=%q state=%+v dirty=%v", ops.wipes, ops.scripts, s.SlotState, s.dirty)
	}
	if _, ok := p.externals[0]; ok {
		t.Fatal("externals must be copied again after a strict job")
	}

	ops.wipeErr = errors.New("daemon gone")
	p.slots[0].Status, p.slots[0].Owner = "busy", StrictOwner
	p.Release(context.Background(), Lease{Slot: 0, Owner: StrictOwner, Strict: true})
	if s := p.slots[0]; !s.dirty || s.Status != "error" || s.Owner != StrictOwner {
		t.Fatalf("failed strict wipe must leave the slot dirty, got %+v dirty=%v", s.SlotState, s.dirty)
	}
}

func TestReleaseCleanupFailureMarksSlotDirty(t *testing.T) {
	for _, tc := range []struct {
		name string
		ops  *fakeSlotOps
	}{
		{"script failed", &fakeSlotOps{code: 1}},
		{"exec failed", &fakeSlotOps{code: -1, err: errors.New("daemon gone")}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := newTestPool("owner", SlotState{Index: 0, Status: "busy", Owner: "acme", TargetID: 1})
			p.sops = tc.ops
			p.Release(context.Background(), Lease{Slot: 0, Owner: "acme", TargetID: 1})
			s := p.slots[0]
			if !s.dirty || s.Status != "error" || s.Owner != "acme" {
				t.Fatalf("failed cleanup must leave the slot dirty and in error, got %+v dirty=%v", s.SlotState, s.dirty)
			}
			if _, ok := p.Acquire(context.Background(), "acme", 1, false); ok {
				t.Fatal("a slot whose cleanup failed must not be handed out, even to its owner")
			}
		})
	}
}

func TestReleaseSuccess(t *testing.T) {
	ops := &fakeSlotOps{}
	// No owner: the pnpm store sync (which needs Docker) is skipped.
	p := newTestPool("owner", SlotState{Index: 0, Status: "busy"})
	p.sops = ops
	p.Release(context.Background(), Lease{Slot: 0})
	if s := p.slots[0]; s.dirty || s.Status != "ready" {
		t.Fatalf("got %+v dirty=%v", s.SlotState, s.dirty)
	}
	if len(ops.scripts) != 1 || ops.scripts[0] != releaseScript(p.cfg.KeepBuildGB) {
		t.Fatalf("Release ran %q", ops.scripts)
	}
	if strings.Contains(ops.scripts[0], "\ntrue") {
		t.Fatal("the cleanup result must not be masked")
	}
}
