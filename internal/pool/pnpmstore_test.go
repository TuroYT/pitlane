package pool

import (
	"context"
	"net/http"
	"os"
	"path/filepath"
	"syscall"
	"testing"
)

func write(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func inode(t *testing.T, path string) uint64 {
	t.Helper()
	var st syscall.Stat_t
	if err := syscall.Lstat(path, &st); err != nil {
		t.Fatalf("%s: %v", path, err)
	}
	return st.Ino
}

func TestMergeStoreDirs(t *testing.T) {
	root := t.TempDir()
	work, canon := filepath.Join(root, "work"), filepath.Join(root, "canon")
	slot := filepath.Join(work, pnpmStoreDir)
	write(t, filepath.Join(slot, "v3/files/aa/new"), "downloaded by the job")
	write(t, filepath.Join(slot, "v3/files/cc/dup"), "same")
	write(t, filepath.Join(slot, "v11/index.db"), "slot database")
	write(t, filepath.Join(canon, "v3/files/bb/old"), "known package")
	write(t, filepath.Join(canon, "v3/files/cc/dup"), "same")
	secret := filepath.Join(root, "secret")
	write(t, secret, "host file")
	if err := os.Symlink(root, filepath.Join(slot, "v3/files/evil")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(secret, filepath.Join(slot, "v3/files/aa/link")); err != nil {
		t.Fatal(err)
	}
	uid, gid := os.Getuid(), os.Getgid()

	st, err := mergeStoreDirs(slot, canon, uid, gid)
	if err != nil {
		t.Fatal(err)
	}
	if st.up != 1 || st.down != 1 || st.dedup != 1 {
		t.Fatalf("stats = %+v, want 1 up, 1 down, 1 dedup", st)
	}
	for _, f := range []string{"v3/files/aa/new", "v3/files/bb/old", "v3/files/cc/dup"} {
		if inode(t, filepath.Join(slot, f)) != inode(t, filepath.Join(canon, f)) {
			t.Errorf("%s: slot and canonical are not the same inode", f)
		}
	}
	for _, f := range []string{"v3/files/evil", "v3/files/aa/link", "v11/index.db"} {
		if _, err := os.Lstat(filepath.Join(canon, f)); err == nil {
			t.Errorf("%s must not reach the canonical store", f)
		}
	}
	// Idempotent.
	if st, err := mergeStoreDirs(slot, canon, uid, gid); err != nil || st != (mergeStats{}) {
		t.Fatalf("second merge = %+v, %v; want nothing to do", st, err)
	}

	// A fresh (wiped) slot is filled from the canonical store.
	if err := os.RemoveAll(slot); err != nil {
		t.Fatal(err)
	}
	st, err = mergeStoreDirs(slot, canon, uid, gid)
	if err != nil || st.down != 3 || st.up != 0 {
		t.Fatalf("fill = %+v, %v; want 3 down", st, err)
	}
}

// The Docker side of the pnpm store sync, against a fake Docker API whose
// volumes live in temporary directories.
func TestDockerSlotOpsStore(t *testing.T) {
	root := t.TempDir()
	work, canon := filepath.Join(root, "work"), filepath.Join(root, "canon")
	write(t, filepath.Join(canon, "v3/files/aa/pkg"), "package")
	if err := os.Mkdir(work, 0o755); err != nil {
		t.Fatal(err)
	}
	p := newTestPool("owner")
	newDockerAPI(t, p, map[string]http.HandlerFunc{
		"GET /volumes/pitlane-slot-0-work":     reply(map[string]string{"Name": "pitlane-slot-0-work", "Mountpoint": work}),
		"GET /volumes/pitlane-cache-acme.pnpm": reply(map[string]string{"Name": "pitlane-cache-acme.pnpm", "Mountpoint": canon}),
	})
	ops := dockerSlotOps{p}
	ctx := context.Background()
	if !ops.slotStoreEmpty(ctx, 0) {
		t.Fatal("a work volume without a store is empty")
	}
	st, err := ops.mergeStore(ctx, 0, "pitlane-cache-acme.pnpm")
	if err != nil || st.down != 1 {
		t.Fatalf("merge: %+v %v", st, err)
	}
	if inode(t, filepath.Join(work, pnpmStoreDir, "v3/files/aa/pkg")) != inode(t, filepath.Join(canon, "v3/files/aa/pkg")) {
		t.Fatal("the slot store must share the canonical inode")
	}
	if ops.slotStoreEmpty(ctx, 0) {
		t.Fatal("the store is filled now")
	}
}
