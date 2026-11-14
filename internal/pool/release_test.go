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
	removed []string
	// externals and pnpm store
	image     string
	copies    int
	copyErr   error
	merges    []string
	recreates int
}

func (f *fakeSlotOps) removeVolume(_ context.Context, name string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.removed = append(f.removed, name)
	return nil
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

func (f *fakeSlotOps) recreate(context.Context, int) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.recreates++
	return nil
}

func (f *fakeSlotOps) imageID(context.Context, string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.image == "" {
		return "sha256:runner", nil
	}
	return f.image, nil
}

func (f *fakeSlotOps) copyExternals(context.Context, int, string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.copies++
	return f.copyErr
}

func (f *fakeSlotOps) slotStoreEmpty(context.Context, int) bool { return true }

func (f *fakeSlotOps) mergeStore(_ context.Context, _ int, vol string) (mergeStats, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.merges = append(f.merges, vol)
	return mergeStats{}, nil
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

// A runner adopted after a restart must be released the way it was started:
// a strict one (or one from an older version, without the label) rebuilds
// its slot even with the shared cache scope, and its slot is never handed
// to another owner meanwhile.
func TestAdoptedStrictRunnerKeepsStrictRelease(t *testing.T) {
	for _, tc := range []struct {
		name   string
		labels map[string]string
		strict bool
	}{
		{"strict", map[string]string{LabelStrict: "true"}, true},
		{"older version without label", map[string]string{}, true},
		{"unreadable label", map[string]string{LabelStrict: "yes"}, true},
		{"not strict", map[string]string{LabelStrict: "false"}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ops := &fakeSlotOps{}
			p := newTestPool("shared", SlotState{Index: 0, Status: "ready"}, SlotState{Index: 1, Status: "ready", Owner: "other"})
			p.sops = ops
			tc.labels[LabelSlot], tc.labels[LabelTarget], tc.labels[LabelRunner], tc.labels[LabelOwner] = "0", "7", "pitlane-abc", "acme"
			o := p.adoptContainer("cid", tc.labels, "running", 0)
			l := o.Lease()
			if o.Strict != tc.strict || l.Strict != tc.strict || l.Runner != "pitlane-abc" || l.TargetID != 7 {
				t.Fatalf("orphan %+v lease %+v", o, l)
			}
			wantOwner := "acme"
			if tc.strict {
				wantOwner = StrictOwner
			}
			if s := p.slots[0]; s.Owner != wantOwner || s.Status != "busy" || l.Owner != wantOwner {
				t.Fatalf("adopted slot %+v lease owner %q, want owner %q", s.SlotState, l.Owner, wantOwner)
			}
			if !tc.strict {
				return // normal Release (covered elsewhere; its pnpm sync needs Docker)
			}
			p.Release(context.Background(), l)
			if ops.wipes != 1 || len(ops.scripts) != 0 || p.slots[0].Owner != "" {
				t.Fatalf("strict runner must rebuild its slot: wipes=%d scripts=%d owner=%q", ops.wipes, len(ops.scripts), p.slots[0].Owner)
			}
			cache, tools := strictVolumes("pitlane-abc")
			if strings.Join(ops.removed, ",") != cache+","+tools {
				t.Fatalf("throwaway caches not removed: %v", ops.removed)
			}
		})
	}
}

// A runner container whose target label is missing or invalid (older
// version, label lost) is untrusted: adopted as strict for no target, so its
// slot is rebuilt. Without a valid slot label no slot is touched.
func TestAdoptedRunnerWithoutTarget(t *testing.T) {
	for _, tc := range []struct {
		name, target string
	}{{"missing", ""}, {"invalid", "x"}, {"zero", "0"}, {"negative", "-3"}} {
		t.Run(tc.name, func(t *testing.T) {
			p := newTestPool("shared", SlotState{Index: 0, Status: "ready"})
			labels := map[string]string{LabelSlot: "0", LabelRunner: "pitlane-abc", LabelOwner: "acme", LabelStrict: "false"}
			if tc.target != "" {
				labels[LabelTarget] = tc.target
			}
			o := p.adoptContainer("cid", labels, "running", 0)
			l := o.Lease()
			if !o.Untrusted || !o.Strict || o.TargetID != 0 || !l.Strict || l.Owner != StrictOwner || l.TargetID != 0 {
				t.Fatalf("orphan %+v lease %+v", o, l)
			}
			if s := p.slots[0]; s.Status != "busy" || s.Owner != StrictOwner {
				t.Fatalf("slot of an untrusted runner must stay busy for the strict owner: %+v", s.SlotState)
			}
		})
	}
	t.Run("valid target", func(t *testing.T) {
		p := newTestPool("shared", SlotState{Index: 0, Status: "ready"})
		o := p.adoptContainer("cid", map[string]string{LabelSlot: "0", LabelTarget: "7", LabelStrict: "false"}, "running", 0)
		if o.Untrusted || o.Strict || o.TargetID != 7 {
			t.Fatalf("orphan %+v", o)
		}
	})
	t.Run("no slot label", func(t *testing.T) {
		p := newTestPool("shared", SlotState{Index: 0, Status: "ready"})
		o := p.adoptContainer("cid", map[string]string{LabelTarget: "7"}, "running", 0)
		if o.Slot != -1 || p.slots[0].Status != "ready" || len(p.slots) != 1 {
			t.Fatalf("orphan %+v slots %+v", o, p.slots)
		}
	})
}
