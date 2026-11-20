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
)

// A runner image that cannot be pulled stops Start; a registry mirror that
// cannot run does not.
func TestStartFailureModes(t *testing.T) {
	t.Run("no runner image", func(t *testing.T) {
		p, _ := slotPool(1)
		routes := map[string]http.HandlerFunc{
			"GET /images/" + p.cfg.DindImage + "/json":     reply(map[string]string{"Id": "x"}),
			"GET /images/" + p.cfg.RunnerImage + "/json":   replyStatus(http.StatusNotFound),
			"GET /images/" + OfficialRunnerImage + "/json": replyStatus(http.StatusNotFound),
			"GET /images/json":                             replyStatus(http.StatusInternalServerError),
			"POST /images/create":                          replyStatus(http.StatusInternalServerError),
		}
		newDockerAPI(t, p, routes)
		if err := p.Start(context.Background()); err == nil {
			t.Fatal("Start must fail without any runner image")
		}
	})
	t.Run("a mirror that cannot run is not fatal", func(t *testing.T) {
		p, _ := slotPool(1)
		p.env = testHost(t)
		p.cfg.NoQEMU, p.cfg.Mirror, p.cfg.Slots = true, true, 1
		var logs bytes.Buffer
		p.log = slog.New(slog.NewTextHandler(&logs, nil))
		routes := map[string]http.HandlerFunc{
			"GET /images/" + p.cfg.DindImage + "/json":   reply(map[string]string{"Id": "x"}),
			"GET /images/" + p.cfg.RunnerImage + "/json": reply(map[string]string{"Id": "x"}),
			"GET /networks/pitlane-mirror":               replyStatus(http.StatusNotFound),
			"POST /networks/create":                      replyStatus(http.StatusInternalServerError),
			"POST /networks/pitlane-mirror/disconnect":   replyStatus(http.StatusNotFound),
			"POST /networks/pitlane-slot-0/connect":      replyStatus(http.StatusInternalServerError),
		}
		slotUpRoutes(routes, 0)
		newDockerAPI(t, p, routes)
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		if err := p.Start(ctx); err != nil || p.FreeSlots() != 1 {
			t.Fatalf("err %v free %d", err, p.FreeSlots())
		}
		if !strings.Contains(logs.String(), "registry mirror unavailable") || !strings.Contains(logs.String(), "connect mirror to slot network") {
			t.Fatalf("the failures must be logged:\n%s", logs.String())
		}
	})
}

func TestEnsureMirrorFailures(t *testing.T) {
	ok := func() map[string]http.HandlerFunc {
		return map[string]http.HandlerFunc{
			"GET /networks/pitlane-mirror":         reply(map[string]string{"Id": "n"}),
			"GET /containers/pitlane-mirror/json":  replyStatus(http.StatusNotFound),
			"GET /images/" + mirrorImage + "/json": reply(map[string]string{"Id": "x"}),
			"GET /volumes/" + mirrorVolume:         reply(map[string]string{"Name": mirrorVolume}),
			"POST /containers/create":              reply(map[string]string{"Id": "m"}),
		}
	}
	for name, tc := range map[string]struct {
		mod  func(map[string]http.HandlerFunc)
		want string
	}{
		"network": {func(r map[string]http.HandlerFunc) {
			r["GET /networks/pitlane-mirror"] = replyStatus(http.StatusNotFound)
			r["POST /networks/create"] = replyStatus(http.StatusInternalServerError)
		}, "create network"},
		"image": {func(r map[string]http.HandlerFunc) {
			r["GET /images/"+mirrorImage+"/json"] = replyStatus(http.StatusNotFound)
			r["POST /images/create"] = replyStatus(http.StatusInternalServerError)
		}, "pull"},
		"volume": {func(r map[string]http.HandlerFunc) {
			r["GET /volumes/"+mirrorVolume] = replyStatus(http.StatusNotFound)
			r["POST /volumes/create"] = replyStatus(http.StatusInternalServerError)
		}, "create volume"},
		"start": {func(r map[string]http.HandlerFunc) {
			r["POST /containers/pitlane-mirror/start"] = replyStatus(http.StatusInternalServerError)
		}, "start the registry mirror"},
	} {
		t.Run(name, func(t *testing.T) {
			p := newTestPool("owner")
			p.chowned[mirrorVolume] = name != "volume"
			r := ok()
			tc.mod(r)
			newDockerAPI(t, p, r)
			if err := p.ensureMirror(context.Background(), Config{Mirror: true}); err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err %v, want %q", err, tc.want)
			}
		})
	}
}

// A slot whose resources cannot be created does not start.
func TestEnsureSlotResourceFailures(t *testing.T) {
	p, _ := slotPool(0, SlotState{Index: 0, Status: statusStarting})
	newDockerAPI(t, p, map[string]http.HandlerFunc{
		"GET /networks/pitlane-slot-0":       reply(map[string]string{"Id": "n"}),
		"GET /volumes/pitlane-slot-0-docker": replyStatus(http.StatusNotFound),
		"POST /volumes/create":               replyStatus(http.StatusInternalServerError),
	})
	if err := p.ensureSlot(context.Background(), 0); err == nil || !strings.Contains(err.Error(), "create volume pitlane-slot-0-docker") {
		t.Fatalf("err %v", err)
	}
}

func TestExecFailureModes(t *testing.T) {
	p := newTestPool("owner")
	newDockerAPI(t, p, map[string]http.HandlerFunc{
		"POST /containers/box/exec": reply(map[string]string{"Id": "e0"}),
		"POST /exec/e0/start":       replyStatus(http.StatusInternalServerError),
	})
	if _, code, err := p.exec(context.Background(), "box", "x"); err == nil || code != -1 {
		t.Fatalf("an exec that cannot be attached: %d %v", code, err)
	}
	routes, _ := execRoutes(t, func(_, _ string) (string, int) { return "out", 0 }, "box")
	routes["GET /exec/e0/json"] = replyStatus(http.StatusInternalServerError)
	newDockerAPI(t, p, routes)
	if _, code, err := p.exec(context.Background(), "box", "x"); err == nil || code != -1 {
		t.Fatalf("an exec whose exit code is unknown must be an error, not success: %d %v", code, err)
	}
}

// The cgroup of a container is only taken under the cgroup root, whatever
// /proc says.
func TestCgroupFindRefusesEscapes(t *testing.T) {
	fs := fakeRoot(t)
	outside := filepath.Join(filepath.Dir(fs.root), "docker", slotCID)
	fakeCgroup(t, outside, 1, 1, 1, 1, 1, 1)
	if err := os.MkdirAll(filepath.Join(fs.proc, "42"), 0o755); err != nil {
		t.Fatal(err)
	}
	write(t, filepath.Join(fs.proc, "42", "cgroup"), "1:name=x:/ignored\n0::/../docker/"+slotCID+"\n")
	if got := fs.find(slotCID, 42); got != "" {
		t.Fatalf("a cgroup outside the root was returned: %q", got)
	}
	// A name that mentions the ID but is not a cgroup is not one either.
	if err := os.MkdirAll(filepath.Join(fs.root, "docker", slotCID), 0o755); err != nil {
		t.Fatal(err)
	}
	write(t, filepath.Join(fs.proc, "42", "cgroup"), "0::/docker/"+slotCID+"\n")
	if got := fs.find(slotCID, 42); got != "" {
		t.Fatalf("a directory without cgroup.procs: %q", got)
	}
	if got := fs.find("", 0); got != "" {
		t.Fatalf("empty id: %q", got)
	}
}

func TestSlotStoreEmpty(t *testing.T) {
	p := newTestPool("owner")
	work := t.TempDir()
	newDockerAPI(t, p, map[string]http.HandlerFunc{
		"GET /volumes/pitlane-slot-0-work": reply(map[string]string{"Name": "w", "Mountpoint": work}),
		"GET /volumes/pitlane-slot-1-work": reply(map[string]string{"Name": "w", "Mountpoint": filepath.Join(work, "missing")}),
		"GET /volumes/pitlane-slot-2-work": replyStatus(http.StatusNotFound),
	})
	ops := dockerSlotOps{p}
	ctx := context.Background()
	if !ops.slotStoreEmpty(ctx, 0) {
		t.Fatal("no store yet: empty")
	}
	if err := os.Mkdir(filepath.Join(work, pnpmStoreDir), 0o755); err != nil {
		t.Fatal(err)
	}
	if !ops.slotStoreEmpty(ctx, 0) {
		t.Fatal("an empty store directory is empty")
	}
	write(t, filepath.Join(work, pnpmStoreDir, "v3"), "x")
	if ops.slotStoreEmpty(ctx, 0) {
		t.Fatal("a store with content is not empty")
	}
	if ops.slotStoreEmpty(ctx, 1) {
		t.Fatal("a volume that is not reachable is not known to be empty: never merge blindly")
	}
	if ops.slotStoreEmpty(ctx, 2) {
		t.Fatal("an unknown volume is not known to be empty")
	}
}

func TestMergeStoreFailures(t *testing.T) {
	root := t.TempDir()
	work, canon := filepath.Join(root, "work"), filepath.Join(root, "canon")
	if err := os.MkdirAll(work, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(canon, 0o755); err != nil {
		t.Fatal(err)
	}
	const vol = "pitlane-cache-acme.pnpm"
	volume := func(mount string) http.HandlerFunc { return reply(map[string]string{"Name": "v", "Mountpoint": mount}) }
	for name, tc := range map[string]struct {
		routes map[string]http.HandlerFunc
		want   error
		text   string
	}{
		"canonical volume cannot be created":  {map[string]http.HandlerFunc{"GET /volumes/" + vol: replyStatus(http.StatusNotFound), "POST /volumes/create": replyStatus(http.StatusInternalServerError)}, nil, "create volume"},
		"canonical volume unknown":            {map[string]http.HandlerFunc{"GET /volumes/" + vol: replyStatus(http.StatusInternalServerError), "POST /volumes/create": replyStatus(http.StatusInternalServerError)}, nil, "create volume"},
		"work volume unknown":                 {map[string]http.HandlerFunc{"GET /volumes/" + vol: volume(canon), "GET /volumes/pitlane-slot-0-work": replyStatus(http.StatusNotFound)}, nil, "inspect volume pitlane-slot-0-work"},
		"volumes not on Pitlane's filesystem": {map[string]http.HandlerFunc{"GET /volumes/" + vol: volume(canon), "GET /volumes/pitlane-slot-0-work": volume(filepath.Join(root, "nowhere"))}, errNoHostAccess, ""},
	} {
		t.Run(name, func(t *testing.T) {
			p := newTestPool("owner")
			newDockerAPI(t, p, tc.routes)
			_, err := dockerSlotOps{p}.mergeStore(context.Background(), 0, vol)
			switch {
			case err == nil:
				t.Fatal("must fail")
			case tc.want != nil && !errors.Is(err, tc.want):
				t.Fatalf("err %v, want %v", err, tc.want)
			case !strings.Contains(err.Error(), tc.text):
				t.Fatalf("err %v, want %q", err, tc.text)
			}
		})
	}
}

// A state file that cannot be rewritten and cannot be removed is reported:
// the operator has to delete it before restarting.
func TestSaveLockedCannotRemoveStaleFile(t *testing.T) {
	p := newTestPool("owner", SlotState{Index: 0, Status: statusReady, Owner: "acme"})
	var logs bytes.Buffer
	p.log = slog.New(slog.NewTextHandler(&logs, nil))
	dir := filepath.Join(t.TempDir(), "state")
	write(t, filepath.Join(dir, "child"), "x") // a non-empty directory: neither renamed over nor removed
	p.cfg.StateFile = dir
	p.mu.Lock()
	p.saveLocked()
	p.mu.Unlock()
	for _, want := range []string{"cannot persist slot ownership", "cannot remove the stale slot state file"} {
		if !strings.Contains(logs.String(), want) {
			t.Fatalf("missing %q in:\n%s", want, logs.String())
		}
	}
}

func TestSaveBinfmtCannotRemoveStaleFile(t *testing.T) {
	p := newTestPool("owner")
	var logs bytes.Buffer
	p.log = slog.New(slog.NewTextHandler(&logs, nil))
	p.cfg.StateFile = filepath.Join(t.TempDir(), "slots.json")
	write(t, filepath.Join(p.binfmtStateFile(), "child"), "x")
	p.saveBinfmt(nil)
	if !strings.Contains(logs.String(), "cannot remove the QEMU handler state") {
		t.Fatalf("logs:\n%s", logs.String())
	}
}
