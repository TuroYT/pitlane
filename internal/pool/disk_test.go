package pool

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"path/filepath"
	"strings"
	"testing"
)

// fakeDisk simulates the disk: every prune action frees a known amount.
type fakeDisk struct {
	free    int64
	freed   map[string]int64 // "slot/what" or volume name -> bytes freed
	vols    []volInfo
	calls   []string
	freeErr error
	otherFS bool // actions cache on another filesystem
	p       *Pool
	// observed slot states while a slot is being pruned
	statusDuring map[int]string
}

func (f *fakeDisk) freeBytes() (int64, error) { return f.free, f.freeErr }

func (f *fakeDisk) execSlot(_ context.Context, slot int, script string) error {
	what := "image"
	if strings.Contains(script, "builder prune") {
		what = "build"
	}
	key := fmt.Sprintf("%d/%s", slot, what)
	f.calls = append(f.calls, key)
	f.p.mu.Lock()
	f.statusDuring[slot] = f.p.slots[slot].Status
	f.p.mu.Unlock()
	f.free += f.freed[key]
	return nil
}

func (f *fakeDisk) onDisk(string) bool { return !f.otherFS }

func (f *fakeDisk) volumes(context.Context) ([]volInfo, error) { return f.vols, nil }

func (f *fakeDisk) removeVolume(_ context.Context, name string) error {
	f.calls = append(f.calls, "vol:"+name)
	f.free += f.freed[name]
	return nil
}

const gib = int64(1) << 30

func newFake(p *Pool, free int64) *fakeDisk {
	return &fakeDisk{p: p, free: free, freed: map[string]int64{}, statusDuring: map[int]string{}}
}

func TestReclaimDiskNothingWhenEnoughSpace(t *testing.T) {
	p := newTestPool("owner", SlotState{Index: 0, Status: "ready"})
	f := newFake(p, 30*gib)
	if n := p.reclaimDisk(context.Background(), 20*gib, f); n != 0 || len(f.calls) != 0 {
		t.Fatalf("pruned with enough space: %v", f.calls)
	}
}

func TestReclaimDiskOrderAndStopsWhenAbove(t *testing.T) {
	p := newTestPool("owner",
		SlotState{Index: 0, Status: "busy", Owner: "acme"},
		SlotState{Index: 1, Status: "ready", Owner: "acme"},
		SlotState{Index: 2, Status: "ready"},
		SlotState{Index: 3, Status: "cleaning"},
	)
	f := newFake(p, 10*gib)
	f.freed["1/build"] = 5 * gib
	f.freed["2/build"] = 3 * gib
	f.freed["1/image"] = 5 * gib
	f.vols = []volInfo{{Name: "pitlane-cache-acme", Size: 50 * gib}}
	n := p.reclaimDisk(context.Background(), 20*gib, f)
	want := []string{"1/build", "2/build", "1/image"}
	if strings.Join(f.calls, ",") != strings.Join(want, ",") {
		t.Fatalf("calls %v, want %v", f.calls, want)
	}
	if n != 13*gib {
		t.Fatalf("freed %d", n)
	}
	for slot, st := range f.statusDuring {
		if st != "cleaning" {
			t.Errorf("slot %d was %q while pruned: it could have been handed out", slot, st)
		}
	}
	for i, want := range map[int]string{0: "busy", 1: "ready", 2: "ready", 3: "cleaning"} {
		if got := p.slots[i].Status; got != want {
			t.Errorf("slot %d status %q, want %q", i, got, want)
		}
	}
}

func TestReclaimDiskNeverTouchesBusyDirtyOrRetiringSlots(t *testing.T) {
	p := newTestPool("owner",
		SlotState{Index: 0, Status: "busy"},
		SlotState{Index: 1, Status: "ready"},
		SlotState{Index: 2, Status: "starting"},
		SlotState{Index: 3, Status: "error"},
	)
	p.slots[1].dirty = true
	f := newFake(p, 1*gib)
	p.reclaimDisk(context.Background(), 20*gib, f)
	for _, c := range f.calls {
		if !strings.HasPrefix(c, "vol:") {
			t.Fatalf("pruned a slot that is not idle: %s", c)
		}
	}
}

func TestReclaimDiskVolumesLargestUnusedFirst(t *testing.T) {
	p := newTestPool("owner")
	p.chowned["pitlane-cache-big"] = true
	f := newFake(p, 1*gib)
	f.vols = []volInfo{
		{Name: "pitlane-cache-used", Size: 90 * gib, InUse: true},
		{Name: "pitlane-cache-small", Size: 2 * gib},
		{Name: "pitlane-cache-big", Size: 10 * gib},
		{Name: "pitlane-toolcache-mid", Size: 5 * gib},
		{Name: "pitlane-slot-0-docker", Size: 80 * gib},
		{Name: "pitlane-slot-0-work", Size: 70 * gib},
		{Name: "pitlane-strict-x-cache", Size: 60 * gib},
		{Name: "pitlane-mirror-data", Size: 40 * gib},
		{Name: "pitlane-cache", Size: 30 * gib}, // shared scope volume: not per-owner
	}
	f.freed["pitlane-cache-big"] = 10 * gib
	f.freed["pitlane-toolcache-mid"] = 5 * gib
	f.freed["pitlane-cache-small"] = 2 * gib
	p.reclaimDisk(context.Background(), 14*gib, f)
	want := []string{"vol:pitlane-cache-big", "vol:pitlane-toolcache-mid"}
	if strings.Join(f.calls, ",") != strings.Join(want, ",") {
		t.Fatalf("calls %v, want %v", f.calls, want)
	}
	if p.chowned["pitlane-cache-big"] {
		t.Fatal("removed volume still marked as prepared: it would be recreated root-owned")
	}
}

func TestReclaimDiskStopsWhenSpaceUnknown(t *testing.T) {
	p := newTestPool("owner", SlotState{Index: 0, Status: "ready"})
	f := newFake(p, 1*gib)
	f.freeErr = errors.New("statfs failed")
	if p.reclaimDisk(context.Background(), 20*gib, f); len(f.calls) != 0 {
		t.Fatalf("pruned without knowing free space: %v", f.calls)
	}
}

func TestEnsureFreeDiskDisabled(t *testing.T) {
	p := newTestPool("owner")
	p.cfg.MinFreeDiskGB = 0
	p.EnsureFreeDisk(context.Background()) // must not touch Docker (nil client)
}

// dockerDiskOps against a fake Docker API: only managed volumes are listed,
// in use when a container mounts them or Docker counts a reference.
func TestDockerDiskOpsVolumes(t *testing.T) {
	p := newTestPool("owner")
	managed := map[string]string{LabelManaged: "true"}
	newDockerAPI(t, p, map[string]http.HandlerFunc{
		"GET /system/df": reply(map[string]any{"Volumes": []map[string]any{
			{"Name": "pitlane-cache-a", "Labels": managed, "UsageData": map[string]any{"Size": 10, "RefCount": 0}},
			{"Name": "pitlane-cache-b", "Labels": managed, "UsageData": map[string]any{"Size": 20, "RefCount": 0}},
			{"Name": "pitlane-cache-c", "Labels": managed, "UsageData": map[string]any{"Size": 30, "RefCount": 1}},
			{"Name": "pitlane-cache-d", "Labels": managed},
			{"Name": "other", "Labels": map[string]string{}},
		}}),
		"GET /containers/json": reply([]map[string]any{{"Id": "c1", "Mounts": []map[string]any{{"Name": "pitlane-cache-b"}}}}),
	})
	vols, err := dockerDiskOps{p}.volumes(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	want := []volInfo{{"pitlane-cache-a", 10, false}, {"pitlane-cache-b", 20, true}, {"pitlane-cache-c", 30, true}, {"pitlane-cache-d", -1, false}}
	if fmt.Sprint(vols) != fmt.Sprint(want) {
		t.Fatalf("volumes: %v", vols)
	}
}

// A slot prune that fails is logged and the run goes on with the next slot.
func TestReclaimDiskSlotPruneFailureGoesOn(t *testing.T) {
	p := newTestPool("owner",
		SlotState{Index: 0, Status: "ready"},
		SlotState{Index: 1, Status: "ready"},
	)
	f := &failingDisk{fakeDisk: newFake(p, 10*gib), failSlot: 0}
	f.freed["1/build"] = 15 * gib
	n := p.reclaimDisk(context.Background(), 20*gib, f)
	if n != 15*gib || strings.Join(f.calls, ",") != "0/build,1/build" {
		t.Fatalf("freed %d, calls %v", n, f.calls)
	}
	if p.slots[0].Status != "ready" {
		t.Fatalf("a slot whose prune failed must be released again, is %q", p.slots[0].Status)
	}
}

type failingDisk struct {
	*fakeDisk
	failSlot int
}

func (f *failingDisk) execSlot(ctx context.Context, slot int, script string) error {
	if err := f.fakeDisk.execSlot(ctx, slot, script); err != nil {
		return err
	}
	if slot == f.failSlot {
		return errExit(1)
	}
	return nil
}

// A slot retired while it was being pruned is removed, not handed out again.
func TestUnclaimRemovesSlotRetiredMeanwhile(t *testing.T) {
	p := newTestPool("owner", SlotState{Index: 0, Status: "ready"}, SlotState{Index: 1, Status: "ready"})
	routes, _ := execRoutes(t, func(_, _ string) (string, int) { return "", 0 })
	routes["DELETE /containers/pitlane-slot-0"] = replyStatus(http.StatusNoContent)
	newDockerAPI(t, p, routes)
	if !p.claimIdle(0) || !p.claimIdle(1) {
		t.Fatal("idle slots must be claimable")
	}
	if p.claimIdle(0) {
		t.Fatal("a slot being pruned must not be claimed twice")
	}
	p.slots[0].retire = true
	p.unclaim(context.Background(), 0)
	p.unclaim(context.Background(), 1)
	p.unclaim(context.Background(), 9) // gone meanwhile: nothing to do
	if _, ok := p.slots[0]; ok {
		t.Fatal("a slot retired during its prune must be removed")
	}
	if p.slots[1].Status != "ready" {
		t.Fatalf("slot 1 is %q", p.slots[1].Status)
	}
}

func TestIdleSlotsOnlyReadyAndClean(t *testing.T) {
	p := newTestPool("owner",
		SlotState{Index: 3, Status: "ready"},
		SlotState{Index: 1, Status: "ready"},
		SlotState{Index: 2, Status: "ready"},
		SlotState{Index: 4, Status: "busy"},
	)
	p.slots[2].retire = true
	got := p.idleSlots()
	if len(got) != 2 || got[0] != 1 || got[1] != 3 {
		t.Fatalf("idle slots %v, want [1 3]", got)
	}
}

// The volume pass stops as soon as there is enough space, keeps going past a
// volume Docker refuses to remove, and gives up when it cannot list them.
func TestReclaimDiskVolumeFailures(t *testing.T) {
	p := newTestPool("owner")
	f := &volFailDisk{fakeDisk: newFake(p, 1*gib), fail: map[string]bool{"pitlane-cache-big": true}}
	f.vols = []volInfo{{Name: "pitlane-cache-big", Size: 10 * gib}, {Name: "pitlane-cache-mid", Size: 5 * gib}, {Name: "pitlane-cache-small", Size: 1 * gib}}
	f.freed["pitlane-cache-mid"] = 30 * gib
	n := p.reclaimDisk(context.Background(), 20*gib, f)
	if got := strings.Join(f.calls, ","); got != "vol:pitlane-cache-big,vol:pitlane-cache-mid" || n != 30*gib {
		t.Fatalf("calls %v freed %d: must go past a failed removal and stop once there is room", f.calls, n)
	}

	f = &volFailDisk{fakeDisk: newFake(p, 1*gib), listErr: errors.New("daemon down")}
	f.vols = []volInfo{{Name: "pitlane-cache-big", Size: 10 * gib}}
	if p.reclaimDisk(context.Background(), 20*gib, f); len(f.calls) != 0 {
		t.Fatalf("removed volumes it could not list: %v", f.calls)
	}
}

type volFailDisk struct {
	*fakeDisk
	fail    map[string]bool
	listErr error
}

func (f *volFailDisk) volumes(ctx context.Context) ([]volInfo, error) {
	if f.listErr != nil {
		return nil, f.listErr
	}
	return f.fakeDisk.volumes(ctx)
}

func (f *volFailDisk) removeVolume(ctx context.Context, name string) error {
	if f.fail[name] {
		f.calls = append(f.calls, "vol:"+name)
		return errors.New("volume in use")
	}
	return f.fakeDisk.removeVolume(ctx, name)
}

// Free space becoming unknown between two steps stops the run.
func TestReclaimDiskStopsWhenSpaceBecomesUnknown(t *testing.T) {
	p := newTestPool("owner", SlotState{Index: 0, Status: "ready"}, SlotState{Index: 1, Status: "ready"})
	f := &flakyFree{fakeDisk: newFake(p, 1*gib)}
	f.vols = []volInfo{{Name: "pitlane-cache-big", Size: 10 * gib}}
	p.reclaimDisk(context.Background(), 20*gib, f)
	if got := strings.Join(f.calls, ","); got != "0/build" {
		t.Fatalf("calls %v: pruning must stop once the free space is unknown", f.calls)
	}
}

// flakyFree answers the first freeBytes, then fails.
type flakyFree struct {
	*fakeDisk
	asked int
}

func (f *flakyFree) freeBytes() (int64, error) {
	f.asked++
	if f.asked > 1 {
		return 0, errors.New("statfs failed")
	}
	return f.fakeDisk.free, nil
}

func TestEnsureFreeDisk(t *testing.T) {
	p := newTestPool("owner")
	p.env = testHost(t)
	p.cfg.MinFreeDiskGB = 20
	p.env.statfs = func(string) (int64, int64, error) { return 100 * gib, 50 * gib, nil }
	p.EnsureFreeDisk(context.Background()) // enough space: nothing to do, Docker never asked

	// A reclaim already running makes the call return at once.
	p.env.statfs = func(string) (int64, int64, error) {
		t.Error("statfs called while another reclaim runs")
		return 0, 0, nil
	}
	p.pruneMu.Lock()
	p.EnsureFreeDisk(context.Background())
	p.pruneMu.Unlock()
}

func TestDockerDiskOps(t *testing.T) {
	p := newTestPool("owner")
	p.env = testHost(t)
	p.env.statfs = func(string) (int64, int64, error) { return 100, 40, nil }
	d := dockerDiskOps{p}
	if free, err := d.freeBytes(); err != nil || free != 40 {
		t.Fatalf("freeBytes: %d %v", free, err)
	}
	p.env.statfs = func(string) (int64, int64, error) { return 0, 0, errors.New("boom") }
	if _, err := d.freeBytes(); err == nil {
		t.Fatal("a statfs failure must be reported")
	}

	// A script's exit code is an error, as is a failed exec.
	for _, tc := range []struct {
		name string
		code int
		want string
	}{{"ok", 0, ""}, {"failed", 2, "exit code 2"}, {"no exec", -1, "fake error"}} {
		routes, _ := execRoutes(t, func(_, _ string) (string, int) { return "", tc.code }, "pitlane-slot-3")
		newDockerAPI(t, p, routes)
		err := d.execSlot(context.Background(), 3, "docker builder prune")
		if tc.want == "" && err != nil || tc.want != "" && (err == nil || !strings.Contains(err.Error(), tc.want)) {
			t.Fatalf("%s: %v", tc.name, err)
		}
	}
}

// removeVolume is not forced, and a volume already gone is a success.
func TestDockerDiskOpsRemoveVolume(t *testing.T) {
	for _, tc := range []struct {
		name    string
		status  int
		wantErr bool
	}{{"removed", http.StatusNoContent, false}, {"already gone", http.StatusNotFound, false}, {"in use", http.StatusConflict, true}, {"daemon error", http.StatusInternalServerError, true}} {
		t.Run(tc.name, func(t *testing.T) {
			p := newTestPool("owner")
			var force string
			newDockerAPI(t, p, map[string]http.HandlerFunc{"DELETE /volumes/pitlane-cache-a": func(w http.ResponseWriter, r *http.Request) {
				force = r.URL.Query().Get("force")
				replyStatus(tc.status)(w, r)
			}})
			err := dockerDiskOps{p}.removeVolume(context.Background(), "pitlane-cache-a")
			if (err != nil) != tc.wantErr {
				t.Fatalf("error %v, want error: %v", err, tc.wantErr)
			}
			if force == "1" || force == "true" {
				t.Fatal("a disk-pressure removal must not be forced: Docker has to refuse a volume in use")
			}
		})
	}
}

func TestDockerDiskOpsVolumesErrors(t *testing.T) {
	p := newTestPool("owner")
	newDockerAPI(t, p, map[string]http.HandlerFunc{"GET /system/df": replyStatus(http.StatusInternalServerError)})
	if _, err := (dockerDiskOps{p}).volumes(context.Background()); err == nil {
		t.Fatal("a failed disk usage call must be reported")
	}
	newDockerAPI(t, p, map[string]http.HandlerFunc{
		"GET /system/df":       reply(map[string]any{"Volumes": []any{}}),
		"GET /containers/json": replyStatus(http.StatusInternalServerError),
	})
	if _, err := (dockerDiskOps{p}).volumes(context.Background()); err == nil {
		t.Fatal("without the container list a volume cannot be called unused: report it")
	}
}

func TestDockerDiskOpsOnDisk(t *testing.T) {
	p := newTestPool("owner")
	p.env = testHost(t)
	dir := t.TempDir()
	p.cfg.StateFile = filepath.Join(dir, "slots.json")
	d := dockerDiskOps{p}
	if !d.onDisk(dir) {
		t.Fatal("a directory next to the state file is on the watched filesystem")
	}
	if d.onDisk(filepath.Join(dir, "missing")) {
		t.Fatal("a directory that cannot be statted is not known to be on the disk")
	}
}

func TestDiskPathFallbacks(t *testing.T) {
	p := newTestPool("owner")
	p.env = testHost(t)
	if got := p.DiskPath(); got != "." {
		t.Fatalf("no Docker directory and no state file: %q", got)
	}
	p.env.dockerRoot = ""
	p.cfg.StateFile = "/data/slots.json"
	if got := p.DiskPath(); got != "/data" {
		t.Fatalf("got %q", got)
	}
}

func TestDockerDiskUsageOfSlotVolumes(t *testing.T) {
	p := newTestPool("owner")
	managed := map[string]string{LabelManaged: "true"}
	newDockerAPI(t, p, map[string]http.HandlerFunc{"GET /system/df": reply(map[string]any{"Volumes": []map[string]any{
		{"Name": "pitlane-slot-0-docker", "Labels": managed, "UsageData": map[string]any{"Size": 7, "RefCount": 1}},
		{"Name": "pitlane-slot-1-docker", "Labels": managed},
		{"Name": "foreign", "Labels": map[string]string{}, "UsageData": map[string]any{"Size": 9}},
	}})})
	got := p.DiskUsage(context.Background())
	if len(got) != 1 || got["pitlane-slot-0-docker"] != 7 {
		t.Fatalf("disk usage %v", got)
	}
	newDockerAPI(t, p, map[string]http.HandlerFunc{"GET /system/df": replyStatus(http.StatusInternalServerError)})
	if got := p.DiskUsage(context.Background()); len(got) != 0 {
		t.Fatalf("on error: %v", got)
	}
}
