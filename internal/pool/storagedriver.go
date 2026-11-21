package pool

import (
	"context"
	"strings"
	"time"
)

// storageDriverScript prints the storage driver of a slot's daemon.
const storageDriverScript = "docker info --format '{{.Driver}}'"

// storageDriverTimeout bounds the storage driver read: a hung daemon leaves
// the driver unknown instead of stalling the slot start.
const storageDriverTimeout = 10 * time.Second

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
	ctx, cancel := context.WithTimeout(ctx, storageDriverTimeout)
	defer cancel()
	type result struct {
		out  string
		code int
		err  error
	}
	// exec does not bound its attach handshake or its output read with ctx,
	// so the read runs aside and is abandoned on timeout. That only happens
	// when the slot daemon is already unresponsive; the abandoned exec ends
	// when the daemon answers or the connection closes. The channel is
	// buffered so its goroutine never blocks.
	done := make(chan result, 1)
	go func() {
		out, code, err := p.ops().exec(ctx, slotName(i), storageDriverScript)
		done <- result{out, code, err}
	}()
	select {
	case r := <-done:
		if r.err != nil || r.code != 0 {
			p.log.Debug("cannot read the slot storage driver", "slot", i, "code", r.code, "error", r.err)
			return ""
		}
		return strings.TrimSpace(r.out)
	case <-ctx.Done():
		p.log.Debug("slot storage driver read timed out", "slot", i, "error", ctx.Err())
		return ""
	}
}
