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
	// Predicted is the memory reserved for the job (bytes, see Admit).
	Predicted int64
	wipe      bool // the slot last served another owner: Prepare wipes it

	prevOwner string // owner of a slot to wipe, for the log and metrics
	prevKeeps int    // other warm slots that owner keeps
	prevUsed  int64  // when the wiped slot was last released
}

// Acquire reserves a free slot for a job with no memory prediction (see
// Admit). It returns false when the job must wait.
func (p *Pool) Acquire(ctx context.Context, owner string, targetID int64, strict bool) (Lease, bool) {
	l, reason := p.Admit(ctx, Request{Owner: owner, TargetID: targetID, Strict: strict})
	return l, reason == ""
}

// ErrSlotRebuilding means Prepare failed to wipe the slot; it is left dirty
// for the retry loop to rebuild and must not be released.
var ErrSlotRebuilding = errors.New("slot wipe failed, slot is being rebuilt")

// wipeFailed takes a slot whose wipe failed out of service: dirty (its
// previous owner is kept) and in error, so the retry loop rebuilds it.
func (p *Pool) wipeFailed(i int, err error) {
	p.mu.Lock()
	p.unreserveLocked(i)
	if s, ok := p.slots[i]; ok {
		s.dirty, s.wiping = true, false
	}
	p.mu.Unlock()
	p.setStatus(i, statusError, "wipe failed, slot will be rebuilt: "+err.Error())
}

// Prepare readies a reserved slot for a runner: it wipes the slot when it
// last served another owner and refreshes the runner externals if needed.
func (p *Pool) Prepare(ctx context.Context, l Lease) error {
	if l.wipe {
		if err := p.wipeForLease(ctx, l); err != nil {
			return err
		}
	}
	if err := p.ensureExternals(ctx, l.Slot, p.Config().RunnerImage); err != nil {
		return err
	}
	p.prepareStore(ctx, l)
	return nil
}

// wipeForLease rebuilds the leased slot from the host side (a fresh daemon
// start, about 15 s) and hands it to the lease's owner. Nothing inside the
// slot is trusted, and Release may not have emptied the previous job's
// workspace (failed cleanup, Pitlane stopped between the job's end and its
// release). The previous owner's caches are useless to the new one anyway.
// When the wipe fails, the slot is taken out of service and
// ErrSlotRebuilding returned.
func (p *Pool) wipeForLease(ctx context.Context, l Lease) error {
	reason := wipeReason(l.prevOwner, l.Strict)
	p.logWipe(l, reason)
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
	delete(p.externals, l.Slot) // re-copied from the image by Prepare
	p.saveLocked()
	p.mu.Unlock()
	return nil
}

// logWipe logs a wipe before a job and, when a warm cache is lost, why this
// slot was the cheapest one.
func (p *Pool) logWipe(l Lease, reason string) {
	args := []any{"slot", l.Slot, "previous_owner", l.prevOwner, "owner", l.Owner, "strict", l.Strict, "reason", reason}
	if !worthless(l.prevOwner) {
		idle := "never"
		if l.prevUsed > 0 {
			idle = p.clock().Sub(time.Unix(l.prevUsed, 0)).Round(time.Second).String()
		}
		args = append(args, "previous_owner_other_slots", l.prevKeeps, "idle_for", idle)
	}
	p.log.Info("rebuilding slot before its job", args...)
}

// prepareStore readies the slot's pnpm store for the job. A strict job's
// store is never merged. A new or wiped slot gets its store filled from the
// owner's canonical one before the job installs; a store left by an earlier
// job of the same owner is merged after this job instead, off the clock.
func (p *Pool) prepareStore(ctx context.Context, l Lease) {
	if l.Strict {
		p.forgetStore(l.Slot)
		return
	}
	if !p.stores.isSynced(l.Slot, pnpmStoreVolume(p.Config().CacheScope, l.Owner)) && p.ops().slotStoreEmpty(ctx, l.Slot) {
		p.syncPnpmStore(ctx, l.Slot, l.Owner)
	}
}

// strictVolumes returns the throwaway cache volumes of a strict runner.
func strictVolumes(runner string) (cache, tools string) {
	return "pitlane-strict-" + runner + "-cache", "pitlane-strict-" + runner + "-tools"
}

// releaseStrict wipes everything a strict job could have touched: the slot
// is rebuilt from the host side, its throwaway caches removed. On any
// failure it stays dirty and is rebuilt rather than reused.
func (p *Pool) releaseStrict(ctx context.Context, l Lease) {
	p.unreserve(l.Slot)
	p.setStatus(l.Slot, statusCleaning, "")
	err := p.ops().wipe(ctx, l.Slot)
	if verr := p.removeStrictVolumes(ctx, l.Runner); err == nil {
		err = verr
	}
	p.ops().auditBinfmt(ctx)
	if err != nil {
		p.log.Error("strict cleanup failed, the slot will be rebuilt", "slot", l.Slot, "error", err)
		p.wipeFailed(l.Slot, err)
		return
	}
	p.mu.Lock()
	s := p.slots[l.Slot]
	if s != nil {
		s.Owner, s.TargetID, s.LastUsed = "", 0, p.clock().Unix()
	}
	delete(p.externals, l.Slot) // the privileged job could have altered it
	p.saveLocked()
	retire := s != nil && s.retire
	p.mu.Unlock()
	if retire {
		p.removeRetired(ctx, l.Slot)
		return
	}
	// Copy the externals now, before the slot is handed out: the next job
	// would otherwise wait for this copy in Prepare (seconds, up to minutes
	// when several slots copy at once). Nothing ran in the slot since its
	// wipe, so this copy is as trustworthy as one made in Prepare.
	if err := p.ensureExternals(ctx, l.Slot, p.Config().RunnerImage); err != nil {
		p.log.Warn("cannot copy the runner externals after a strict job, the next job copies them", "slot", l.Slot, "error", err)
	}
	p.setStatus(l.Slot, statusReady, "")
}

// removeStrictVolumes removes the throwaway caches of a strict runner ("" =
// unknown runner, nothing to remove) and returns the first error.
func (p *Pool) removeStrictVolumes(ctx context.Context, runner string) error {
	if runner == "" {
		return nil
	}
	var first error
	c, t := strictVolumes(runner)
	for _, v := range []string{c, t} {
		if err := p.ops().removeVolume(ctx, v); err != nil && first == nil {
			first = err
		}
		p.mu.Lock()
		delete(p.chowned, v)
		p.mu.Unlock()
	}
	return first
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
	p.unreserve(l.Slot) // the job is over: its memory is free
	p.setStatus(l.Slot, statusCleaning, "")
	execErr := p.cleanSlot(ctx, l.Slot)
	p.ops().auditBinfmt(ctx)
	if p.cleanFor(l.Slot, l.Owner) {
		// The job's containers are gone: publish the packages it downloaded
		// and fill the slot with the others for the next job.
		p.syncPnpmStore(ctx, l.Slot, l.Owner)
	}
	p.settle(ctx, l.Slot, execErr)
}

// cleanSlot runs releaseScript in slot i. When it fails the slot is marked
// dirty (its previous owner is kept); the returned exec error, nil when the
// script itself failed, only explains the failure.
func (p *Pool) cleanSlot(ctx context.Context, i int) (execErr error) {
	_, code, err := p.ops().exec(ctx, slotName(i), releaseScript(p.Config().KeepBuildGB))
	if err == nil && code == 0 {
		return nil
	}
	p.log.Error("slot cleanup failed, rebuilding the slot before any reuse", "slot", i, "code", code, "error", err)
	p.mu.Lock()
	if s, ok := p.slots[i]; ok {
		s.dirty = true
	}
	p.mu.Unlock()
	return err
}

// cleanFor reports whether slot i is clean and owned by owner.
func (p *Pool) cleanFor(i int, owner string) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	s, ok := p.slots[i]
	return ok && !s.dirty && s.Owner == owner
}

// settle hands a cleaned slot out again, or applies what is pending on it:
// removal when it was retired meanwhile, a rebuild by the retry loop when it
// is dirty (which also covers a pending upgrade), else the upgrade itself.
// execErr explains a failed cleanup in the slot's status.
func (p *Pool) settle(ctx context.Context, i int, execErr error) {
	p.mu.Lock()
	s, ok := p.slots[i]
	retire := ok && s.retire
	upgrade := ok && s.upgrade
	if upgrade {
		s.upgrade = false
	}
	dirty := ok && s.dirty
	p.mu.Unlock()
	switch {
	case retire:
		p.removeRetired(ctx, i)
		return
	case dirty:
		msg := "cleanup failed, slot will be rebuilt"
		if execErr != nil {
			msg += ": " + execErr.Error()
		}
		p.setStatus(i, statusError, msg)
		return
	case upgrade:
		p.log.Info("upgrading slot after its job", "slot", i)
		if err := p.ops().recreate(ctx, i); err != nil {
			p.setStatus(i, statusError, err.Error())
			return
		}
	}
	p.markUsed(i)
	p.setStatus(i, statusReady, "")
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
