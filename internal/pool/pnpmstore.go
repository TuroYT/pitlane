package pool

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"syscall"
	"time"

	"github.com/docker/docker/api/types/volume"
	"golang.org/x/sys/unix"
)

// Shared pnpm store
//
// pnpm only hardlinks packages into node_modules when its store is on the same
// mount as the workspace, so each slot keeps a store in its work volume. Six
// independent stores of the same packages are six copies on disk and, worse,
// six copies competing for the page cache: on a slow disk most installs then
// read cold inodes and spend 15-30 s "reusing" packages instead of ~5 s.
//
// The fix keeps one canonical store per owner (a volume no container ever
// mounts) and makes every slot store of that owner a hardlink farm of it: same
// inodes, hence one copy in the page cache shared by all slots. After each job
// the slot store and the canonical store are merged both ways (new packages go
// up, missing ones come down, duplicate copies are replaced by the canonical
// inode). Only content-addressed files are shared: pnpm's mutable databases
// (index.db, projects/, tmp/) stay per slot.
//
// Security: the canonical store only ever receives files from slots serving
// the same owner (slots are wiped on owner change before any merge), strict
// jobs never touch it, and it is walked with one directory file descriptor
// per level and O_NOFOLLOW, so a job that planted symlinks in its slot store
// cannot make Pitlane (root on the host) link files from outside the volume.

// sharedStoreDirs are the content-addressed parts of a pnpm store version
// directory (v3 for pnpm 9, v10, v11…).
var sharedStoreDirs = map[string]bool{"files": true, "index": true}

func pnpmStoreVolume(scope, owner string) string {
	return cacheVolume(scope, owner) + ".pnpm"
}

// storeSync tracks which slot stores are merged with their canonical store.
type storeSync struct {
	mu     sync.Mutex
	synced map[int]string // slot -> canonical volume it was last merged with
	locks  map[string]*sync.Mutex
	warned bool
}

func (s *storeSync) lock(vol string) *sync.Mutex {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.locks == nil {
		s.locks = map[string]*sync.Mutex{}
	}
	m := s.locks[vol]
	if m == nil {
		m = &sync.Mutex{}
		s.locks[vol] = m
	}
	return m
}

func (s *storeSync) isSynced(slot int, vol string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.synced[slot] == vol
}

func (s *storeSync) set(slot int, vol string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.synced == nil {
		s.synced = map[int]string{}
	}
	if vol == "" {
		delete(s.synced, slot)
	} else {
		s.synced[slot] = vol
	}
}

// forgetStore marks a slot store as not merged (wiped, rebuilt, strict).
func (p *Pool) forgetStore(slot int) { p.stores.set(slot, "") }

// syncPnpmStore merges the slot's pnpm store with the owner's canonical store.
// It must only run while no job runs in the slot. Failures are logged and
// harmless: pnpm then simply works from the slot store alone.
func (p *Pool) syncPnpmStore(ctx context.Context, slot int, owner string) {
	if owner == "" || owner == StrictOwner || owner == UnknownOwner {
		return
	}
	vol := pnpmStoreVolume(p.Config().CacheScope, owner)
	m := p.stores.lock(vol)
	m.Lock()
	defer m.Unlock()
	// The disk reclaimer removes cache volumes under the write lock.
	p.volMu.RLock()
	defer p.volMu.RUnlock()
	start := time.Now()
	st, err := p.mergeStore(ctx, slot, vol)
	if err != nil {
		p.stores.set(slot, "")
		p.stores.mu.Lock()
		warned := p.stores.warned
		p.stores.warned = true
		p.stores.mu.Unlock()
		if !warned || !errors.Is(err, errNoHostAccess) {
			p.log.Warn("pnpm store not shared", "slot", slot, "error", err)
		}
		return
	}
	p.stores.set(slot, vol)
	p.log.Info("pnpm store merged", "slot", slot, "store", vol, "up", st.up, "down", st.down,
		"deduped", st.dedup, "ms", time.Since(start).Milliseconds())
}

// slotStoreEmpty reports whether the slot has no pnpm store content yet.
func (p *Pool) slotStoreEmpty(ctx context.Context, slot int) bool {
	work, err := p.cli.VolumeInspect(ctx, slotWork(slot))
	if err != nil {
		return false
	}
	list, err := os.ReadDir(filepath.Join(work.Mountpoint, pnpmStoreDir))
	if errors.Is(err, os.ErrNotExist) {
		_, err = os.Stat(work.Mountpoint)
		return err == nil
	}
	return err == nil && len(list) == 0
}

var errNoHostAccess = errors.New("Docker volumes are not reachable from Pitlane's filesystem")

func (p *Pool) mergeStore(ctx context.Context, slot int, vol string) (mergeStats, error) {
	if _, err := p.cli.VolumeInspect(ctx, vol); err != nil {
		if _, err := p.cli.VolumeCreate(ctx, volume.CreateOptions{Name: vol, Labels: map[string]string{LabelManaged: "true"}}); err != nil {
			return mergeStats{}, fmt.Errorf("create volume %s: %w", vol, err)
		}
	}
	canon, err := p.cli.VolumeInspect(ctx, vol)
	if err != nil {
		return mergeStats{}, err
	}
	work, err := p.cli.VolumeInspect(ctx, slotWork(slot))
	if err != nil {
		return mergeStats{}, err
	}
	var a, b syscall.Stat_t
	if syscall.Stat(canon.Mountpoint, &a) != nil || syscall.Stat(work.Mountpoint, &b) != nil {
		return mergeStats{}, errNoHostAccess
	}
	if a.Dev != b.Dev {
		return mergeStats{}, fmt.Errorf("%s and %s are on different filesystems", vol, slotWork(slot))
	}
	uid, gid := runnerIDs()
	return mergeStoreDirs(filepath.Join(work.Mountpoint, pnpmStoreDir), canon.Mountpoint, uid, gid)
}

func runnerIDs() (int, int) {
	var uid, gid int
	if _, err := fmt.Sscanf(runnerUID, "%d:%d", &uid, &gid); err != nil {
		panic("invalid runnerUID constant: " + runnerUID)
	}
	return uid, gid
}

type mergeStats struct{ up, down, dedup int }

// mergeStoreDirs merges the store at slotRoot (created if missing, owned by
// uid:gid) with the canonical store at canonRoot.
func mergeStoreDirs(slotRoot, canonRoot string, uid, gid int) (mergeStats, error) {
	var st mergeStats
	canon, err := unix.Open(canonRoot, unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_RDONLY|unix.O_CLOEXEC, 0)
	if err != nil {
		return st, err
	}
	defer unix.Close(canon)
	parent, err := unix.Open(filepath.Dir(slotRoot), unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_RDONLY|unix.O_CLOEXEC, 0)
	if err != nil {
		return st, err
	}
	defer unix.Close(parent)
	slot, err := openOrMkdir(parent, filepath.Base(slotRoot), uid, gid)
	if err != nil {
		return st, err
	}
	defer unix.Close(slot)

	// Top level: one directory per store layout version (v3, v10…).
	versions := map[string]bool{}
	for _, side := range []int{slot, canon} {
		names, err := readDir(side)
		if err != nil {
			return st, err
		}
		for _, n := range names {
			if n.dir {
				versions[n.name] = true
			}
		}
	}
	for v := range versions {
		if len(v) < 2 || v[0] != 'v' {
			continue
		}
		if _, err := strconv.Atoi(v[1:]); err != nil {
			continue
		}
		sv, err := openOrMkdir(slot, v, uid, gid)
		if err != nil {
			return st, err
		}
		cv, err := openOrMkdir(canon, v, -1, -1)
		if err != nil {
			unix.Close(sv)
			return st, err
		}
		for d := range sharedStoreDirs {
			if !exists(sv, d) && !exists(cv, d) {
				continue
			}
			_ = mergeSub(sv, cv, d, uid, gid, &st)
		}
		unix.Close(sv)
		unix.Close(cv)
	}
	return st, nil
}

// mergeSub merges directory name of slotParent and canonParent recursively.
func mergeSub(slotParent, canonParent int, name string, uid, gid int, st *mergeStats) error {
	s, err := openOrMkdir(slotParent, name, uid, gid)
	if err != nil {
		return err
	}
	defer unix.Close(s)
	c, err := openOrMkdir(canonParent, name, -1, -1)
	if err != nil {
		return err
	}
	defer unix.Close(c)
	return mergeDir(s, c, uid, gid, st)
}

func mergeDir(s, c int, uid, gid int, st *mergeStats) error {
	sNames, err := readDir(s)
	if err != nil {
		return err
	}
	cNames, err := readDir(c)
	if err != nil {
		return err
	}
	inSlot := make(map[string]bool, len(sNames))
	for _, n := range sNames {
		inSlot[n.name] = true
		if n.dir {
			_ = mergeSub(s, c, n.name, uid, gid, st) // odd entry: skip it
			continue
		}
		var ss unix.Stat_t
		if unix.Fstatat(s, n.name, &ss, unix.AT_SYMLINK_NOFOLLOW) != nil || ss.Mode&unix.S_IFMT != unix.S_IFREG {
			continue // symlinks, sockets…: never shared
		}
		var cs unix.Stat_t
		switch err := unix.Fstatat(c, n.name, &cs, unix.AT_SYMLINK_NOFOLLOW); {
		case errors.Is(err, unix.ENOENT):
			// New package downloaded by the job: publish it.
			if unix.Linkat(s, n.name, c, n.name, 0) == nil {
				st.up++
			}
		case err != nil:
			continue
		case cs.Mode&unix.S_IFMT != unix.S_IFREG || (cs.Dev == ss.Dev && cs.Ino == ss.Ino):
			continue
		case cs.Size == ss.Size:
			// Same content-addressed name, separate copy: use the canonical
			// inode so the page cache holds it once.
			tmp := ".pitlane-tmp-" + n.name
			_ = unix.Unlinkat(s, tmp, 0)
			if unix.Linkat(c, n.name, s, tmp, 0) == nil {
				if unix.Renameat(s, tmp, s, n.name) == nil {
					st.dedup++
				} else {
					_ = unix.Unlinkat(s, tmp, 0)
				}
			}
		}
	}
	for _, n := range cNames {
		if inSlot[n.name] {
			continue
		}
		if n.dir {
			_ = mergeSub(s, c, n.name, uid, gid, st) // odd entry: skip it
			continue
		}
		var cs unix.Stat_t
		if unix.Fstatat(c, n.name, &cs, unix.AT_SYMLINK_NOFOLLOW) != nil || cs.Mode&unix.S_IFMT != unix.S_IFREG {
			continue
		}
		if unix.Linkat(c, n.name, s, n.name, 0) == nil {
			st.down++
		}
	}
	return nil
}

type dirEntry struct {
	name string
	dir  bool
}

// readDir lists a directory file descriptor without following anything.
func readDir(fd int) ([]dirEntry, error) {
	dup, err := unix.Dup(fd)
	if err != nil {
		return nil, err
	}
	f := os.NewFile(uintptr(dup), "")
	defer func() { _ = f.Close() }() // read only
	if _, err := f.Seek(0, 0); err != nil {
		return nil, err
	}
	list, err := f.ReadDir(-1)
	if err != nil {
		return nil, err
	}
	out := make([]dirEntry, 0, len(list))
	for _, e := range list {
		if len(e.Name()) > 13 && e.Name()[:13] == ".pitlane-tmp-" {
			_ = unix.Unlinkat(fd, e.Name(), 0)
			continue
		}
		out = append(out, dirEntry{name: e.Name(), dir: e.Type().IsDir()})
	}
	return out, nil
}

func exists(dir int, name string) bool {
	var s unix.Stat_t
	return unix.Fstatat(dir, name, &s, unix.AT_SYMLINK_NOFOLLOW) == nil
}

// openOrMkdir opens dir/name as a directory, creating it (owned by uid:gid
// when uid >= 0) when missing. It never follows a symlink.
func openOrMkdir(dir int, name string, uid, gid int) (int, error) {
	flags := unix.O_DIRECTORY | unix.O_NOFOLLOW | unix.O_RDONLY | unix.O_CLOEXEC
	fd, err := unix.Openat(dir, name, flags, 0)
	if errors.Is(err, unix.ENOENT) {
		if err := unix.Mkdirat(dir, name, 0o755); err != nil && !errors.Is(err, unix.EEXIST) {
			return -1, err
		}
		fd, err = unix.Openat(dir, name, flags, 0)
		if err == nil && uid >= 0 {
			_ = unix.Fchown(fd, uid, gid)
		}
	}
	if err != nil {
		return -1, fmt.Errorf("open %s: %w", name, err)
	}
	return fd, nil
}
