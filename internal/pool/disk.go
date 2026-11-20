package pool

import (
	"context"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"syscall"

	cerrdefs "github.com/containerd/errdefs"
	"github.com/docker/docker/api/types"
	"github.com/docker/docker/api/types/container"
)

// Shared per-owner cache volumes (see cacheVolume). Only these are pruned when
// the disk runs low: slot volumes are cleaned through their daemon, strict
// volumes are removed with their job.
var prunableVolumePrefixes = []string{"pitlane-cache-", "pitlane-toolcache-"}

// volInfo describes a Docker volume for disk reclaiming.
type volInfo struct {
	Name  string
	Size  int64 // bytes, -1 when unknown
	InUse bool  // referenced by any container (running or not)
}

// diskOps is what disk reclaiming needs from Docker and the host. Tests use a
// fake implementation.
type diskOps interface {
	freeBytes() (int64, error)
	execSlot(ctx context.Context, slot int, script string) error
	volumes(ctx context.Context) ([]volInfo, error)
	removeVolume(ctx context.Context, name string) error
	// onDisk reports whether dir is on the watched filesystem (evicting
	// files elsewhere would free nothing there).
	onDisk(dir string) bool
}

// DiskPath returns the filesystem whose free space Pitlane watches: Docker's
// data directory when visible, else Pitlane's own data directory.
func (p *Pool) DiskPath() string {
	if root := p.host().dockerRoot; root != "" {
		if _, err := os.Stat(root); err == nil {
			return root
		}
	}
	if f := p.Config().StateFile; f != "" {
		return filepath.Dir(f)
	}
	return "."
}

// DiskSpace returns the total and available bytes of DiskPath.
func (p *Pool) DiskSpace() (total, free int64, err error) {
	return p.host().statfs(p.DiskPath())
}

// EnsureFreeDisk frees disk space when less than the configured minimum is
// available. It is called after each job's cleanup; concurrent calls return
// immediately.
func (p *Pool) EnsureFreeDisk(ctx context.Context) {
	minGB := p.Config().MinFreeDiskGB
	if minGB <= 0 {
		return
	}
	if !p.pruneMu.TryLock() {
		return
	}
	defer p.pruneMu.Unlock()
	p.reclaimDisk(ctx, int64(minGB)<<30, dockerDiskOps{p})
}

// slotPrunes are the slot prunes reclaimDisk runs on every idle slot, in
// order.
var slotPrunes = []struct{ what, script string }{
	{"build cache", "docker builder prune -af >/dev/null"},
	{"unused images", "docker image prune -af >/dev/null"},
}

// reclaimDisk frees space, cheapest loss first, until minFree bytes are
// available: build caches of idle slots, then their images, then actions
// cache entries, then the largest per-owner cache volumes no container uses.
// Busy slots are never touched: an idle slot is marked "cleaning" while it is
// pruned so it cannot be handed out meanwhile. It returns the bytes freed.
func (p *Pool) reclaimDisk(ctx context.Context, minFree int64, ops diskOps) int64 {
	free, err := ops.freeBytes()
	if err != nil || free >= minFree {
		return 0
	}
	p.log.Warn("low disk space, pruning caches", "freeGB", gb(free), "minGB", gb(minFree))
	r := &reclaim{p: p, ops: ops, minFree: minFree, start: free, free: free}
	if r.pruneIdleSlots(ctx) && r.evictActionsCache() {
		r.removeCacheVolumes(ctx)
	}
	return r.done()
}

// reclaim is one run of reclaimDisk. Each step returns whether to go on:
// false once enough space is free, or when free space became unknown.
type reclaim struct {
	p       *Pool
	ops     diskOps
	minFree int64
	start   int64 // free bytes when the run started
	free    int64 // free bytes after the last step
}

// measure refreshes free after what was pruned (free was before then).
func (r *reclaim) measure(what string, before int64) bool {
	f, err := r.ops.freeBytes()
	if err != nil {
		r.p.log.Error("disk space unknown, pruning stopped", "error", err)
		return false
	}
	r.free = f
	r.p.log.Info("pruned", "what", what, "freedMB", (f-before)>>20, "freeGB", gb(f))
	return f < r.minFree
}

// done ends the run and returns the bytes freed.
func (r *reclaim) done() int64 {
	if r.free < r.minFree {
		r.p.log.Warn("disk space still low after pruning every idle cache", "freeGB", gb(r.free), "minGB", gb(r.minFree))
	}
	return r.free - r.start
}

// pruneIdleSlots runs every slot prune on every idle slot.
func (r *reclaim) pruneIdleSlots(ctx context.Context) bool {
	for _, st := range slotPrunes {
		for _, i := range r.p.idleSlots() {
			if !r.p.claimIdle(i) {
				continue
			}
			before := r.free
			err := r.ops.execSlot(ctx, i, st.script)
			r.p.unclaim(ctx, i)
			if err != nil {
				r.p.log.Warn("prune slot", "slot", i, "what", st.what, "error", err)
			}
			if !r.measure(st.what+" of slot "+strconv.Itoa(i), before) {
				return false
			}
		}
	}
	return true
}

// evictActionsCache evicts actions cache entries, least recently used first:
// they cost one download each, less than a per-owner volume. Entries on
// another filesystem would free nothing here.
func (r *reclaim) evictActionsCache() bool {
	b, _ := r.p.cacheBroker()
	if b == nil || !r.ops.onDisk(b.Dir()) {
		return true
	}
	before := r.free
	if b.Evict(r.minFree-r.free) > 0 {
		return r.measure("actions cache entries", before)
	}
	return true
}

// removeCacheVolumes removes the per-owner cache volumes no container uses,
// largest first. No runner may be created meanwhile (StartRunner holds the
// read side of volMu), so "unused" stays true.
func (r *reclaim) removeCacheVolumes(ctx context.Context) {
	r.p.volMu.Lock()
	defer r.p.volMu.Unlock()
	vols, err := r.ops.volumes(ctx)
	if err != nil {
		r.p.log.Error("list volumes", "error", err)
		return
	}
	for _, v := range prunableVolumes(vols) {
		before := r.free
		if err := r.ops.removeVolume(ctx, v.Name); err != nil {
			r.p.log.Warn("remove cache volume", "volume", v.Name, "error", err)
			continue
		}
		r.p.mu.Lock()
		delete(r.p.chowned, v.Name) // recreated (and chowned) by the next runner
		r.p.mu.Unlock()
		if !r.measure("cache volume "+v.Name, before) {
			return
		}
	}
}

// prunableVolumes returns the per-owner cache volumes no container uses,
// largest first.
func prunableVolumes(vols []volInfo) []volInfo {
	var out []volInfo
	for _, v := range vols {
		if v.InUse {
			continue
		}
		for _, pre := range prunableVolumePrefixes {
			if strings.HasPrefix(v.Name, pre) && len(v.Name) > len(pre) {
				out = append(out, v)
				break
			}
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Size > out[j].Size })
	return out
}

// idleSlots lists ready slots, lowest index first.
func (p *Pool) idleSlots() []int {
	p.mu.Lock()
	defer p.mu.Unlock()
	var out []int
	for i, s := range p.slots {
		if s.Status == statusReady && !s.retire && !s.dirty {
			out = append(out, i)
		}
	}
	sort.Ints(out)
	return out
}

// claimIdle marks a ready slot as cleaning so Acquire skips it.
func (p *Pool) claimIdle(i int) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	s, ok := p.slots[i]
	if !ok || s.Status != statusReady || s.retire || s.dirty {
		return false
	}
	s.Status = statusCleaning
	return true
}

// unclaim makes a slot claimed by claimIdle available again (or retires it if
// it was retired meanwhile).
func (p *Pool) unclaim(ctx context.Context, i int) {
	p.mu.Lock()
	s, ok := p.slots[i]
	retire := ok && s.retire
	p.mu.Unlock()
	if !ok {
		return
	}
	if retire {
		p.removeRetired(ctx, i)
		return
	}
	p.setStatus(i, statusReady, "")
}

func gb(b int64) float64 { return float64(b*10>>30) / 10 }

// dockerDiskOps is the real diskOps.
type dockerDiskOps struct{ p *Pool }

func (d dockerDiskOps) freeBytes() (int64, error) {
	_, free, err := d.p.DiskSpace()
	return free, err
}

func (d dockerDiskOps) execSlot(ctx context.Context, slot int, script string) error {
	_, code, err := d.p.exec(ctx, slotName(slot), script)
	if err == nil && code != 0 {
		return errExit(code)
	}
	return err
}

func (d dockerDiskOps) volumes(ctx context.Context) ([]volInfo, error) {
	du, err := d.p.cli.DiskUsage(ctx, types.DiskUsageOptions{Types: []types.DiskUsageObject{types.VolumeObject}})
	if err != nil {
		return nil, err
	}
	// Any container referencing a volume counts, running or not.
	list, err := d.p.cli.ContainerList(ctx, container.ListOptions{All: true})
	if err != nil {
		return nil, err
	}
	used := map[string]bool{}
	for _, c := range list {
		for _, m := range c.Mounts {
			if m.Name != "" {
				used[m.Name] = true
			}
		}
	}
	var out []volInfo
	for _, v := range du.Volumes {
		if v == nil || v.Labels[LabelManaged] != "true" {
			continue
		}
		size := int64(-1)
		if v.UsageData != nil {
			size = v.UsageData.Size
		}
		// RefCount is Docker's own view; trust whichever says "in use".
		inUse := used[v.Name] || (v.UsageData != nil && v.UsageData.RefCount > 0)
		out = append(out, volInfo{Name: v.Name, Size: size, InUse: inUse})
	}
	return out, nil
}

func (d dockerDiskOps) onDisk(dir string) bool {
	var a, b syscall.Stat_t
	return syscall.Stat(dir, &a) == nil && syscall.Stat(d.p.DiskPath(), &b) == nil && a.Dev == b.Dev
}

func (d dockerDiskOps) removeVolume(ctx context.Context, name string) error {
	// Not forced: Docker refuses to remove a volume a container still uses.
	err := d.p.cli.VolumeRemove(ctx, name, false)
	if cerrdefs.IsNotFound(err) {
		return nil
	}
	return err
}

type errExit int

func (e errExit) Error() string { return "exit code " + strconv.Itoa(int(e)) }
