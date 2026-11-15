package pool

import (
	"context"
	"errors"
	"fmt"
	"time"
)

// UnknownOwner marks a slot whose previous owner is not known (no or lost
// persisted state): it is wiped before its first job, whatever the cache
// scope, since it may hold a strict job's data. Parentheses never appear in
// GitHub logins.
const UnknownOwner = "(unknown)"

// StrictOwner marks a slot running a strict-isolation job: it is wiped before
// and after, and never trusted by anyone else.
const StrictOwner = "(strict)"

// Lease is a slot reserved for one runner.
type Lease struct {
	Slot     int
	Owner    string
	TargetID int64
	Strict   bool   // strict isolation: fresh caches, slot wiped before and after
	Runner   string // set by the scaler once the runner name is known
	wipe     bool   // the slot last served another owner: Prepare wipes it

	prevOwner string // owner of a slot to wipe, for the log and metrics
	prevKeeps int    // other warm slots that owner keeps
	prevUsed  int64  // when the wiped slot was last released
}

// Acquire reserves a free slot, preferring one that already ran jobs for the
// same target, then the same owner (warmest cache); when a wipe cannot be
// avoided it takes the slot whose cache loss costs least (see chooseLocked).
// It returns false when every slot is busy, or while a strict job waits for
// a slot being cleaned after another strict job.
func (p *Pool) Acquire(ctx context.Context, owner string, targetID int64, strict bool) (Lease, bool) {
	if strict {
		owner = StrictOwner
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.qemuBusy {
		return Lease{}, false
	}
	c, deferred := p.chooseLocked(owner, targetID, strict)
	if c.s == nil {
		if deferred {
			p.log.Debug("strict job waits for a slot being cleaned rather than wipe a warm one")
		}
		return Lease{}, false
	}
	if strict {
		p.strictWaitSince = time.Time{}
	}
	best := c.s
	l := Lease{Slot: best.Index, Owner: owner, TargetID: targetID, Strict: strict, wipe: c.wipe}
	best.Status, best.Since = "busy", time.Now().Unix()
	if c.wipe {
		best.wiping = true
		l.prevOwner, l.prevKeeps, l.prevUsed = best.Owner, c.keeps, best.LastUsed
	} else if best.Owner != owner || best.TargetID != targetID {
		best.Owner, best.TargetID = owner, targetID
		p.saveLocked()
	}
	return l, true
}

// ErrSlotRebuilding means Prepare failed to wipe the slot; it is left dirty
// for the retry loop to rebuild and must not be released.
var ErrSlotRebuilding = errors.New("slot wipe failed, slot is being rebuilt")

// wipeFailed takes a slot whose wipe failed out of service: dirty (its
// previous owner is kept) and in error, so the retry loop rebuilds it.
func (p *Pool) wipeFailed(i int, err error) {
	p.mu.Lock()
	if s, ok := p.slots[i]; ok {
		s.dirty, s.wiping = true, false
	}
	p.mu.Unlock()
	p.setStatus(i, "error", "wipe failed, slot will be rebuilt: "+err.Error())
}

// Prepare readies a reserved slot for a runner: it wipes the slot when it
// last served another owner and refreshes the runner externals if needed.
func (p *Pool) Prepare(ctx context.Context, l Lease) error {
	if l.wipe {
		// The slot is rebuilt from the host side (a fresh daemon start, about
		// 15 s): nothing inside it is trusted, and Release may not have
		// emptied the previous job's workspace (failed cleanup, Pitlane
		// stopped between the job's end and its release). The previous
		// owner's caches are useless to the new one anyway.
		reason := wipeReason(l.prevOwner, l.Strict)
		args := []any{"slot", l.Slot, "previous_owner", l.prevOwner, "owner", l.Owner, "strict", l.Strict, "reason", reason}
		if !worthless(l.prevOwner) {
			// A warm cache is lost: say why this slot was the cheapest.
			idle := "never"
			if l.prevUsed > 0 {
				idle = time.Since(time.Unix(l.prevUsed, 0)).Round(time.Second).String()
			}
			args = append(args, "previous_owner_other_slots", l.prevKeeps, "idle_for", idle)
		}
		p.log.Info("rebuilding slot before its job", args...)
		p.mu.Lock()
		if p.wipes == nil {
			p.wipes = map[string]int64{}
		}
		p.wipes[reason]++
		p.mu.Unlock()
		if err := p.ops().wipe(ctx, l.Slot); err != nil {
			// Never hand out a slot that may still hold another owner's data.
			p.log.Error("slot wipe failed, it will be rebuilt", "slot", l.Slot, "error", err)
			p.wipeFailed(l.Slot, err)
			return ErrSlotRebuilding
		}
		p.mu.Lock()
		if s, ok := p.slots[l.Slot]; ok {
			s.Owner, s.TargetID, s.wiping = l.Owner, l.TargetID, false
		}
		delete(p.externals, l.Slot) // re-copied from the image below
		p.saveLocked()
		p.mu.Unlock()
	}
	if err := p.ensureExternals(ctx, l.Slot, p.Config().RunnerImage); err != nil {
		return err
	}
	if l.Strict {
		p.forgetStore(l.Slot)
	} else if !p.stores.isSynced(l.Slot, pnpmStoreVolume(p.Config().CacheScope, l.Owner)) && p.ops().slotStoreEmpty(ctx, l.Slot) {
		// New or wiped slot: fill its pnpm store from the owner's canonical
		// one before the job installs (a store left by an earlier job of
		// the same owner is merged after this job instead, off the clock).
		p.syncPnpmStore(ctx, l.Slot, l.Owner)
	}
	return nil
}

// strictVolumes returns the throwaway cache volumes of a strict runner.
func strictVolumes(runner string) (cache, tools string) {
	return "pitlane-strict-" + runner + "-cache", "pitlane-strict-" + runner + "-tools"
}

// releaseStrict wipes everything a strict job could have touched: the slot
// is rebuilt from the host side, its throwaway caches removed. On any
// failure it stays dirty and is rebuilt rather than reused.
func (p *Pool) releaseStrict(ctx context.Context, l Lease) {
	p.setStatus(l.Slot, "cleaning", "")
	err := p.ops().wipe(ctx, l.Slot)
	if l.Runner != "" {
		c, t := strictVolumes(l.Runner)
		for _, v := range []string{c, t} {
			if rerr := p.ops().removeVolume(ctx, v); rerr != nil && err == nil {
				err = rerr
			}
			p.mu.Lock()
			delete(p.chowned, v)
			p.mu.Unlock()
		}
	}
	p.ops().auditBinfmt(ctx)
	if err != nil {
		p.log.Error("strict cleanup failed, the slot will be rebuilt", "slot", l.Slot, "error", err)
		p.wipeFailed(l.Slot, err)
		return
	}
	p.mu.Lock()
	if s, ok := p.slots[l.Slot]; ok {
		s.Owner, s.TargetID, s.LastUsed = "", 0, p.clock().Unix()
	}
	delete(p.externals, l.Slot) // the privileged job could have altered it
	p.saveLocked()
	retire := p.slots[l.Slot] != nil && p.slots[l.Slot].retire
	p.mu.Unlock()
	if retire {
		p.removeSlot(ctx, l.Slot)
		p.signal()
		return
	}
	p.setStatus(l.Slot, "ready", "")
}

// clearDir returns a command removing everything in dir (hidden files
// included) except the top-level entries named in keep.
func clearDir(dir string, keep ...string) string {
	cmd := "find " + dir + " -mindepth 1 -maxdepth 1"
	for _, k := range keep {
		cmd += " ! -name " + k
	}
	return cmd + " -exec rm -rf {} +"
}

// releaseScript cleans what a job left in its slot while keeping the owner's
// caches (images, build cache, actions, pnpm store). The daemon prunes are
// best effort; the script fails when the workspace cannot be emptied or a
// container of the job survives, and the slot is then never reused as is.
func releaseScript(keepBuildGB int) string {
	return fmt.Sprintf(`docker ps -aq | xargs -r docker rm -f >/dev/null 2>&1
docker network prune -f >/dev/null 2>&1
docker volume prune -af >/dev/null 2>&1
docker image prune -f >/dev/null 2>&1
docker builder prune -f --keep-storage %dgb >/dev/null 2>&1
%s &&
chown %s %s &&
left=$(docker ps -aq) && test -z "$left"`, keepBuildGB, clearDir(workDir, "_actions", pnpmStoreDir), runnerUID, workDir)
}

// Release cleans what the job left in the slot (containers, networks,
// volumes, workspace) while keeping images and build cache, then frees it.
// Fail closed: when the cleanup fails the slot is marked dirty, so it is never
// handed out again until it has been rebuilt from scratch.
func (p *Pool) Release(ctx context.Context, l Lease) {
	p.revokeCache(l.Runner)
	if l.Strict {
		p.releaseStrict(ctx, l)
		return
	}
	p.setStatus(l.Slot, "cleaning", "")
	cfg := p.Config()
	_, code, err := p.ops().exec(ctx, slotName(l.Slot), releaseScript(cfg.KeepBuildGB))
	failed := err != nil || code != 0
	if failed {
		p.log.Error("slot cleanup failed, rebuilding the slot before any reuse", "slot", l.Slot, "code", code, "error", err)
		p.mu.Lock()
		if s, ok := p.slots[l.Slot]; ok {
			s.dirty = true // previous owner is kept
		}
		p.mu.Unlock()
	}
	p.ops().auditBinfmt(ctx)
	p.mu.Lock()
	s, ok := p.slots[l.Slot]
	owned := ok && !s.dirty && s.Owner == l.Owner
	p.mu.Unlock()
	if owned {
		// The job's containers are gone: publish the packages it downloaded
		// and fill the slot with the others for the next job.
		p.syncPnpmStore(ctx, l.Slot, l.Owner)
	}
	p.mu.Lock()
	s, ok = p.slots[l.Slot]
	retire := ok && s.retire
	upgrade := ok && s.upgrade
	if upgrade {
		s.upgrade = false
	}
	dirty := ok && s.dirty
	p.mu.Unlock()
	if retire {
		p.removeSlot(ctx, l.Slot)
		p.signal()
		return
	}
	if dirty {
		// The retry loop rebuilds dirty slots in error (a rebuild recreates
		// the container, which also covers a pending upgrade).
		msg := "cleanup failed, slot will be rebuilt"
		if err != nil {
			msg += ": " + err.Error()
		}
		p.setStatus(l.Slot, "error", msg)
		return
	}
	if upgrade {
		p.log.Info("upgrading slot after its job", "slot", l.Slot)
		if err := p.ops().recreate(ctx, l.Slot); err != nil {
			p.setStatus(l.Slot, "error", err.Error())
			return
		}
	}
	p.markUsed(l.Slot)
	p.setStatus(l.Slot, "ready", "")
}

// markUsed records (and persists) that a slot just finished a job: the least
// recently used slot is the first sacrificed when a wipe is unavoidable.
func (p *Pool) markUsed(i int) {
	p.mu.Lock()
	if s, ok := p.slots[i]; ok {
		s.LastUsed = p.clock().Unix()
		p.saveLocked()
	}
	p.mu.Unlock()
}
