package pool

import (
	"context"
	"path/filepath"
	"sort"
	"testing"
	"time"
)

// A replay of the burst that brought the production host down: 6 jobs of a
// private repository whose slots are warm (two type-check & build at
// 3.1 GiB, two unit tests at 2.1 GiB, two Docker image builds at 0.9 GiB in
// the runner plus 1.5 GiB of anonymous memory in the slot), 7 jobs of
// another owner and 3 strict jobs of a public repository, on a 23 GiB host.
// Pool, admission and slot choice are the real ones; Docker is faked and
// the scaler's ordering (longest waiting target first, heaviest pending job
// first, a target that got a runner goes behind) is reproduced.

type simJob struct {
	name     string
	owner    string
	target   int64
	strict   bool
	arrive   time.Duration
	run      time.Duration
	predict  int64 // from history
	actual   int64 // what it really uses (the docker job uses more than predicted)
	started  time.Duration
	finished time.Duration
}

type simResult struct {
	maxActual   int64 // highest sum of the memory of concurrent jobs
	budget      int64
	warmWipes   int // warm caches lost (slot wiped while its owner could use it)
	strictWarm  int // of which by strict jobs
	wipes       int64
	makespan    time.Duration
	meanWait    time.Duration
	strictWait  time.Duration // longest wait of a strict job
	overBudget  bool
	maxParallel int
}

func burst() []*simJob {
	const g = int64(1) << 30
	mb := func(n int64) int64 { return n << 20 }
	jobs := []*simJob{
		{name: "build", owner: "markapage", target: 1, run: 6 * time.Minute, predict: mb(3200), actual: mb(3200)},
		{name: "build", owner: "markapage", target: 1, run: 6 * time.Minute, predict: mb(3200), actual: mb(3200)},
		{name: "tests", owner: "markapage", target: 1, run: 4 * time.Minute, predict: mb(2150), actual: mb(2100)},
		{name: "tests", owner: "markapage", target: 1, run: 4 * time.Minute, predict: mb(2150), actual: mb(2100)},
		{name: "docker", owner: "markapage", target: 1, run: 8 * time.Minute, predict: mb(2400), actual: mb(2700)},
		{name: "docker", owner: "markapage", target: 1, run: 8 * time.Minute, predict: mb(2400), actual: mb(2700)},
	}
	for i := range 7 {
		jobs = append(jobs, &simJob{name: "other", owner: "other", target: 2, arrive: time.Duration(15+5*i) * time.Second,
			run: 3 * time.Minute, predict: mb(1300), actual: mb(1250)})
	}
	for i := range 3 {
		jobs = append(jobs, &simJob{name: "public", owner: "public", target: 3, strict: true, arrive: time.Duration(10+10*i) * time.Second,
			run: 2 * time.Minute, predict: g, actual: g})
	}
	return jobs
}

func simulate(t *testing.T, slots int, admission bool, strictSlots int) simResult {
	t.Helper()
	const total = int64(23) << 30
	p := newTestPool("owner")
	p.sops = &fakeSlotOps{}
	p.cfg.StateFile = filepath.Join(t.TempDir(), "slots.json")
	p.cfg.MemoryAdmission = admission
	p.cfg.MemoryReserveMB = DefaultMemoryReserveMB
	p.cfg.StrictSlots = strictSlots
	p.SetStrictDemand(true)
	p.memTotal = func() int64 { return total }
	start := time.Unix(10_000_000, 0)
	now := start
	p.now = func() time.Time { return now }
	// Warm slots: the private repository's, then two of the other owner's;
	// the last one is empty, as a strict job leaves it (in both versions).
	for i := range slots {
		owner, target := "markapage", int64(1)
		switch {
		case i == slots-1:
			owner, target = "", 0
		case i >= slots-3:
			owner, target = "other", 2
		}
		p.slots[i] = &slot{SlotState: SlotState{Index: i, Status: "ready", Owner: owner, TargetID: target, LastUsed: start.Unix() - int64(100*(slots-i))}}
	}
	ctx := context.Background()
	jobs := burst()
	var running []simRun
	since := map[int64]time.Time{}
	res := simResult{budget: p.MemoryBudget()}
	done := 0
	for elapsed := time.Duration(0); done < len(jobs); elapsed += 5 * time.Second {
		if elapsed > 3*time.Hour {
			t.Fatal("simulation does not finish: starvation")
		}
		now = start.Add(elapsed)
		// Jobs that ended release their slot.
		kept := running[:0]
		for _, r := range running {
			if elapsed-r.j.started >= r.j.run {
				r.j.finished = elapsed
				p.Release(ctx, r.l)
				done++
				continue
			}
			kept = append(kept, r)
		}
		running = kept
		// Pending jobs per target, heaviest first; targets longest waiting first.
		pending := map[int64][]*simJob{}
		for _, j := range jobs {
			if j.arrive <= elapsed && j.started == 0 && j.finished == 0 && !isRunning(running, j) {
				pending[j.target] = append(pending[j.target], j)
				if _, ok := since[j.target]; !ok {
					since[j.target] = now
				}
			}
		}
		targets := make([]int64, 0, len(pending))
		for tg := range pending {
			targets = append(targets, tg)
		}
		sort.Slice(targets, func(a, b int) bool { return since[targets[a]].Before(since[targets[b]]) })
		for _, tg := range targets {
			q := pending[tg]
			sort.SliceStable(q, func(a, b int) bool { return q[a].predict > q[b].predict })
			n := 0
			for _, j := range q {
				l, reason := p.Admit(ctx, Request{Owner: j.owner, TargetID: j.target, Strict: j.strict, Predicted: j.predict, Since: since[tg]})
				if reason != "" {
					break
				}
				if l.wipe && !worthless(l.prevOwner) {
					res.warmWipes++
					if j.strict {
						res.strictWarm++
					}
				}
				if err := p.Prepare(ctx, l); err != nil {
					t.Fatal(err)
				}
				// The job's measured use, as the meter reports it.
				p.mu.Lock()
				if p.meters == nil {
					p.meters = map[int]*Meter{}
				}
				p.meters[l.Slot] = &Meter{runner: &cgTracker{seen: true, last: cgSample{memCurrent: j.actual, anon: -1}}}
				p.mu.Unlock()
				j.started = max(elapsed, time.Nanosecond)
				running = append(running, simRun{j, l})
				n++
			}
			switch {
			case n == len(q):
				delete(since, tg)
			case n > 0:
				since[tg] = now // got a runner, still waits: behind the others
			}
		}
		var sum int64
		for _, r := range running {
			sum += r.j.actual
		}
		res.maxActual = max(res.maxActual, sum)
		res.maxParallel = max(res.maxParallel, len(running))
		if sum > res.budget && len(running) > 1 {
			res.overBudget = true
		}
	}
	var waits time.Duration
	for _, j := range jobs {
		w := j.started - j.arrive
		waits += w
		res.makespan = max(res.makespan, j.finished)
		if j.strict {
			res.strictWait = max(res.strictWait, w)
		}
	}
	res.meanWait = waits / time.Duration(len(jobs))
	for _, n := range p.SlotWipes() {
		res.wipes += n
	}
	return res
}

type simRun struct {
	j *simJob
	l Lease
}

func isRunning(running []simRun, j *simJob) bool {
	for _, r := range running {
		if r.j == j {
			return true
		}
	}
	return false
}

func logSim(t *testing.T, name string, r simResult) {
	t.Helper()
	t.Logf("%-28s peak %5.1f GiB / budget %4.1f GiB, parallel %2d, warm caches wiped %d (%d by strict jobs, all wipes %d), mean wait %v, strict wait %v, makespan %v",
		name, float64(r.maxActual)/(1<<30), float64(r.budget)/(1<<30), r.maxParallel, r.warmWipes, r.strictWarm, r.wipes, r.meanWait, r.strictWait, r.makespan)
}

func TestSimulateProductionBurst(t *testing.T) {
	old8 := simulate(t, 8, false, 0)
	new8 := simulate(t, 8, true, AutoStrictSlots)
	old12 := simulate(t, 12, false, 0)
	new12 := simulate(t, 12, true, AutoStrictSlots)
	logSim(t, "v1.0.1, 8 slots", old8)
	logSim(t, "v1.1, 8 slots", new8)
	logSim(t, "v1.0.1, 12 slots", old12)
	logSim(t, "v1.1, 12 slots", new12)

	if !old12.overBudget {
		t.Error("the replay must reproduce the over-commit of the 12-slot setup")
	}
	for name, r := range map[string]simResult{"8 slots": new8, "12 slots": new12} {
		if r.overBudget {
			t.Errorf("%s: over-commit, peak %d > budget %d", name, r.maxActual, r.budget)
		}
		if r.strictWarm != 0 {
			t.Errorf("%s: strict jobs wiped %d warm caches, want none", name, r.strictWarm)
		}
	}
	// v1.1 never wipes more warm caches than v1.0.1 (strict jobs wipe none,
	// checked above); the remaining wipes come from jobs of another owner and
	// are unavoidable with the owner cache scope. The replay is not fully
	// deterministic, so only these invariants are asserted.
	for name, p := range map[string][2]simResult{"8 slots": {old8, new8}, "12 slots": {old12, new12}} {
		old, cur := p[0], p[1]
		if cur.warmWipes > old.warmWipes {
			t.Errorf("%s: %d warm caches wiped, want at most v1.0.1's %d", name, cur.warmWipes, old.warmWipes)
		}
	}
}
