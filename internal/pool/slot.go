package pool

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	cerrdefs "github.com/containerd/errdefs"
	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/filters"
	"github.com/docker/docker/api/types/image"
)

// localRunnerImage returns the most recent Pitlane runner image present on
// the host (any tag of runnerRepo), or "" when there is none.
func (p *Pool) localRunnerImage(ctx context.Context) (string, error) {
	list, err := p.cli.ImageList(ctx, image.ListOptions{Filters: filters.NewArgs(filters.Arg("reference", runnerRepo))})
	if err != nil {
		return "", err
	}
	best, created := "", int64(-1)
	for _, im := range list {
		for _, tag := range im.RepoTags {
			if strings.HasPrefix(tag, runnerRepo+":") && im.Created > created {
				best, created = tag, im.Created
			}
		}
	}
	return best, nil
}

// retryInterval is how often slots in error are started (or rebuilt) again.
const retryInterval = 30 * time.Second

// Start pulls images, starts the mirror and every slot. Slots that already
// exist (e.g. after a Pitlane restart) are reused with their warm cache.
func (p *Pool) Start(ctx context.Context) error {
	cfg := p.Config()
	if err := p.EnsureImage(ctx, cfg.DindImage, false); err != nil {
		return err
	}
	img, err := p.ensureRunnerImage(ctx, cfg.RunnerImage)
	if err != nil {
		return err
	}
	p.mu.Lock()
	if img != cfg.RunnerImage { // a fallback
		p.cfg.RunnerImage = img
	}
	p.qemuWant = !cfg.NoQEMU
	p.mu.Unlock()
	cfg.RunnerImage = img
	p.restoreBinfmt(ctx)
	if err := p.ensureMirror(ctx, cfg); err != nil {
		p.log.Warn("registry mirror unavailable, continuing without it", "error", err)
	}
	go p.retryFailed(ctx)
	return p.Configure(ctx, cfg)
}

// ensureRunnerImage pulls the configured runner image and returns the image
// runners will use. When Pitlane's default image cannot be pulled, any
// Pitlane runner image already on this host (another release, or one built
// locally) is preferred: it has the tools the CIs use. GitHub's bare image is
// the last resort, and a loud one: jobs that expect Node, compilers… will
// fail on it. A runner image chosen in the settings has no fallback.
func (p *Pool) ensureRunnerImage(ctx context.Context, want string) (string, error) {
	err := p.EnsureImage(ctx, want, false)
	if err == nil {
		return want, nil
	}
	if want != DefaultRunnerImage {
		return "", err
	}
	if local, lerr := p.localRunnerImage(ctx); lerr == nil && local != "" {
		p.log.Warn("Pitlane runner image unavailable, using the Pitlane runner image already on this host",
			"wanted", want, "using", local, "error", err)
		return local, nil
	}
	p.log.Error("no Pitlane runner image available: using GitHub's bare official image; jobs that need Node, compilers or other tools will fail. Pull or build a Pitlane runner image, or set one in Settings",
		"wanted", want, "error", err)
	if err := p.EnsureImage(ctx, OfficialRunnerImage, false); err != nil {
		return "", err
	}
	return OfficialRunnerImage, nil
}

// retryFailed restarts slots in error every retryInterval, after the
// periodic binfmt_misc audit and a pending QEMU registration.
func (p *Pool) retryFailed(ctx context.Context) {
	t := time.NewTicker(retryInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		p.auditBinfmt(ctx)
		p.maybeRegisterQEMU(ctx)
		failed, dirty := p.takeFailed()
		for _, i := range failed {
			p.log.Info("retrying slot", "slot", i)
			go p.startSlot(ctx, i)
		}
		for _, i := range dirty {
			p.log.Info("retrying slot rebuild", "slot", i)
			go p.rebuildSlot(ctx, i)
		}
	}
}

// takeFailed marks every slot in error (and not retired) as starting and
// returns them: those to start again, and the dirty ones to rebuild.
func (p *Pool) takeFailed() (failed, dirty []int) {
	p.mu.Lock()
	defer p.mu.Unlock()
	for i, s := range p.slots {
		if s.Status != statusError || s.retire {
			continue
		}
		s.Status = statusStarting
		if s.dirty {
			dirty = append(dirty, i)
		} else {
			failed = append(failed, i)
		}
	}
	return failed, dirty
}

// Configure applies a new configuration: slots are added or retired, other
// settings apply to the next runners.
func (p *Pool) Configure(ctx context.Context, cfg Config) error {
	cfg.defaults()
	p.mu.Lock()
	old := p.cfg
	p.cfg = cfg
	toStart, toRemove := p.resizeLocked(cfg.Slots)
	p.mu.Unlock()

	p.applyChanges(ctx, old, cfg)
	for _, i := range toRemove {
		p.removeSlot(ctx, i)
	}
	var wg sync.WaitGroup
	for _, i := range toStart {
		wg.Go(func() { p.startSlot(ctx, i) })
	}
	wg.Wait()
	return nil
}

// resizeLocked adds the slots missing below n (from their persisted state)
// and retires those at or beyond it. It returns the slots to start, and the
// retired ones to remove now: a busy or cleaning slot is removed when its
// job is released. Callers hold p.mu.
func (p *Pool) resizeLocked(n int) (toStart, toRemove []int) {
	for i := range n {
		if s, ok := p.slots[i]; ok {
			s.retire = false
			continue
		}
		p.slots[i] = p.restoredSlotLocked(i)
		toStart = append(toStart, i)
	}
	for i, s := range p.slots {
		if i < n {
			continue
		}
		s.retire = true
		if s.Status != statusBusy && s.Status != statusCleaning {
			toRemove = append(toRemove, i)
		}
	}
	return toStart, toRemove
}

// applyChanges applies the settings that changed from old to cfg outside
// the slot table: cache limits, runner image (pulled in the background) and
// registry mirror.
func (p *Pool) applyChanges(ctx context.Context, old, cfg Config) {
	if old.ActionsCacheGB != cfg.ActionsCacheGB || old.ActionsCacheOwnerPct != cfg.ActionsCacheOwnerPct {
		p.applyCacheLimits()
	}
	if old.RunnerImage != cfg.RunnerImage {
		pullCtx := context.WithoutCancel(ctx)
		go func() {
			if err := p.EnsureImage(pullCtx, cfg.RunnerImage, false); err != nil {
				p.log.Error("pull runner image", "error", err)
			}
		}()
	}
	if cfg.Mirror && !old.Mirror {
		if err := p.ensureMirror(ctx, cfg); err != nil {
			p.log.Warn("registry mirror", "error", err)
		}
	}
}

// restoredSlotLocked builds a new slot from its persisted state. Callers
// hold p.mu.
func (p *Pool) restoredSlotLocked(i int) *slot {
	st, known := p.restored[i]
	if !known {
		// Its volumes may hold data from an owner we do not know about: fail
		// closed, the first job wipes it (and it counts as least recently
		// used).
		st = persistedSlot{Owner: UnknownOwner}
	}
	return &slot{SlotState: SlotState{Index: i, Status: statusStarting, Since: p.clock().Unix(), Owner: st.Owner, TargetID: st.TargetID, LastUsed: st.LastUsed}}
}

func slotName(i int) string    { return fmt.Sprintf("pitlane-slot-%d", i) }
func slotNetwork(i int) string { return fmt.Sprintf("pitlane-slot-%d", i) }
func slotDocker(i int) string  { return fmt.Sprintf("pitlane-slot-%d-docker", i) }
func slotWork(i int) string    { return fmt.Sprintf("pitlane-slot-%d-work", i) }

// slotVolumes lists the volumes of slot i: its daemon's data, its work
// directory and its runner externals.
func slotVolumes(i int) []string { return []string{slotDocker(i), slotWork(i), slotExternals(i)} }

// slotExternals holds a copy of the runner's /home/runner/externals (the Node
// runtimes used by JavaScript actions). Job containers (`container:`) get it
// mounted at /__e, so it must exist on the slot daemon's side too.
func slotExternals(i int) string { return fmt.Sprintf("pitlane-slot-%d-externals", i) }

func (p *Pool) setStatus(i int, status, errMsg string) {
	p.mu.Lock()
	if s, ok := p.slots[i]; ok {
		s.Status, s.Error, s.Since = status, errMsg, p.clock().Unix()
		if status == statusReady {
			s.Runner = ""
		}
	}
	p.mu.Unlock()
	if status == statusReady {
		p.signal()
	}
}

func (p *Pool) startSlot(ctx context.Context, i int) {
	if err := p.ensureSlot(ctx, i); err != nil {
		p.log.Error("slot failed to start", "slot", i, "error", err)
		p.setStatus(i, statusError, err.Error())
		return
	}
	p.mu.Lock()
	s := p.slots[i]
	busy := s != nil && s.Status == statusBusy
	dirty := s != nil && s.dirty
	p.mu.Unlock()
	if dirty {
		p.setStatus(i, statusError, "slot must be rebuilt before reuse")
		return
	}
	if !busy {
		p.setStatus(i, statusReady, "")
	}
	p.log.Info("slot ready", "slot", i)
}

func (p *Pool) removeSlot(ctx context.Context, i int) {
	p.log.Info("retiring slot", "slot", i)
	p.forgetStore(i)
	_ = p.cli.ContainerRemove(ctx, slotName(i), container.RemoveOptions{Force: true, RemoveVolumes: false})
	p.mu.Lock()
	p.saveLocked() // record its current owner while it is still live: its volumes are kept
	delete(p.slots, i)
	delete(p.externals, i)
	p.saveLocked() // now gone: forgotten if beyond the slot count (fail closed)
	p.mu.Unlock()
}

// removeRetired removes a slot retired while it was in use, now that it is
// free, and wakes up whoever waits for a slot.
func (p *Pool) removeRetired(ctx context.Context, i int) {
	p.removeSlot(ctx, i)
	p.signal()
}

// rebuildSlot deletes a slot with all its volumes and starts it again empty.
func (p *Pool) rebuildSlot(ctx context.Context, i int) {
	if err := p.resetSlot(ctx, i); err != nil {
		p.setStatus(i, statusError, "rebuild: "+err.Error())
		return
	}
	p.startSlot(ctx, i)
}

// resetSlot deletes a slot's container and volumes through the host's Docker
// API, never trusting anything inside the slot: its jobs were root on its
// daemon and could have tampered with its filesystem or left processes. The
// slot stays dirty (with its previous owner) until everything is gone; it
// then has no owner. The slot container is not recreated.
func (p *Pool) resetSlot(ctx context.Context, i int) error {
	p.mu.Lock()
	if s, ok := p.slots[i]; ok {
		s.dirty = true // cleared only once everything is really gone
	}
	p.mu.Unlock()
	p.forgetStore(i)
	err := p.cli.ContainerRemove(ctx, slotName(i), container.RemoveOptions{Force: true})
	if err != nil && !cerrdefs.IsNotFound(err) {
		return err
	}
	if err := p.DropSlotCache(ctx, i); err != nil {
		// Keep the previous owner: the slot will be wiped again before any
		// other owner can get it.
		return err
	}
	p.mu.Lock()
	if s, ok := p.slots[i]; ok {
		s.Owner, s.TargetID, s.dirty = "", 0, false
	}
	for _, v := range slotVolumes(i) {
		delete(p.chowned, v)
	}
	delete(p.externals, i)
	p.saveLocked()
	p.mu.Unlock()
	return nil
}

// DropSlotCache deletes a retired slot's volumes (frees disk).
func (p *Pool) DropSlotCache(ctx context.Context, i int) error {
	for _, v := range slotVolumes(i) {
		// Runner and init containers mount these volumes too (even when
		// stopped): Docker refuses to delete a volume while any container
		// still references it.
		p.removeVolumeUsers(ctx, v)
		if err := p.cli.VolumeRemove(ctx, v, true); err != nil && !cerrdefs.IsNotFound(err) {
			return fmt.Errorf("remove volume %s: %w", v, err)
		}
	}
	return nil
}

// removeVolumeUsers force-removes every container, in any state, that mounts
// the volume.
func (p *Pool) removeVolumeUsers(ctx context.Context, vol string) {
	list, err := p.cli.ContainerList(ctx, container.ListOptions{All: true, Filters: filters.NewArgs(filters.Arg("volume", vol))})
	if err != nil {
		p.log.Warn("list containers using volume", "volume", vol, "error", err)
		return
	}
	for _, c := range list {
		p.Remove(ctx, c.ID)
	}
}

// MarkRunner records which runner occupies a slot (for the UI).
func (p *Pool) MarkRunner(slotIdx int, runner string) {
	p.mu.Lock()
	if s, ok := p.slots[slotIdx]; ok {
		s.Runner = runner
	}
	p.mu.Unlock()
}

// slotIndexSlack is how far beyond the slot count Slots looks for slots
// (retired ones, or adopted runners' slots with a higher index).
const slotIndexSlack = 8

// Slots returns a snapshot of every slot, by index.
func (p *Pool) Slots() []SlotState {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := make([]SlotState, 0, len(p.slots))
	reserved := p.reservedLocked()
	for i := 0; i < len(p.slots)+slotIndexSlack; i++ {
		if s, ok := p.slots[i]; ok {
			st := s.SlotState
			if s.retire {
				st.Status = statusRetiring
			}
			st.StrictOnly = reserved[i]
			if res, ok := p.reserved[i]; ok {
				st.Predicted, st.Actual = res, p.meters[i].Current()
			}
			out = append(out, st)
		}
	}
	return out
}

// FreeSlots counts ready slots.
func (p *Pool) FreeSlots() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	n := 0
	for _, s := range p.slots {
		if s.Status == statusReady && !s.retire {
			n++
		}
	}
	return n
}
