package pool

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"net/http"
	"slices"
	"strings"
	"testing"
)

// A slot retired while its job ran is removed on release, whichever way it
// is released.
func TestReleaseRemovesRetiredSlot(t *testing.T) {
	for _, tc := range []struct {
		name   string
		strict bool
	}{{"normal", false}, {"strict", true}} {
		t.Run(tc.name, func(t *testing.T) {
			p := newTestPool("owner", SlotState{Index: 2, Status: statusBusy}, SlotState{Index: 0, Status: statusReady})
			p.sops = &fakeSlotOps{}
			p.slots[2].retire = true
			api := newDockerAPI(t, p, map[string]http.HandlerFunc{"DELETE /containers/pitlane-slot-2": replyStatus(http.StatusNoContent)})
			p.Release(context.Background(), Lease{Slot: 2, Strict: tc.strict})
			if _, ok := p.slots[2]; ok || !slices.Contains(api.called(), "DELETE /containers/pitlane-slot-2") {
				t.Fatalf("a retired slot must be removed once its job is released: %v", api.called())
			}
			select {
			case <-p.Released():
			default:
				t.Fatal("the scaler must hear about the freed capacity")
			}
		})
	}
}

// A failed upgrade leaves the slot in error for the retry loop.
func TestReleaseUpgradeFailure(t *testing.T) {
	ops := &fakeSlotOps{recreateErr: errors.New("create dind: no space")}
	p := newTestPool("owner", SlotState{Index: 0, Status: statusBusy, Owner: "acme", TargetID: 1})
	p.sops = ops
	p.slots[0].upgrade = true
	p.Release(context.Background(), Lease{Slot: 0, Owner: "acme", TargetID: 1})
	if s := p.slots[0]; s.Status != statusError || !strings.Contains(s.Error, "no space") {
		t.Fatalf("slot %+v", s.SlotState)
	}
}

// A dirty slot with a pending upgrade is rebuilt (which also upgrades it),
// not recreated with the previous owner's data.
func TestReleaseDirtyWinsOverUpgrade(t *testing.T) {
	ops := &fakeSlotOps{code: 1}
	p := newTestPool("owner", SlotState{Index: 0, Status: statusBusy, Owner: "acme", TargetID: 1})
	p.sops = ops
	p.slots[0].upgrade = true
	p.Release(context.Background(), Lease{Slot: 0, Owner: "acme", TargetID: 1})
	if s := p.slots[0]; s.Status != statusError || !s.dirty || ops.recreates != 0 {
		t.Fatalf("slot %+v dirty=%v recreates=%d", s.SlotState, s.dirty, ops.recreates)
	}
}

// A strict job's throwaway volumes that cannot be removed leave the slot dirty.
func TestReleaseStrictVolumeFailure(t *testing.T) {
	ops := &fakeSlotOps{removeErr: errors.New("volume in use")}
	p := newTestPool("owner", SlotState{Index: 0, Status: statusBusy, Owner: StrictOwner})
	p.sops = ops
	p.chowned["pitlane-strict-pitlane-a-cache"] = true
	p.Release(context.Background(), Lease{Slot: 0, Owner: StrictOwner, Strict: true, Runner: "pitlane-a"})
	s := p.slots[0]
	if !s.dirty || s.Status != statusError {
		t.Fatalf("slot %+v dirty=%v", s.SlotState, s.dirty)
	}
	// Both volumes are tried even when the first fails.
	if len(ops.removed) != 2 || p.chowned["pitlane-strict-pitlane-a-cache"] {
		t.Fatalf("removed %v chowned %v", ops.removed, p.chowned)
	}
}

func TestRemoveStrictVolumesWithoutRunner(t *testing.T) {
	ops := &fakeSlotOps{removeErr: errors.New("boom")}
	p := newTestPool("owner")
	p.sops = ops
	if err := p.removeStrictVolumes(context.Background(), ""); err != nil || len(ops.removed) != 0 {
		t.Fatalf("an unknown runner has nothing to remove: %v %v", err, ops.removed)
	}
}

// The pnpm store of a slot is filled before a job that finds it empty, merged
// after a job that left the slot clean, and never for strict or unowned slots.
func TestPnpmStoreSyncAroundJobs(t *testing.T) {
	t.Run("prepare fills an empty store", func(t *testing.T) {
		ops := &fakeSlotOps{}
		p := newTestPool("owner", SlotState{Index: 0, Status: statusBusy, Owner: "acme", TargetID: 1})
		p.sops = ops
		p.externals[0] = externalsKey("sha256:runner")
		if err := p.Prepare(context.Background(), Lease{Slot: 0, Owner: "acme", TargetID: 1}); err != nil {
			t.Fatal(err)
		}
		if !slices.Equal(ops.merges, []string{"pitlane-cache-acme.pnpm"}) {
			t.Fatalf("merges %v", ops.merges)
		}
		// Merged with its canonical store: the next job does not look again.
		if err := p.Prepare(context.Background(), Lease{Slot: 0, Owner: "acme", TargetID: 1}); err != nil || len(ops.merges) != 1 || ops.stores != 1 {
			t.Fatalf("merges %v store checks %d", ops.merges, ops.stores)
		}
	})
	t.Run("prepare leaves a store that already has packages", func(t *testing.T) {
		ops := &fakeSlotOps{storeFull: true}
		p := newTestPool("owner", SlotState{Index: 0, Status: statusBusy, Owner: "acme"})
		p.sops = ops
		p.externals[0] = externalsKey("sha256:runner")
		_ = p.Prepare(context.Background(), Lease{Slot: 0, Owner: "acme"})
		if len(ops.merges) != 0 {
			t.Fatalf("merges %v", ops.merges)
		}
	})
	t.Run("a strict job never touches a store", func(t *testing.T) {
		ops := &fakeSlotOps{}
		p := newTestPool("owner", SlotState{Index: 0, Status: statusBusy, Owner: StrictOwner})
		p.sops = ops
		p.externals[0] = externalsKey("sha256:runner")
		p.stores.set(0, "pitlane-cache-acme.pnpm")
		_ = p.Prepare(context.Background(), Lease{Slot: 0, Owner: StrictOwner, Strict: true})
		if len(ops.merges) != 0 || p.stores.isSynced(0, "pitlane-cache-acme.pnpm") {
			t.Fatalf("merges %v: a strict slot's store sync state must be forgotten", ops.merges)
		}
	})
	t.Run("release merges the packages of a clean slot", func(t *testing.T) {
		ops := &fakeSlotOps{}
		p := newTestPool("owner", SlotState{Index: 0, Status: statusBusy, Owner: "acme", TargetID: 1})
		p.sops = ops
		p.Release(context.Background(), Lease{Slot: 0, Owner: "acme", TargetID: 1})
		if !slices.Equal(ops.merges, []string{"pitlane-cache-acme.pnpm"}) || p.slots[0].Status != statusReady {
			t.Fatalf("merges %v slot %+v", ops.merges, p.slots[0].SlotState)
		}
	})
	t.Run("release does not merge a dirty slot", func(t *testing.T) {
		ops := &fakeSlotOps{code: 1}
		p := newTestPool("owner", SlotState{Index: 0, Status: statusBusy, Owner: "acme", TargetID: 1})
		p.sops = ops
		p.Release(context.Background(), Lease{Slot: 0, Owner: "acme", TargetID: 1})
		if len(ops.merges) != 0 {
			t.Fatalf("a slot whose cleanup failed was merged into the canonical store: %v", ops.merges)
		}
	})
	t.Run("no owner, no store", func(t *testing.T) {
		ops := &fakeSlotOps{}
		p := newTestPool("owner")
		for _, o := range []string{"", StrictOwner, UnknownOwner} {
			p.syncPnpmStore(context.Background(), 0, o)
		}
		if len(ops.merges) != 0 {
			t.Fatal("merged for a slot without a trusted owner")
		}
	})
}

// A merge that fails forgets the sync state; the missing host access is only
// reported once.
func TestSyncPnpmStoreFailure(t *testing.T) {
	var logs bytes.Buffer
	ops := &fakeSlotOps{mergeErr: errNoHostAccess}
	p := newTestPool("owner")
	p.log = slog.New(slog.NewTextHandler(&logs, nil))
	p.sops = ops
	p.stores.set(0, "pitlane-cache-acme.pnpm")
	p.syncPnpmStore(context.Background(), 0, "acme")
	p.syncPnpmStore(context.Background(), 0, "acme")
	if p.stores.isSynced(0, "pitlane-cache-acme.pnpm") {
		t.Fatal("a failed merge must not leave the slot marked as synced")
	}
	if n := strings.Count(logs.String(), "pnpm store not shared"); n != 1 {
		t.Fatalf("host access failure reported %d times, want once:\n%s", n, logs.String())
	}
	ops.mergeErr = errors.New("different filesystems")
	p.syncPnpmStore(context.Background(), 0, "acme")
	p.syncPnpmStore(context.Background(), 0, "acme")
	if n := strings.Count(logs.String(), "pnpm store not shared"); n != 3 {
		t.Fatalf("other failures are always reported, logged %d times", n)
	}
}

func TestForgetStore(t *testing.T) {
	p := newTestPool("owner")
	p.stores.set(1, "v")
	p.forgetStore(1)
	if p.stores.isSynced(1, "v") {
		t.Fatal("forgotten store still synced")
	}
}
