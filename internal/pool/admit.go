package pool

import (
	"context"
	"sort"
	"time"
)

// Admission: besides a free slot, a job needs memory. Each leased slot
// reserves the memory predicted for its job (Request.Predicted, refined by
// SetPrediction once the job is known); a running job counts for the larger
// of its reservation and what it uses now. A new job is admitted when the
// total plus its own prediction fits the budget (host memory minus
// MemoryReserveMB). Two rules keep this from blocking forever:
//
//   - a job is always admitted when nothing else runs, even if its
//     prediction alone exceeds the budget;
//   - the oldest request refused for memory (the head) keeps its place: a
//     job of another target is admitted only if it leaves room for the head,
//     so small jobs cannot overtake a big one indefinitely.

// Wait reasons: why Admit refused a job (label values of
// pitlane_jobs_waiting).
const (
	WaitSlot       = "slot"        // every usable slot is busy
	WaitMemory     = "memory"      // the job's predicted memory does not fit
	WaitStrictSlot = "strict_slot" // a strict job waits for a reserved or clean slot
)

// WaitReasons lists every wait reason.
var WaitReasons = []string{WaitSlot, WaitMemory, WaitStrictSlot}

// HeavyJobBytes is the predicted memory from which a job counts as heavy:
// it takes the most recently used slot of its target (the warmest cache)
// rather than the least recently used one.
const HeavyJobBytes = 1536 << 20

// memHeadTTL drops the head of the memory queue when its target stops
// asking (its job was cancelled or picked up elsewhere).
const memHeadTTL = 15 * time.Second

// Request is a job asking for a slot.
type Request struct {
	Owner     string
	TargetID  int64
	Strict    bool
	Predicted int64     // bytes of memory the job is expected to use (0: unknown)
	Since     time.Time // when the target started waiting (zero: now)
}

// memWaiter is the oldest request refused for memory.
type memWaiter struct {
	target int64
	bytes  int64
	since  time.Time
	seen   time.Time
}

// MemoryState is the memory admission as the UI and /metrics show it.
type MemoryState struct {
	Enabled   bool  `json:"enabled"`
	Total     int64 `json:"total"`     // host (or LXC container) memory, 0 when unknown
	Reserve   int64 `json:"reserve"`   // kept for the host
	Budget    int64 `json:"budget"`    // Total - Reserve (0 when unknown)
	Predicted int64 `json:"predicted"` // reserved for the leased slots
	Actual    int64 `json:"actual"`    // measured use of the running jobs
	Committed int64 `json:"committed"` // what admission counts: per slot the larger of both
	// Waiting is the target at the head of the memory queue (0: none) and
	// what its job needs.
	WaitingTarget int64 `json:"waitingTarget,omitempty"`
	WaitingBytes  int64 `json:"waitingBytes,omitempty"`
}

// Admit reserves a slot (see Acquire) for a job whose memory is predicted.
// It returns the reason ("" when admitted) when the job must wait.
func (p *Pool) Admit(ctx context.Context, r Request) (Lease, string) {
	_ = ctx
	if r.Strict {
		r.Owner = StrictOwner
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.qemuBusy {
		return Lease{}, WaitSlot
	}
	c, reason := p.chooseLocked(r)
	if c.s == nil {
		if reason == WaitStrictSlot {
			p.log.Debug("strict job waits for a reserved or clean slot rather than wipe a warm one")
		}
		return Lease{}, reason
	}
	if !p.admitLocked(r) {
		return Lease{}, WaitMemory
	}
	if r.Strict {
		p.strictWaitSince = time.Time{}
	}
	best := c.s
	l := Lease{Slot: best.Index, Owner: r.Owner, TargetID: r.TargetID, Strict: r.Strict, wipe: c.wipe, Predicted: r.Predicted}
	best.Status, best.Since = statusBusy, time.Now().Unix()
	if p.reserved == nil {
		p.reserved = map[int]int64{}
	}
	p.reserved[best.Index] = r.Predicted
	delete(p.meters, best.Index) // a meter left by an earlier job
	if c.wipe {
		best.wiping = true
		l.prevOwner, l.prevKeeps, l.prevUsed = best.Owner, c.keeps, best.LastUsed
	} else if best.Owner != r.Owner || best.TargetID != r.TargetID {
		best.Owner, best.TargetID = r.Owner, r.TargetID
		p.saveLocked()
	}
	return l, ""
}

// admitLocked applies the memory rules to r. Callers hold p.mu.
func (p *Pool) admitLocked(r Request) bool {
	if !p.cfg.MemoryAdmission {
		return true
	}
	budget := p.budgetLocked()
	if budget <= 0 {
		return true // host memory unknown: slots are the only limit
	}
	now := p.clock()
	since := r.Since
	if since.IsZero() {
		since = now
	}
	head := p.memHead
	if head != nil && now.Sub(head.seen) > memHeadTTL {
		head, p.memHead = nil, nil
	}
	committed, running := p.committedLocked()
	extra := int64(0)
	if head != nil && head.target != r.TargetID {
		extra = head.bytes
	}
	fits := committed+r.Predicted+extra <= budget
	alone := running == 0 && (head == nil || head.target == r.TargetID)
	if fits || alone {
		if head != nil && head.target == r.TargetID {
			p.memHead = nil
		}
		return true
	}
	switch {
	case head == nil || since.Before(head.since):
		p.memHead = &memWaiter{target: r.TargetID, bytes: r.Predicted, since: since, seen: now}
	case head.target == r.TargetID:
		head.bytes, head.seen = r.Predicted, now
	}
	return false
}

// committedLocked returns the memory admission counts for the leased slots
// (per slot the larger of its reservation and its measured use) and how
// many slots are leased. Callers hold p.mu.
func (p *Pool) committedLocked() (committed int64, leased int) {
	for i, res := range p.reserved {
		committed += max(res, p.meters[i].Current())
		leased++
	}
	return committed, leased
}

// budgetLocked is the memory jobs may use: the host's minus the reserve (0
// when the host's is unknown). Callers hold p.mu.
func (p *Pool) budgetLocked() int64 {
	total := p.hostTotal()
	if total <= 0 {
		return 0
	}
	return total - p.reserveBytes(total)
}

// reserveBytes is the configured reserve, at most half the host's memory.
func (p *Pool) reserveBytes(total int64) int64 {
	return min(p.cfg.MemoryReserveMB<<20, total/2)
}

func (p *Pool) hostTotal() int64 {
	if p.memTotal != nil {
		return p.memTotal()
	}
	return hostMemory()
}

// MemoryBudget returns the memory jobs may use (host memory minus the
// reserve, 0 when unknown), whether admission is on or not.
func (p *Pool) MemoryBudget() int64 {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.budgetLocked()
}

// Memory returns the state of the memory admission.
func (p *Pool) Memory() MemoryState {
	p.mu.Lock()
	defer p.mu.Unlock()
	total := p.hostTotal()
	m := MemoryState{Enabled: p.cfg.MemoryAdmission, Total: total}
	if total > 0 {
		m.Reserve = p.reserveBytes(total)
		m.Budget = total - m.Reserve
	}
	for i, res := range p.reserved {
		m.Predicted += res
		m.Actual += p.meters[i].Current()
	}
	m.Committed, _ = p.committedLocked()
	if h := p.memHead; h != nil && p.clock().Sub(h.seen) <= memHeadTTL {
		m.WaitingTarget, m.WaitingBytes = h.target, h.bytes
	}
	return m
}

// SetPrediction replaces the memory reserved for the job of a leased slot
// (once the job, hence its history, is known). It does nothing for a free
// slot.
func (p *Pool) SetPrediction(slot int, bytes int64) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if _, ok := p.reserved[slot]; ok {
		p.reserved[slot] = bytes
		return
	}
	// A runner adopted after a restart: its slot is busy but was never
	// leased by this process.
	if s, ok := p.slots[slot]; ok && s.Status == statusBusy {
		if p.reserved == nil {
			p.reserved = map[int]int64{}
		}
		p.reserved[slot] = bytes
	}
}

// unreserveLocked frees the memory of a slot whose job ended. Callers hold
// p.mu.
func (p *Pool) unreserveLocked(slot int) {
	delete(p.reserved, slot)
	delete(p.meters, slot)
}

func (p *Pool) unreserve(slot int) {
	p.mu.Lock()
	p.unreserveLocked(slot)
	p.mu.Unlock()
}

// SetStrictDemand tells the pool whether a strict target is enabled: with
// AutoStrictSlots, one slot is then reserved for strict jobs.
func (p *Pool) SetStrictDemand(on bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.strictDemand = on
}

// StrictSlots returns how many slots are reserved for strict jobs now.
func (p *Pool) StrictSlots() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.reservedLocked())
}

// strictSlotsLocked is how many slots are reserved for strict jobs: at most
// all but one, so non-strict jobs always have a slot. Callers hold p.mu.
func (p *Pool) strictSlotsLocked(live int) int {
	n := p.cfg.StrictSlots
	if n == AutoStrictSlots {
		n = 0
		if p.strictDemand {
			n = 1
		}
	}
	return max(0, min(n, live-1))
}

// reservedLocked returns the slots reserved for strict jobs: the highest
// indexes, so the set is stable while the slot count does not change.
// Callers hold p.mu.
func (p *Pool) reservedLocked() map[int]bool {
	idx := make([]int, 0, len(p.slots))
	for i, s := range p.slots {
		if !s.retire {
			idx = append(idx, i)
		}
	}
	n := p.strictSlotsLocked(len(idx))
	if n == 0 {
		return nil
	}
	sort.Sort(sort.Reverse(sort.IntSlice(idx)))
	out := make(map[int]bool, n)
	for _, i := range idx[:n] {
		out[i] = true
	}
	return out
}
