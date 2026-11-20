package pool

import (
	"context"
	"testing"
	"time"
)

const gB = int64(1) << 30 // bytes

// admitPool is a pool of n empty ready slots with memory admission on a
// host of total bytes (reserve 0 unless set).
func admitPool(n int, total int64) *Pool {
	p := newTestPool("owner")
	for i := range n {
		p.slots[i] = &slot{SlotState: SlotState{Index: i, Status: "ready"}}
	}
	p.cfg.MemoryAdmission = true
	p.cfg.MemoryReserveMB = 0
	p.memTotal = func() int64 { return total }
	now := time.Unix(1_000_000, 0)
	p.now = func() time.Time { return now }
	return p
}

func TestAdmitMemory(t *testing.T) {
	type req struct {
		target int64
		pred   int64
		reason string // "" admitted
	}
	for _, tc := range []struct {
		name    string
		slots   int
		total   int64
		reserve int64 // MB
		off     bool
		reqs    []req
	}{
		{name: "fits", slots: 4, total: 10 * gB, reqs: []req{
			{1, 3 * gB, ""}, {1, 3 * gB, ""}, {2, 3 * gB, ""}, {2, 3 * gB, WaitMemory}}},
		{name: "reserve counts", slots: 4, total: 10 * gB, reserve: 2048, reqs: []req{
			{1, 3 * gB, ""}, {1, 3 * gB, ""}, {2, 3 * gB, WaitMemory}}},
		{name: "a job above the budget runs alone", slots: 4, total: 10 * gB, reqs: []req{
			{1, 12 * gB, ""}, {2, 1 * gB, WaitMemory}}},
		{name: "a job above the budget waits for the others", slots: 4, total: 10 * gB, reqs: []req{
			{1, 1 * gB, ""}, {2, 12 * gB, WaitMemory}}},
		{name: "slots still limit", slots: 2, total: 100 * gB, reqs: []req{
			{1, gB, ""}, {1, gB, ""}, {1, gB, WaitSlot}}},
		{name: "unknown prediction always fits", slots: 3, total: 10 * gB, reqs: []req{
			{1, 9 * gB, ""}, {1, 0, ""}}},
		{name: "admission off", slots: 3, total: 10 * gB, off: true, reqs: []req{
			{1, 8 * gB, ""}, {1, 8 * gB, ""}}},
		{name: "host memory unknown", slots: 3, total: 0, reqs: []req{
			{1, 8 * gB, ""}, {1, 8 * gB, ""}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := admitPool(tc.slots, tc.total)
			p.cfg.MemoryReserveMB = tc.reserve
			p.cfg.MemoryAdmission = !tc.off
			for i, r := range tc.reqs {
				l, reason := p.Admit(context.Background(), Request{Owner: "o", TargetID: r.target, Predicted: r.pred})
				if reason != r.reason {
					t.Fatalf("request %d: reason %q, want %q (lease %+v, memory %+v)", i, reason, r.reason, l, p.Memory())
				}
				if reason == "" && l.Predicted != r.pred {
					t.Fatalf("request %d: lease predicts %d", i, l.Predicted)
				}
			}
		})
	}
}

// A running job counts for what it uses when that exceeds its prediction,
// and a release frees its memory.
func TestAdmitCountsActualUseAndRelease(t *testing.T) {
	p := admitPool(3, 10*gB)
	l, reason := p.Admit(context.Background(), Request{Owner: "o", TargetID: 1, Predicted: 2 * gB})
	if reason != "" {
		t.Fatal(reason)
	}
	// The job uses 7 GiB, far above its prediction.
	m := &Meter{runner: &cgTracker{seen: true, last: cgSample{memCurrent: 7 * gB, anon: -1}}}
	p.meters = map[int]*Meter{l.Slot: m}
	if _, reason := p.Admit(context.Background(), Request{Owner: "o", TargetID: 1, Predicted: 4 * gB}); reason != WaitMemory {
		t.Fatalf("reason %q, want memory (memory %+v)", reason, p.Memory())
	}
	if mem := p.Memory(); mem.Predicted != 2*gB || mem.Actual != 7*gB || mem.Committed != 7*gB || mem.Budget != 10*gB {
		t.Fatalf("memory %+v", mem)
	}
	// The job ends: its slot's memory is free again.
	p.unreserve(l.Slot)
	p.slots[l.Slot].Status = "ready"
	if _, reason := p.Admit(context.Background(), Request{Owner: "o", TargetID: 1, Predicted: 4 * gB}); reason != "" {
		t.Fatalf("after release: %q", reason)
	}
}

// SetPrediction refines the reservation once the job is known, and covers a
// runner adopted after a restart (busy slot, never leased).
func TestSetPrediction(t *testing.T) {
	p := admitPool(3, 10*gB)
	l, _ := p.Admit(context.Background(), Request{Owner: "o", TargetID: 1, Predicted: 2 * gB})
	p.SetPrediction(l.Slot, 9*gB)
	if _, reason := p.Admit(context.Background(), Request{Owner: "o", TargetID: 1, Predicted: 2 * gB}); reason != WaitMemory {
		t.Fatalf("reason %q, want memory", reason)
	}
	p.slots[2].Status = "busy"
	p.SetPrediction(2, gB)
	p.SetPrediction(1, gB) // free slot: ignored
	if mem := p.Memory(); mem.Predicted != 10*gB {
		t.Fatalf("memory %+v", mem)
	}
}

// The oldest request refused for memory keeps its place: a smaller job of
// another target does not overtake it, and it runs alone once everything
// else is done when it does not fit at all.
func TestAdmitNoStarvation(t *testing.T) {
	p := admitPool(8, 10*gB)
	ctx := context.Background()
	t0 := p.clock()
	running := []Lease{}
	for range 3 {
		l, reason := p.Admit(ctx, Request{Owner: "o", TargetID: 1, Predicted: 3 * gB, Since: t0})
		if reason != "" {
			t.Fatal(reason)
		}
		running = append(running, l)
	}
	// The big job (target 2) waits; small jobs of target 3, queued later,
	// would fit but would delay it forever.
	if _, reason := p.Admit(ctx, Request{Owner: "o", TargetID: 2, Predicted: 12 * gB, Since: t0.Add(time.Second)}); reason != WaitMemory {
		t.Fatalf("big job: %q", reason)
	}
	if _, reason := p.Admit(ctx, Request{Owner: "o", TargetID: 3, Predicted: gB / 2, Since: t0.Add(2 * time.Second)}); reason != WaitMemory {
		t.Fatalf("small job overtook the big one: %q", reason)
	}
	if mem := p.Memory(); mem.WaitingTarget != 2 || mem.WaitingBytes != 12*gB {
		t.Fatalf("head: %+v", mem)
	}
	for _, l := range running {
		p.unreserve(l.Slot)
		p.slots[l.Slot].Status = "ready"
	}
	// Nothing runs: the small job still yields to the head, which runs alone.
	if _, reason := p.Admit(ctx, Request{Owner: "o", TargetID: 3, Predicted: gB / 2, Since: t0.Add(2 * time.Second)}); reason != WaitMemory {
		t.Fatalf("small job before the head: %q", reason)
	}
	if _, reason := p.Admit(ctx, Request{Owner: "o", TargetID: 2, Predicted: 12 * gB, Since: t0.Add(time.Second)}); reason != "" {
		t.Fatalf("head: %q", reason)
	}
	if p.Memory().WaitingTarget != 0 {
		t.Fatal("the head must be cleared once admitted")
	}
}

// A head whose target stopped asking (job cancelled) no longer blocks the
// others.
func TestAdmitHeadExpires(t *testing.T) {
	p := admitPool(8, 10*gB)
	ctx := context.Background()
	now := p.clock()
	p.now = func() time.Time { return now }
	if _, r := p.Admit(ctx, Request{Owner: "o", TargetID: 1, Predicted: 8 * gB}); r != "" {
		t.Fatal(r)
	}
	if _, r := p.Admit(ctx, Request{Owner: "o", TargetID: 2, Predicted: 5 * gB}); r != WaitMemory {
		t.Fatal(r)
	}
	if _, r := p.Admit(ctx, Request{Owner: "o", TargetID: 3, Predicted: gB}); r != WaitMemory {
		t.Fatalf("within the TTL: %q", r)
	}
	now = now.Add(memHeadTTL + time.Second)
	if _, r := p.Admit(ctx, Request{Owner: "o", TargetID: 3, Predicted: gB}); r != "" {
		t.Fatalf("after the TTL: %q", r)
	}
}

// Of two waiting targets, the one waiting longest becomes the head, whatever
// the order they ask in.
func TestAdmitFairnessBetweenTargets(t *testing.T) {
	p := admitPool(8, 10*gB)
	ctx := context.Background()
	t0 := p.clock()
	if _, r := p.Admit(ctx, Request{Owner: "o", TargetID: 9, Predicted: 9 * gB}); r != "" {
		t.Fatal(r)
	}
	p.Admit(ctx, Request{Owner: "o", TargetID: 2, Predicted: 2 * gB, Since: t0.Add(5 * time.Second)})
	p.Admit(ctx, Request{Owner: "o", TargetID: 1, Predicted: 2 * gB, Since: t0})
	if got := p.Memory().WaitingTarget; got != 1 {
		t.Fatalf("head is target %d, want 1 (waiting longest)", got)
	}
}

func TestStrictSlots(t *testing.T) {
	const r, b = "ready", "busy"
	for _, tc := range []struct {
		name       string
		strictN    int
		demand     bool
		slots      []ts
		strict     bool
		want       int // -1: refused
		wantWipe   bool
		wantReason string
	}{
		{name: "auto without strict target: no reservation", strictN: AutoStrictSlots,
			slots: []ts{{0, b, "acme", 1, 0, false}, {1, r, "", 0, 0, false}}, want: 1},
		{name: "auto with a strict target: highest slot reserved", strictN: AutoStrictSlots, demand: true,
			slots: []ts{{0, b, "acme", 1, 0, false}, {1, r, "", 0, 0, false}}, want: -1, wantReason: WaitSlot},
		{name: "strict takes its reserved slot", strictN: 1, strict: true,
			slots: []ts{{0, r, "", 0, 0, false}, {1, r, "", 0, 0, false}}, want: 1},
		{name: "strict wipes a warm cache left in its reserved slot", strictN: 1, strict: true,
			slots: []ts{{0, r, "acme", 1, 0, false}, {1, r, "acme", 1, 0, false}}, want: 1, wantWipe: true},
		{name: "strict overflows to an empty slot", strictN: 1, strict: true,
			slots: []ts{{0, r, "acme", 1, 0, false}, {1, r, "", 0, 0, false}, {2, b, StrictOwner, 0, 0, false}}, want: 1},
		{name: "strict overflows to a slot left by a strict job", strictN: 1, strict: true,
			slots: []ts{{0, r, "acme", 1, 0, false}, {1, r, StrictOwner, 0, 0, false}, {2, b, StrictOwner, 0, 0, false}}, want: 1, wantWipe: true},
		{name: "strict never wipes an owner's last warm slot outside the reserved ones", strictN: 1, strict: true,
			slots: []ts{{0, r, "acme", 1, 0, false}, {1, r, "org", 2, 0, false}, {2, b, StrictOwner, 0, 0, false}}, want: -1, wantReason: WaitStrictSlot},
		{name: "strict takes a spare warm slot while its reserved one is busy", strictN: 1, strict: true,
			slots: []ts{{0, r, "org", 2, 0, false}, {1, r, "acme", 1, 50, false}, {2, r, "acme", 1, 10, false}, {3, b, StrictOwner, 0, 0, false}}, want: 2, wantWipe: true},
		{name: "strict waits for its reserved slot being cleaned rather than wipe", strictN: 1, strict: true,
			slots: []ts{{0, r, "acme", 1, 0, false}, {1, r, "acme", 1, 0, false}, {2, "cleaning", StrictOwner, 0, 0, false}}, want: -1, wantReason: WaitStrictSlot},
		{name: "strict wipes its reserved slot before a spare warm one", strictN: 1, strict: true,
			slots: []ts{{0, r, "acme", 1, 0, false}, {1, r, "acme", 1, 0, false}, {2, r, "org", 2, 0, false}}, want: 2, wantWipe: true},
		{name: "reservation of zero keeps v1.0.1 behaviour", strictN: 0, strict: true,
			slots: []ts{{0, r, "acme", 1, 0, false}, {1, r, "acme", 1, 0, false}}, want: 0, wantWipe: true},
		{name: "never every slot", strictN: 5,
			slots: []ts{{0, r, "acme", 1, 0, false}, {1, r, "", 0, 0, false}}, want: 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := choosePool("owner", tc.slots)
			p.cfg.StrictSlots = tc.strictN
			p.SetStrictDemand(tc.demand)
			l, reason := p.Admit(context.Background(), Request{Owner: "acme", TargetID: 1, Strict: tc.strict})
			if tc.want < 0 {
				if reason != tc.wantReason {
					t.Fatalf("got lease %+v reason %q, want reason %q", l, reason, tc.wantReason)
				}
				return
			}
			if reason != "" || l.Slot != tc.want || l.wipe != tc.wantWipe {
				t.Fatalf("got slot %d wipe=%v reason %q, want slot %d wipe=%v", l.Slot, l.wipe, reason, tc.want, tc.wantWipe)
			}
		})
	}
}

// The reserved slots are flagged in the slot list.
func TestStrictSlotsShown(t *testing.T) {
	p := choosePool("owner", []ts{{0, "ready", "", 0, 0, false}, {1, "ready", "", 0, 0, false}, {2, "ready", "", 0, 0, false}})
	p.cfg.StrictSlots = 2
	got := []bool{}
	for _, s := range p.Slots() {
		got = append(got, s.StrictOnly)
	}
	if len(got) != 3 || got[0] || !got[1] || !got[2] {
		t.Fatalf("strict-only flags %v", got)
	}
}

// Among the slots of its own target, a heavy job takes the warmest (most
// recently used), a light one the coldest, a job without prediction the
// lowest index.
func TestPlacementByPrediction(t *testing.T) {
	slots := []ts{{0, "ready", "acme", 1, 300, false}, {1, "ready", "acme", 1, 900, false}, {2, "ready", "acme", 1, 100, false}, {3, "ready", "", 0, 0, false}}
	for _, tc := range []struct {
		pred int64
		want int
	}{{3 * gB, 1}, {gB / 2, 2}, {0, 0}} {
		p := choosePool("owner", slots)
		l, reason := p.Admit(context.Background(), Request{Owner: "acme", TargetID: 1, Predicted: tc.pred})
		if reason != "" || l.Slot != tc.want {
			t.Errorf("predicted %d: slot %d (%q), want %d", tc.pred, l.Slot, reason, tc.want)
		}
	}
}

func TestMemoryLimit(t *testing.T) {
	meminfo := []byte("MemTotal:       24117248 kB\nMemFree:        1000 kB\n")
	for _, tc := range []struct {
		name     string
		meminfo  []byte
		max      string
		inDocker bool
		want     int64
	}{
		{"meminfo only", meminfo, "max\n", false, 24117248 << 10},
		{"lower cgroup limit", meminfo, "8589934592\n", false, 8 << 30},
		{"higher cgroup limit ignored", meminfo, "99999999999999\n", false, 24117248 << 10},
		{"limit of Pitlane's own Docker container ignored", meminfo, "1073741824\n", true, 24117248 << 10},
		{"cgroup only", nil, "4294967296", false, 4 << 30},
		{"unknown", nil, "", false, 0},
	} {
		if got := memoryLimit(tc.meminfo, []byte(tc.max), tc.inDocker); got != tc.want {
			t.Errorf("%s: got %d, want %d", tc.name, got, tc.want)
		}
	}
}

func TestStrictSlotsCount(t *testing.T) {
	p := newTestPool("owner", SlotState{Index: 0, Status: statusReady}, SlotState{Index: 1, Status: statusReady})
	p.cfg.StrictSlots = AutoStrictSlots
	if p.StrictSlots() != 0 {
		t.Fatal("no strict target: no reserved slot")
	}
	p.SetStrictDemand(true)
	if p.StrictSlots() != 1 {
		t.Fatal("a strict target reserves one slot")
	}
	p.cfg.StrictSlots = 5
	if p.StrictSlots() != 1 {
		t.Fatal("non-strict jobs always keep a slot")
	}
}
