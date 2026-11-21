package pool

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"testing/synctest"
	"time"
)

func TestSlowStorageDriver(t *testing.T) {
	for driver, want := range map[string]bool{
		"":               false, // unknown
		"overlay2":       false,
		"overlayfs":      false, // containerd snapshotter
		"vfs":            true,
		"fuse-overlayfs": true,
		"zfs":            true,
	} {
		if got := SlowStorageDriver(driver); got != want {
			t.Errorf("SlowStorageDriver(%q) = %v, want %v", driver, got, want)
		}
	}
}

// startSlot reads the storage driver of a running (adopted) slot daemon,
// shows it in Slots, keeps it out of the persisted state and warns only for
// a driver that is not overlay-based.
func TestStartSlotRecordsStorageDriver(t *testing.T) {
	for _, tc := range []struct {
		name       string
		out        string
		code       int
		err        error
		wantDriver string
		wantWarn   bool
	}{
		{"overlay2", "overlay2\n", 0, nil, "overlay2", false},
		{"containerd snapshotter", "overlayfs\n", 0, nil, "overlayfs", false},
		{"vfs", "vfs\n", 0, nil, "vfs", true},
		{"fuse-overlayfs", "fuse-overlayfs\n", 0, nil, "fuse-overlayfs", true},
		{"exec fails", "", -1, errors.New("exec refused"), "", false},
		{"docker info fails", "vfs\n", 1, nil, "", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p, ops := slotPool(1, SlotState{Index: 0, Status: statusStarting})
			var logs bytes.Buffer
			p.log = slog.New(slog.NewTextHandler(&logs, nil))
			// waitDind needs a working exec, so the failure is injected on
			// the storage driver read only.
			ops.outputs = map[string]string{storageDriverScript: tc.out}
			p.sops = &driverOps{fakeSlotOps: ops, code: tc.code, err: tc.err}
			routes := map[string]http.HandlerFunc{}
			slotUpRoutes(routes, 0)
			newDockerAPI(t, p, routes)

			p.startSlot(context.Background(), 0)

			st := p.Slots()[0]
			if st.Status != statusReady || st.StorageDriver != tc.wantDriver {
				t.Fatalf("status %q driver %q, want ready %q", st.Status, st.StorageDriver, tc.wantDriver)
			}
			if warned := strings.Contains(logs.String(), "slow storage driver"); warned != tc.wantWarn {
				t.Fatalf("warned %v, want %v: %s", warned, tc.wantWarn, logs.String())
			}
			if tc.wantWarn && !strings.Contains(logs.String(), "driver="+tc.wantDriver) {
				t.Fatalf("warning without the driver: %s", logs.String())
			}
		})
	}
}

// driverOps fails the storage driver read with code and err, and passes
// every other exec to the embedded fake.
type driverOps struct {
	*fakeSlotOps
	code int
	err  error
}

func (d *driverOps) exec(ctx context.Context, name, script string) (string, int, error) {
	out, code, err := d.fakeSlotOps.exec(ctx, name, script)
	if script == storageDriverScript && (d.code != 0 || d.err != nil) {
		return out, d.code, d.err
	}
	return out, code, err
}

func TestStorageDriverNotPersisted(t *testing.T) {
	p := newTestPool("owner", SlotState{Index: 0, Status: statusReady, Owner: "acme"})
	p.cfg.StateFile = filepath.Join(t.TempDir(), "slots.json")
	p.slots[0].StorageDriver = "vfs"
	p.mu.Lock()
	p.saveLocked()
	p.mu.Unlock()
	b, err := os.ReadFile(p.cfg.StateFile)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), "acme") || strings.Contains(string(b), "vfs") {
		t.Fatalf("storage driver persisted: %s", b)
	}
}

// hungOps blocks the storage driver read until release is closed, whatever
// its context, like a slot daemon that does not answer.
type hungOps struct {
	*fakeSlotOps
	release chan struct{}
	exited  chan struct{}
}

func (h hungOps) exec(context.Context, string, string) (string, int, error) {
	<-h.release
	close(h.exited)
	return "overlay2", 0, nil
}

// A hung daemon leaves the driver unknown after storageDriverTimeout instead
// of stalling the slot start.
func TestStorageDriverReadTimesOut(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		p := newTestPool("owner", SlotState{Index: 0, Status: statusStarting})
		ops := hungOps{fakeSlotOps: &fakeSlotOps{}, release: make(chan struct{}), exited: make(chan struct{})}
		p.sops = ops
		start := time.Now()
		p.recordStorageDriver(context.Background(), 0)
		if waited := time.Since(start); waited != storageDriverTimeout {
			t.Fatalf("waited %v, want %v", waited, storageDriverTimeout)
		}
		if d := p.Slots()[0].StorageDriver; d != "" {
			t.Fatalf("driver %q, want unknown", d)
		}
		select {
		case <-ops.exited:
			t.Fatal("the exec should still be blocked")
		default:
		}
		close(ops.release) // the abandoned read ends: no goroutine leaks
		<-ops.exited
		synctest.Wait()
	})
}
