package pool

import (
	"context"
	"errors"
	"fmt"
	"net/http"
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
