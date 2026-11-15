package pool

import "time"

// strictWait bounds how long strict jobs wait for a clean slot (one being
// rebuilt after a strict job) instead of wiping a warm slot. That rebuild is
// the same host-side wipe Prepare would run on the warm slot, and it started
// earlier, so waiting for it never delays the job beyond what wiping would
// cost; the bound only covers a cleanup that takes unusually long or fails.
const strictWait = 20 * time.Second

// Wipe reasons, the label values of pitlane_slot_wipes_total.
const (
	WipeOwnerChange  = "owner_change"
	WipeStrict       = "strict"
	WipeUnknownOwner = "unknown_owner"
)

// WipeReasons lists every wipe reason.
var WipeReasons = []string{WipeOwnerChange, WipeStrict, WipeUnknownOwner}

// choice is the slot Acquire hands out.
type choice struct {
	s    *slot
	wipe bool
	// keeps is how many other warm slots the previous owner keeps (wiped
	// slots only).
	keeps int
}

// needsWipe reports whether s must be rebuilt before a job of owner. The
// shared scope lets owners share caches, but a slot left by a strict job, or
// whose last owner is unknown (it may have been a strict job), is always
// wiped, and so is any non-empty slot for a strict job.
func (p *Pool) needsWipe(s *slot, owner string, strict bool) bool {
	return (p.cfg.CacheScope == "owner" || strict || worthless(s.Owner)) && s.Owner != "" && (s.Owner != owner || strict)
}

// worthless reports whether the cache of a slot last used by owner can serve
// no one: it is wiped before any job whatever happens, so taking it loses
// nothing.
func worthless(owner string) bool { return owner == StrictOwner || owner == UnknownOwner }

// warmFor reports whether s holds a cache its owner keeps using.
func warmFor(s *slot, owner string) bool {
	if s.Owner != owner || s.dirty || s.retire || s.wiping {
		return false
	}
	switch s.Status {
	case "ready", "busy", "cleaning", "starting":
		return true
	}
	return false
}

// othersWarmLocked counts the other warm slots of s's owner. Callers hold
// p.mu.
func (p *Pool) othersWarmLocked(s *slot) int {
	n := 0
	for _, o := range p.slots {
		if o != s && warmFor(o, s.Owner) {
			n++
		}
	}
	return n
}

// strictCleaningLocked reports whether a slot is being rebuilt after a strict
// job: it comes back clean shortly. Callers hold p.mu.
func (p *Pool) strictCleaningLocked() bool {
	for _, s := range p.slots {
		if s.Owner == StrictOwner && s.Status == "cleaning" && !s.dirty && !s.retire {
			return true
		}
	}
	return false
}

// affinity ranks a slot that needs no wipe, lower first: same target, same
// owner, empty, then (shared scope) another owner's.
func affinity(s *slot, owner string, targetID int64, strict bool) int {
	switch {
	case strict:
		return 0 // only an empty slot needs no wipe for a strict job
	case s.Owner == owner && s.TargetID == targetID:
		return 0
	case s.Owner == owner:
		return 1
	case s.Owner == "":
		return 2
	}
	return 3
}

// sacrifice ranks a slot to wipe, lower first: a cache no one can use, then
// the slot of an owner that keeps another warm slot (an owner's last warm
// slot goes last), then the least recently used.
func sacrifice(s *slot, keeps int) (tier int, lastUsed int64) {
	switch {
	case worthless(s.Owner):
		tier = 0
	case keeps > 0:
		tier = 1
	default:
		tier = 2
	}
	return tier, s.LastUsed
}

// chooseLocked picks the free slot for a job, by expected cache loss:
//
//  1. a slot that needs no wipe: same target, then same owner, then empty
//     (for strict jobs: empty only), then (shared scope) another owner's;
//  2. otherwise a slot to wipe: one whose cache no one can use (unknown
//     owner, left by a strict job), then one whose owner keeps at least one
//     other warm slot, then any; least recently used first, then lowest
//     index.
//
// Strict jobs wait (nil, deferred) rather than wipe a warm slot while a slot
// is being rebuilt after a strict job, for at most strictWait. Callers hold
// p.mu.
func (p *Pool) chooseLocked(owner string, targetID int64, strict bool) (c choice, deferred bool) {
	var clean, dirty *slot
	bestAff, bestTier, bestKeeps := 0, 0, 0
	var bestUsed int64
	for _, s := range p.slots {
		if s.Status != "ready" || s.retire || s.dirty {
			continue
		}
		if !p.needsWipe(s, owner, strict) {
			a := affinity(s, owner, targetID, strict)
			if clean == nil || a < bestAff || (a == bestAff && s.Index < clean.Index) {
				clean, bestAff = s, a
			}
			continue
		}
		keeps := p.othersWarmLocked(s)
		tier, used := sacrifice(s, keeps)
		if dirty == nil || tier < bestTier || (tier == bestTier && (used < bestUsed || (used == bestUsed && s.Index < dirty.Index))) {
			dirty, bestTier, bestUsed, bestKeeps = s, tier, used, keeps
		}
	}
	switch {
	case clean != nil:
		return choice{s: clean}, false
	case dirty == nil:
		return choice{}, false
	}
	if strict && !worthless(dirty.Owner) && p.strictCleaningLocked() {
		now := p.clock()
		if p.strictWaitSince.IsZero() {
			p.strictWaitSince = now
		}
		if now.Sub(p.strictWaitSince) < strictWait {
			return choice{}, true
		}
	}
	return choice{s: dirty, wipe: true, keeps: bestKeeps}, false
}

// wipeReason is the metric label of a wipe before a job.
func wipeReason(prevOwner string, strict bool) string {
	switch {
	case strict:
		return WipeStrict
	case prevOwner == UnknownOwner:
		return WipeUnknownOwner
	}
	return WipeOwnerChange
}

// SlotWipes returns how many slots were wiped before a job since start, by
// reason (every reason present).
func (p *Pool) SlotWipes() map[string]int64 {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := make(map[string]int64, len(WipeReasons))
	for _, r := range WipeReasons {
		out[r] = p.wipes[r]
	}
	return out
}
