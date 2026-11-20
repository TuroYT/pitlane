package pool

import (
	"context"
	"strings"
)

// storageDriverScript prints the storage driver of a slot's daemon.
const storageDriverScript = "docker info --format '{{.Driver}}'"

// SlowStorageDriver reports whether a slot daemon's storage driver is known
// and is not overlay-based: vfs copies every layer on each container start
// and fuse-overlayfs runs in user space, both much slower than overlay2.
// overlayfs is the name the containerd snapshotter gives to overlay.
func SlowStorageDriver(driver string) bool {
	switch driver {
	case "", "overlay2", "overlayfs":
		return false
	}
	return true
}

// recordStorageDriver reads the storage driver of slot i's running daemon,
// keeps it on the slot (it is not persisted) and warns when it is slow. A
// failed read leaves it unknown and never fails the slot.
func (p *Pool) recordStorageDriver(ctx context.Context, i int) {
	driver := p.slotStorageDriver(ctx, i)
	p.mu.Lock()
	if s, ok := p.slots[i]; ok {
		s.StorageDriver = driver
	}
	p.mu.Unlock()
	if SlowStorageDriver(driver) {
		p.log.Warn("slot Docker daemon uses a slow storage driver, jobs with heavy file or layer work will be slow; see docs/troubleshooting.md",
			"slot", i, "driver", driver)
	}
}

// slotStorageDriver returns the storage driver of slot i's daemon, or ""
// when it cannot be read.
func (p *Pool) slotStorageDriver(ctx context.Context, i int) string {
	out, code, err := p.ops().exec(ctx, slotName(i), storageDriverScript)
	if err != nil || code != 0 {
		p.log.Debug("cannot read the slot storage driver", "slot", i, "code", code, "error", err)
		return ""
	}
	return strings.TrimSpace(out)
}
