package pool

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func stateOwner(t *testing.T, p *Pool, slot int) string {
	t.Helper()
	b, err := os.ReadFile(p.cfg.StateFile)
	if err != nil {
		t.Fatal(err)
	}
	var st map[int]persistedSlot
	if err := json.Unmarshal(b, &st); err != nil {
		t.Fatal(err)
	}
	return st[slot].Owner
}

// The whole lease (Acquire -> Prepare -> Release) for every previous owner
// of a slot, cache scope and isolation mode: a slot is rebuilt from the host
// whenever its data may belong to someone else.
func TestLeaseOwnerChangeMatrix(t *testing.T) {
	const owner, target = "acme", int64(1)
	type prev struct {
		owner  string
		target int64
	}
	prevs := map[string]prev{
		"empty":         {"", 0},
		"same target":   {owner, target},
		"same owner":    {owner, 2},
		"other owner":   {"other", 9},
		"unknown owner": {UnknownOwner, 0},
		"strict job":    {StrictOwner, 0},
	}
	for _, tc := range []struct {
		scope  string
		strict bool
		prev   string
		wipe   bool
	}{
		{"owner", false, "empty", false},
		{"owner", false, "same target", false},
		{"owner", false, "same owner", false},
		{"owner", false, "other owner", true},
		{"owner", false, "unknown owner", true},
		{"owner", false, "strict job", true},
		{"shared", false, "empty", false},
		{"shared", false, "same target", false},
		{"shared", false, "other owner", false},
		{"shared", false, "unknown owner", true},
		{"shared", false, "strict job", true},
		{"owner", true, "empty", false},
		{"owner", true, "same target", true},
		{"owner", true, "other owner", true},
		{"owner", true, "unknown owner", true},
		{"owner", true, "strict job", true},
		{"shared", true, "empty", false},
		{"shared", true, "other owner", true},
		{"shared", true, "unknown owner", true},
		{"shared", true, "strict job", true},
	} {
		name := tc.scope + "/" + tc.prev
		if tc.strict {
			name += "/strict"
		}
		t.Run(name, func(t *testing.T) {
			pr := prevs[tc.prev]
			ops := &fakeSlotOps{}
			p := newTestPool(tc.scope, SlotState{Index: 0, Status: "ready", Owner: pr.owner, TargetID: pr.target})
			p.sops = ops
			p.cfg.StateFile = filepath.Join(t.TempDir(), "slots.json")
			p.externals[0] = "sha256:runner" // copied for the previous owner

			l, ok := p.Acquire(context.Background(), owner, target, tc.strict)
			if !ok {
				t.Fatal("free slot not handed out")
			}
			if l.wipe != tc.wipe {
				t.Fatalf("lease wipe=%v, want %v", l.wipe, tc.wipe)
			}
			want := owner
			if tc.strict {
				want = StrictOwner
			}
			if l.Owner != want || l.Strict != tc.strict {
				t.Fatalf("lease %+v", l)
			}
			if err := p.Prepare(context.Background(), l); err != nil {
				t.Fatal(err)
			}
			if got := ops.wipes; got != map[bool]int{true: 1, false: 0}[tc.wipe] {
				t.Fatalf("%d wipes before the job, want wipe=%v", got, tc.wipe)
			}
			if len(ops.scripts) != 0 {
				t.Fatal("an owner change must never trust a script run inside the slot")
			}
			// A wiped slot gets the runner externals again; otherwise the
			// previous copy (same image) is reused.
			if wantCopies := map[bool]int{true: 1, false: 0}[tc.wipe]; ops.copies != wantCopies {
				t.Fatalf("%d externals copies, want %d", ops.copies, wantCopies)
			}
			if s := p.slots[0]; s.Status != "busy" || (tc.wipe && s.Owner != want) {
				t.Fatalf("prepared slot %+v", s.SlotState)
			}
			if tc.wipe && stateOwner(t, p, 0) != want {
				t.Fatalf("persisted owner %q, want %q", stateOwner(t, p, 0), want)
			}
			// pnpm: strict jobs never see a shared store.
			if tc.strict && len(ops.merges) != 0 {
				t.Fatalf("strict job merged a pnpm store: %v", ops.merges)
			}

			l.Runner = "pitlane-x"
			p.Release(context.Background(), l)
			s := p.slots[0]
			if s.dirty || s.Status != "ready" {
				t.Fatalf("released slot %+v dirty=%v", s.SlotState, s.dirty)
			}
			if tc.strict {
				// Rebuilt from the host after the job, throwaway caches gone.
				if ops.wipes != map[bool]int{true: 2, false: 1}[tc.wipe] || len(ops.scripts) != 0 || s.Owner != "" || stateOwner(t, p, 0) != "" {
					t.Fatalf("strict release: wipes=%d scripts=%d owner=%q", ops.wipes, len(ops.scripts), s.Owner)
				}
				if c, tl := strictVolumes("pitlane-x"); len(ops.removed) != 2 || ops.removed[0] != c || ops.removed[1] != tl {
					t.Fatalf("throwaway caches removed: %v", ops.removed)
				}
				if _, ok := p.externals[0]; ok {
					t.Fatal("externals must be copied again after a strict job")
				}
				return
			}
			if len(ops.scripts) != 1 || s.Owner != want {
				t.Fatalf("release: scripts=%d owner=%q", len(ops.scripts), s.Owner)
			}
			// The owner's pnpm store is merged after the job.
			if len(ops.merges) == 0 || ops.merges[len(ops.merges)-1] != pnpmStoreVolume(tc.scope, owner) {
				t.Fatalf("pnpm store not merged after the job: %v", ops.merges)
			}
		})
	}
}

// A failed wipe or cleanup leaves the slot dirty: it is never handed out
// again, to anyone, in any scope, until it has been rebuilt.
func TestDirtySlotNeverHandedOut(t *testing.T) {
	for _, scope := range []string{"owner", "shared"} {
		for _, step := range []string{"prepare", "release", "strict release", "externals"} {
			t.Run(scope+"/"+step, func(t *testing.T) {
				ops := &fakeSlotOps{}
				p := newTestPool(scope, SlotState{Index: 0, Status: "ready", Owner: UnknownOwner})
				p.sops = ops
				strict := step == "strict release"
				l, ok := p.Acquire(context.Background(), "acme", 1, strict)
				if !ok {
					t.Fatal("not handed out")
				}
				switch step {
				case "prepare":
					ops.wipeErr = errors.New("volume in use")
					if err := p.Prepare(context.Background(), l); !errors.Is(err, ErrSlotRebuilding) {
						t.Fatalf("got %v", err)
					}
				case "externals":
					// The wipe worked but the externals copy failed: the slot
					// is clean (new owner), released as usual.
					ops.copyErr = errors.New("no space")
					if err := p.Prepare(context.Background(), l); err == nil || errors.Is(err, ErrSlotRebuilding) {
						t.Fatalf("got %v", err)
					}
					ops.code = 1 // and its cleanup fails
					p.Release(context.Background(), l)
				case "release":
					if err := p.Prepare(context.Background(), l); err != nil {
						t.Fatal(err)
					}
					ops.code = 1
					p.Release(context.Background(), l)
				case "strict release":
					if err := p.Prepare(context.Background(), l); err != nil {
						t.Fatal(err)
					}
					ops.wipeErr = errors.New("daemon gone")
					p.Release(context.Background(), l)
				}
				s := p.slots[0]
				if !s.dirty || s.Status != "error" {
					t.Fatalf("slot %+v dirty=%v, want dirty and in error", s.SlotState, s.dirty)
				}
				for _, o := range []struct {
					owner  string
					strict bool
				}{{"acme", false}, {"other", false}, {s.Owner, false}, {"acme", true}} {
					if _, ok := p.Acquire(context.Background(), o.owner, 1, o.strict); ok {
						t.Fatalf("dirty slot handed out to %q (strict %v)", o.owner, o.strict)
					}
				}
				// Even ready again (a status reset by mistake), dirty wins.
				s.Status = "ready"
				if _, ok := p.Acquire(context.Background(), "acme", 1, false); ok {
					t.Fatal("dirty slot handed out once marked ready")
				}
			})
		}
	}
}

// A slot whose container must be upgraded is recreated after its job, from
// the host; its owner (and caches) are kept.
func TestReleaseUpgradesSlot(t *testing.T) {
	ops := &fakeSlotOps{}
	p := newTestPool("owner", SlotState{Index: 0, Status: "busy", Owner: "acme", TargetID: 1})
	p.sops = ops
	p.slots[0].upgrade = true
	p.Release(context.Background(), Lease{Slot: 0, Owner: "acme", TargetID: 1})
	if s := p.slots[0]; ops.recreates != 1 || s.upgrade || s.Status != "ready" || s.Owner != "acme" {
		t.Fatalf("recreates=%d slot %+v upgrade=%v", ops.recreates, s.SlotState, s.upgrade)
	}
}

// A new runner image is copied into the externals volume once.
func TestEnsureExternalsTracksImage(t *testing.T) {
	ops := &fakeSlotOps{image: "sha256:v1"}
	p := newTestPool("owner", SlotState{Index: 0, Status: "busy", Owner: "acme"})
	p.sops = ops
	for range 2 {
		if err := p.ensureExternals(context.Background(), 0, "runner:1"); err != nil {
			t.Fatal(err)
		}
	}
	ops.image = "sha256:v2"
	if err := p.ensureExternals(context.Background(), 0, "runner:2"); err != nil {
		t.Fatal(err)
	}
	if ops.copies != 2 || p.externals[0] != "sha256:v2" {
		t.Fatalf("copies=%d externals=%q", ops.copies, p.externals[0])
	}
	ops.image, ops.copyErr = "sha256:v3", errors.New("disk full")
	if err := p.ensureExternals(context.Background(), 0, "runner:3"); err == nil || p.externals[0] != "sha256:v2" {
		t.Fatalf("failed copy recorded: %v %q", err, p.externals[0])
	}
}
