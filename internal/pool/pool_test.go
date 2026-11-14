package pool

import (
	"context"
	"io"
	"log/slog"
	"testing"
)

// newTestPool builds a pool without Docker: Acquire only touches Docker when
// a slot must be wiped, which Prepare does (not tested here).
func newTestPool(scope string, slots ...SlotState) *Pool {
	p := &Pool{log: slog.New(slog.NewTextHandler(io.Discard, nil)), slots: map[int]*slot{}, externals: map[int]string{},
		chowned: map[string]bool{}, restored: map[int]persistedSlot{}, released: make(chan struct{}, 1)}
	p.cfg = Config{CacheScope: scope}
	p.cfg.defaults()
	for _, s := range slots {
		p.slots[s.Index] = &slot{SlotState: s}
	}
	return p
}

func TestAcquirePrefersSameTargetThenOwner(t *testing.T) {
	p := newTestPool("owner",
		SlotState{Index: 0, Status: "ready", Owner: "acme", TargetID: 2},
		SlotState{Index: 1, Status: "ready", Owner: "acme", TargetID: 1},
		SlotState{Index: 2, Status: "ready"},
	)
	l, ok := p.Acquire(context.Background(), "acme", 1, false)
	if !ok || l.Slot != 1 || l.wipe {
		t.Fatalf("want slot 1 without wipe, got %+v ok=%v", l, ok)
	}
	l, ok = p.Acquire(context.Background(), "acme", 3, false)
	if !ok || l.Slot != 0 || l.wipe {
		t.Fatalf("want slot 0 (same owner) without wipe, got %+v", l)
	}
	l, ok = p.Acquire(context.Background(), "acme", 3, false)
	if !ok || l.Slot != 2 || l.wipe {
		t.Fatalf("want empty slot 2, got %+v", l)
	}
	if _, ok := p.Acquire(context.Background(), "acme", 3, false); ok {
		t.Fatal("all slots busy: Acquire must fail")
	}
}

func TestAcquireWipesOnOwnerChange(t *testing.T) {
	p := newTestPool("owner", SlotState{Index: 0, Status: "ready", Owner: "other", TargetID: 9})
	l, ok := p.Acquire(context.Background(), "acme", 1, false)
	if !ok || !l.wipe {
		t.Fatalf("slot of another owner must be wiped, got %+v", l)
	}
	if p.slots[0].Owner != "other" {
		t.Fatal("owner must only change after a successful wipe")
	}
}

func TestAcquireUnknownOwnerFailsClosed(t *testing.T) {
	p := newTestPool("owner", SlotState{Index: 0, Status: "ready", Owner: UnknownOwner})
	if l, _ := p.Acquire(context.Background(), "acme", 1, false); !l.wipe {
		t.Fatal("a slot with an unknown owner must be wiped")
	}
}

func TestAcquireSharedScopeNoWipe(t *testing.T) {
	p := newTestPool("shared", SlotState{Index: 0, Status: "ready", Owner: "other"})
	if l, _ := p.Acquire(context.Background(), "acme", 1, false); l.wipe {
		t.Fatal("shared cache scope must not wipe between owners")
	}
}

func TestAcquireStrict(t *testing.T) {
	p := newTestPool("shared",
		SlotState{Index: 0, Status: "ready", Owner: "acme", TargetID: 1},
		SlotState{Index: 1, Status: "ready"},
	)
	l, ok := p.Acquire(context.Background(), "acme", 1, true)
	if !ok || l.Slot != 1 || l.wipe || !l.Strict || l.Owner != StrictOwner {
		t.Fatalf("strict job must prefer a clean slot, got %+v", l)
	}
	l, _ = p.Acquire(context.Background(), "acme", 1, true)
	if l.Slot != 0 || !l.wipe {
		t.Fatalf("strict job on a used slot (even same owner) must wipe, got %+v", l)
	}
	// A normal job after a strict one must wipe too, whatever the scope.
	p.slots[1].Status = "ready"
	p.slots[1].Owner = StrictOwner
	if l, _ := p.Acquire(context.Background(), "acme", 1, false); !l.wipe {
		t.Fatal("slot left by a strict job must be wiped")
	}
}

func TestAcquireSkipsDirtyRetiredAndPaused(t *testing.T) {
	p := newTestPool("owner",
		SlotState{Index: 0, Status: "ready"},
		SlotState{Index: 1, Status: "ready"},
	)
	p.slots[0].dirty = true
	p.slots[1].retire = true
	if _, ok := p.Acquire(context.Background(), "acme", 1, false); ok {
		t.Fatal("dirty and retiring slots must never be handed out")
	}
	p.slots[1].retire = false
	p.qemuBusy = true
	if _, ok := p.Acquire(context.Background(), "acme", 1, false); ok {
		t.Fatal("no slot may be handed out while QEMU registers")
	}
}

func TestTrustedQEMU(t *testing.T) {
	all := map[string]string{
		"qemu-aarch64": "enabled\ninterpreter /usr/bin/qemu-aarch64\nflags: POCF\noffset 0\nmagic 7f454c460201010000000000000000000200b700\n",
		"qemu-x86_64":  "enabled\ninterpreter /usr/bin/qemu-x86_64\nflags: POCF\noffset 0\nmagic 7f454c4602010100000000000000000002003e00\n",
		"evil":         "enabled\ninterpreter /tmp/x\nflags: F\noffset 0\nmagic 7f454c46\n",
		"qemu-arm":     "enabled\ninterpreter /tmp/qemu-arm\nflags: F\noffset 0\nmagic 7f454c4601010100000000000000000002002800\n",
	}
	got := trustedQEMU(all)
	if _, ok := got["evil"]; ok {
		t.Error("non-QEMU handler trusted")
	}
	if _, ok := got["qemu-arm"]; ok {
		t.Error("QEMU handler with a foreign interpreter path trusted")
	}
	if _, ok := got["qemu-x86_64"]; ok && hostMachine() == "3e00" {
		t.Error("handler matching the host architecture trusted")
	}
	if _, ok := got["qemu-aarch64"]; !ok && hostMachine() != "b700" {
		t.Error("legitimate foreign QEMU handler rejected")
	}
}

func TestCacheVolumeNames(t *testing.T) {
	if v := cacheVolume("owner", "Acme Corp/../x"); v != "pitlane-cache-acme_corp_.._x" {
		t.Fatalf("unsafe owner not sanitized: %s", v)
	}
	if v := cacheVolume("shared", "acme"); v != "pitlane-cache" {
		t.Fatal(v)
	}
	c, tl := strictVolumes("pitlane-1234")
	if c == tl || c == cacheVolume("owner", "pitlane-1234") {
		t.Fatal("strict volumes must be unique per runner")
	}
}
