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
}

// DiskPath returns the filesystem whose free space Pitlane watches: Docker's
// data directory when visible, else Pitlane's own data directory.
func (p *Pool) DiskPath() string {
	if _, err := os.Stat("/var/lib/docker"); err == nil {
		return "/var/lib/docker"
	}
	if f := p.Config().StateFile; f != "" {
		return filepath.Dir(f)
	}
	return "."
}

// DiskSpace returns the total and available bytes of DiskPath.
func (p *Pool) DiskSpace() (total, free int64, err error) {
	var st syscall.Statfs_t
	if err := syscall.Statfs(p.DiskPath(), &st); err != nil {
		return 0, 0, err
	}
	return int64(st.Blocks) * st.Bsize, int64(st.Bavail) * st.Bsize, nil //nolint:gosec // G115: block counts of a real filesystem fit in int64
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

// reclaimDisk frees space, cheapest loss first, until minFree bytes are
// available: build caches of idle slots, then their images, then the largest
// per-owner cache volumes no container uses. Busy slots are never touched:
// an idle slot is marked "cleaning" while it is pruned so it cannot be handed
// out meanwhile. It returns the bytes freed.
func (p *Pool) reclaimDisk(ctx context.Context, minFree int64, ops diskOps) int64 {
	free, err := ops.freeBytes()
	if err != nil || free >= minFree {
		return 0
	}
	start := free
	p.log.Warn("low disk space, pruning caches", "freeGB", gb(free), "minGB", gb(minFree))
	// measure refreshes free; false means "stop" (enough space, or unknown).
	measure := func(what string, before int64) bool {
		f, err := ops.freeBytes()
		if err != nil {
			p.log.Error("disk space unknown, pruning stopped", "error", err)
			return false
		}
		free = f
		p.log.Info("pruned", "what", what, "freedMB", (f-before)>>20, "freeGB", gb(f))
		return f < minFree
	}
	done := func() int64 {
		if free < minFree {
			p.log.Warn("disk space still low after pruning every idle cache", "freeGB", gb(free), "minGB", gb(minFree))
		}
		return free - start
	}

	steps := []struct{ what, script string }{
		{"build cache", "docker builder prune -af >/dev/null"},
		{"unused images", "docker image prune -af >/dev/null"},
	}
	for _, st := range steps {
		for _, i := range p.idleSlots() {
			if !p.claimIdle(i) {
				continue
			}
			before := free
			err := ops.execSlot(ctx, i, st.script)
			p.unclaim(ctx, i)
			if err != nil {
				p.log.Warn("prune slot", "slot", i, "what", st.what, "error", err)
			}
			if !measure(st.what+" of slot "+strconv.Itoa(i), before) {
				return done()
			}
		}
	}

	// Actions cache entries, least recently used first: they cost one
	// download each, less than a per-owner volume.
	if b, _ := p.cacheBroker(); b != nil {
		before := free
		if b.Evict(minFree-free) > 0 && !measure("actions cache entries", before) {
			return done()
		}
	}

	// Volumes: no runner may be created while they are inspected and removed
	// (StartRunner holds the read side), so "unused" stays true.
	p.volMu.Lock()
	defer p.volMu.Unlock()
	vols, err := ops.volumes(ctx)
	if err != nil {
		p.log.Error("list volumes", "error", err)
		return done()
	}
	for _, v := range prunableVolumes(vols) {
		before := free
		if err := ops.removeVolume(ctx, v.Name); err != nil {
			p.log.Warn("remove cache volume", "volume", v.Name, "error", err)
			continue
		}
		p.mu.Lock()
		delete(p.chowned, v.Name) // recreated (and chowned) by the next runner
		p.mu.Unlock()
		if !measure("cache volume "+v.Name, before) {
			return done()
		}
	}
	return done()
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
		if s.Status == "ready" && !s.retire && !s.dirty {
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
	if !ok || s.Status != "ready" || s.retire || s.dirty {
		return false
	}
	s.Status = "cleaning"
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
		p.removeSlot(ctx, i)
		p.signal()
		return
	}
	p.setStatus(i, "ready", "")
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
