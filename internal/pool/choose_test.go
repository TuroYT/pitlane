package pool

import (
	"context"
	"io"
	"log/slog"
	"path/filepath"
	"testing"
	"time"
)

// ts is a test slot: a state plus the flags Acquire looks at.
type ts struct {
	idx      int
	status   string
	owner    string
	target   int64
	lastUsed int64
	wiping   bool
}

func choosePool(scope string, slots []ts) *Pool {
	p := newTestPool(scope)
	for _, s := range slots {
		p.slots[s.idx] = &slot{SlotState: SlotState{Index: s.idx, Status: s.status, Owner: s.owner, TargetID: s.target, LastUsed: s.lastUsed}, wiping: s.wiping}
	}
	return p
}

// Which slot a job gets, by expected cache loss.
func TestAcquireChoosesCheapestSlot(t *testing.T) {
	const r, b = "ready", "busy"
	for _, tc := range []struct {
		name     string
		scope    string
		strict   bool
		slots    []ts
		want     int
		wantWipe bool
	}{
		{name: "same target first", scope: "owner",
			slots: []ts{{0, r, "acme", 2, 0, false}, {1, r, "", 0, 0, false}, {2, r, "acme", 1, 0, false}},
			want:  2},
		{name: "same owner before an empty slot", scope: "owner",
			slots: []ts{{0, r, "", 0, 0, false}, {1, r, "acme", 7, 0, false}},
			want:  1},
		{name: "empty slot before any wipe", scope: "owner",
			slots: []ts{{0, r, "org", 5, 0, false}, {1, r, UnknownOwner, 0, 0, false}, {2, r, "", 0, 0, false}},
			want:  2},
		{name: "unknown owner before a warm slot", scope: "owner",
			slots: []ts{{0, r, "org", 5, 0, false}, {1, r, "org", 5, 0, false}, {2, r, UnknownOwner, 0, 9, false}},
			want:  2, wantWipe: true},
		{name: "slot left by a strict job before a warm slot", scope: "owner",
			slots: []ts{{0, r, "org", 5, 0, false}, {1, r, "org", 5, 0, false}, {2, r, StrictOwner, 0, 9, false}},
			want:  2, wantWipe: true},
		{name: "an owner's last warm slot is kept", scope: "owner",
			// priv's only slot is the oldest and the lowest index; org keeps
			// slot 2 (busy) if slot 1 goes.
			slots: []ts{{0, r, "priv", 1, 100, false}, {1, r, "org", 5, 900, false}, {2, b, "org", 5, 0, false}},
			want:  1, wantWipe: true},
		{name: "least recently used among owners that keep a slot", scope: "owner",
			slots: []ts{{0, r, "org", 5, 300, false}, {1, r, "pub", 6, 100, false}, {2, b, "org", 5, 0, false}, {3, b, "pub", 6, 0, false}},
			want:  1, wantWipe: true},
		{name: "unknown last use counts as oldest", scope: "owner",
			slots: []ts{{0, r, "org", 5, 100, false}, {1, r, "org", 5, 0, false}},
			want:  1, wantWipe: true},
		{name: "index breaks ties", scope: "owner",
			slots: []ts{{3, r, "org", 5, 100, false}, {1, r, "org", 5, 100, false}},
			want:  1, wantWipe: true},
		{name: "every candidate is a last warm slot: least recently used", scope: "owner",
			slots: []ts{{0, r, "a", 1, 500, false}, {1, r, "b", 2, 200, false}, {2, r, "c", 3, 300, false}},
			want:  1, wantWipe: true},
		{name: "a slot about to be wiped no longer counts as warm", scope: "owner",
			// priv's slot 0 is leased for another owner: slot 1 is now priv's
			// last one, org keeps slot 3.
			slots: []ts{{0, b, "priv", 1, 0, true}, {1, r, "priv", 1, 100, false}, {2, r, "org", 5, 900, false}, {3, b, "org", 5, 0, false}},
			want:  2, wantWipe: true},
		{name: "shared scope: another owner's slot before a wipe", scope: "shared",
			slots: []ts{{0, r, UnknownOwner, 0, 0, false}, {1, r, "org", 5, 0, false}},
			want:  1},
		{name: "shared scope: empty slot before another owner's", scope: "shared",
			slots: []ts{{0, r, "org", 5, 0, false}, {1, r, "", 0, 0, false}},
			want:  1},
		{name: "strict: empty slot first", scope: "owner", strict: true,
			slots: []ts{{0, r, "acme", 1, 0, false}, {1, r, UnknownOwner, 0, 0, false}, {2, r, "", 0, 0, false}},
			want:  2},
		{name: "strict: unknown owner before a warm slot", scope: "owner", strict: true,
			slots: []ts{{0, r, "acme", 1, 0, false}, {1, r, UnknownOwner, 0, 9, false}},
			want:  1, wantWipe: true},
		{name: "strict: warm slot of an owner that keeps another, least recently used", scope: "shared", strict: true,
			slots: []ts{{0, r, "priv", 1, 100, false}, {1, r, "org", 5, 800, false}, {2, r, "org", 5, 700, false}, {3, b, "priv", 1, 0, false}},
			want:  0, wantWipe: true},
		{name: "strict: an owner's last warm slot goes last", scope: "owner", strict: true,
			slots: []ts{{0, r, "priv", 1, 100, false}, {1, r, "org", 5, 900, false}, {2, b, "org", 5, 0, false}},
			want:  1, wantWipe: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := choosePool(tc.scope, tc.slots)
			l, ok := p.Acquire(context.Background(), "acme", 1, tc.strict)
			if !ok {
				t.Fatal("no slot handed out")
			}
			if l.Slot != tc.want || l.wipe != tc.wantWipe {
				t.Fatalf("got slot %d wipe=%v, want slot %d wipe=%v", l.Slot, l.wipe, tc.want, tc.wantWipe)
			}
			s := p.slots[l.Slot]
			if s.Status != "busy" || s.wiping != tc.wantWipe {
				t.Fatalf("leased slot %+v wiping=%v", s.SlotState, s.wiping)
			}
		})
	}
}

// Two jobs of another owner in a row never take both slots of an owner: the
// first lease marks its slot as being wiped.
func TestAcquireSpreadsWipesAcrossOwners(t *testing.T) {
	p := choosePool("owner", []ts{
		{0, "ready", "priv", 1, 100, false}, {1, "ready", "priv", 1, 200, false},
		{2, "ready", "org", 5, 900, false}, {3, "busy", "org", 5, 0, false},
	})
	var got []int
	for range 2 {
		l, ok := p.Acquire(context.Background(), "pub", 9, false)
		if !ok || !l.wipe {
			t.Fatalf("lease %+v ok=%v", l, ok)
		}
		got = append(got, l.Slot)
	}
	// priv keeps slot 1, org keeps slot 3.
	if got[0] != 0 || got[1] != 2 {
		t.Fatalf("wiped slots %v, want [0 2]", got)
	}
}

// A strict job waits (bounded) for a slot being cleaned after a strict job
// rather than wipe a warm slot.
func TestAcquireStrictWaitsForCleanSlot(t *testing.T) {
	now := time.Unix(1_000_000, 0)
	p := choosePool("owner", []ts{
		{0, "ready", "priv", 1, 100, false},
		{1, "cleaning", StrictOwner, 0, 0, false},
	})
	p.now = func() time.Time { return now }
	if _, ok := p.Acquire(context.Background(), "pub", 9, true); ok {
		t.Fatal("strict job wiped a warm slot while a clean one is coming")
	}
	// A normal job is never held back.
	if l, ok := p.Acquire(context.Background(), "org", 5, false); !ok || l.Slot != 0 {
		t.Fatalf("normal job: %+v ok=%v", l, ok)
	}
	p.slots[0].Status, p.slots[0].Owner, p.slots[0].wiping = "ready", "priv", false
	now = now.Add(strictWait - time.Second)
	if _, ok := p.Acquire(context.Background(), "pub", 9, true); ok {
		t.Fatal("still within the wait")
	}
	// The cleanup is done: the clean slot, no wipe.
	p.slots[1].Status, p.slots[1].Owner = "ready", ""
	l, ok := p.Acquire(context.Background(), "pub", 9, true)
	if !ok || l.Slot != 1 || l.wipe {
		t.Fatalf("strict job: %+v ok=%v", l, ok)
	}
	if !p.strictWaitSince.IsZero() {
		t.Fatal("wait not reset after a strict lease")
	}

	// The cleanup takes too long: after strictWait the warm slot goes.
	p.slots[1].Status, p.slots[1].Owner = "cleaning", StrictOwner
	if _, ok := p.Acquire(context.Background(), "pub", 9, true); ok {
		t.Fatal("must wait")
	}
	now = now.Add(strictWait)
	l, ok = p.Acquire(context.Background(), "pub", 9, true)
	if !ok || l.Slot != 0 || !l.wipe {
		t.Fatalf("strict job starved: %+v ok=%v", l, ok)
	}
}

// Without a strict cleanup in progress, or when a worthless slot is free, a
// strict job never waits.
func TestAcquireStrictNoNeedlessWait(t *testing.T) {
	for name, slots := range map[string][]ts{
		"no strict cleanup": {{0, "ready", "priv", 1, 100, false}, {1, "cleaning", "org", 5, 0, false}},
		"worthless slot":    {{0, "ready", UnknownOwner, 0, 0, false}, {1, "cleaning", StrictOwner, 0, 0, false}},
		"cleanup failed":    {{0, "ready", "priv", 1, 0, false}, {1, "error", StrictOwner, 0, 0, false}},
	} {
		t.Run(name, func(t *testing.T) {
			p := choosePool("owner", slots)
			if l, ok := p.Acquire(context.Background(), "pub", 9, true); !ok || l.Slot != 0 || !l.wipe {
				t.Fatalf("%+v ok=%v", l, ok)
			}
		})
	}
}

// Wipes are counted by reason and a slot's last use is recorded on release.
func TestWipeCountsAndLastUsed(t *testing.T) {
	ops := &fakeSlotOps{}
	p := choosePool("owner", []ts{
		{0, "ready", "org", 5, 0, false},
		{1, "ready", UnknownOwner, 0, 0, false},
		{2, "ready", "priv", 1, 0, false},
	})
	p.sops = ops
	p.cfg.StateFile = filepath.Join(t.TempDir(), "slots.json")
	now := time.Unix(2_000_000, 0)
	p.now = func() time.Time { return now }
	ctx := context.Background()
	run := func(owner string, strict bool) Lease {
		t.Helper()
		l, ok := p.Acquire(ctx, owner, 3, strict)
		if !ok {
			t.Fatal("no slot")
		}
		if err := p.Prepare(ctx, l); err != nil {
			t.Fatal(err)
		}
		if p.slots[l.Slot].wiping {
			t.Fatal("wiping flag left after the wipe")
		}
		p.Release(ctx, l)
		return l
	}
	run("acme", false) // unknown owner (slot 1)
	now = now.Add(time.Minute)
	if l := run("acme", false); l.Slot != 1 || l.wipe {
		t.Fatalf("same owner again: %+v", l)
	}
	run("pub", true)    // owner change for a strict job: org's slot 0, LRU
	run("acme2", false) // the slot the strict job left clean
	if l := run("acme3", false); l.Slot != 2 || !l.wipe {
		t.Fatalf("least recently used slot not wiped: %+v", l)
	}
	got := p.SlotWipes()
	want := map[string]int64{WipeOwnerChange: 1, WipeStrict: 1, WipeUnknownOwner: 1}
	for r, n := range want {
		if got[r] != n {
			t.Fatalf("wipes %v, want %v", got, want)
		}
	}
	if p.slots[1].LastUsed != now.Unix() {
		t.Fatalf("last use %d, want %d", p.slots[1].LastUsed, now.Unix())
	}
}

// The last use survives a restart with the rest of the slot state; a slot
// without state is the oldest.
func TestLastUsedPersisted(t *testing.T) {
	file := filepath.Join(t.TempDir(), "slots.json")
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	p, err := New(Config{Slots: 3, StateFile: file}, logger)
	if err != nil {
		t.Fatal(err)
	}
	p.sops = &fakeSlotOps{}
	p.now = func() time.Time { return time.Unix(3_000_000, 0) }
	p.slots[0] = &slot{SlotState: SlotState{Index: 0, Status: "busy", Owner: "org", TargetID: 5}}
	p.slots[1] = &slot{SlotState: SlotState{Index: 1, Status: "ready", Owner: "org", TargetID: 5, LastUsed: 2_000_000}}
	p.Release(context.Background(), Lease{Slot: 0, Owner: "org", TargetID: 5})

	p2, err := New(Config{Slots: 3, StateFile: file}, logger)
	if err != nil {
		t.Fatal(err)
	}
	p2.mu.Lock()
	for i := range 3 {
		p2.slots[i] = p2.restoredSlotLocked(i)
		p2.slots[i].Status = "ready"
	}
	p2.mu.Unlock()
	if p2.slots[0].LastUsed != 3_000_000 || p2.slots[1].LastUsed != 2_000_000 || p2.slots[2].LastUsed != 0 || p2.slots[2].Owner != UnknownOwner {
		t.Fatalf("restored %+v %+v %+v", p2.slots[0].SlotState, p2.slots[1].SlotState, p2.slots[2].SlotState)
	}
	// The unknown slot goes first, then org's least recently used.
	for _, want := range []int{2, 1} {
		if l, ok := p2.Acquire(context.Background(), "acme", 1, false); !ok || l.Slot != want || !l.wipe {
			t.Fatalf("got %+v ok=%v, want slot %d", l, ok, want)
		}
	}
}
