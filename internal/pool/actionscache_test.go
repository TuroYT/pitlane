package pool

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"syscall"
	"testing"

	"github.com/TuroYT/pitlane/internal/cache"
)

type fakeBroker struct {
	mu      sync.Mutex
	reg     map[string]cache.Identity
	revoked []string
	limit   int64
	stored  int64
}

func newBroker() *fakeBroker { return &fakeBroker{reg: map[string]cache.Identity{}} }

func (b *fakeBroker) Register(runner string, id cache.Identity) string {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.reg[runner] = id
	return "tok-" + runner
}

func (b *fakeBroker) Revoke(runner string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	delete(b.reg, runner)
	b.revoked = append(b.revoked, runner)
}

func (b *fakeBroker) SetLimit(n int64) { b.limit = n }

func (b *fakeBroker) Dir() string { return "/cache" }

func (b *fakeBroker) Evict(n int64) int64 {
	freed := min(n, b.stored)
	b.stored -= freed
	return freed
}

// buildPitlane builds the Pitlane binary (the Node shim) for the tests,
// static like the released one.
func buildPitlane(t *testing.T) string {
	t.Helper()
	bin := filepath.Join(t.TempDir(), "pitlane")
	cmd := exec.Command("go", "build", "-o", bin, "../../cmd/pitlane")
	cmd.Env = append(os.Environ(), "CGO_ENABLED=0")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("build pitlane: %v\n%s", err, out)
	}
	return bin
}

// The externals copy puts the shim in front of every Node runtime, keeps the
// real binary next to it, and leaves anything else alone.
func TestShimScript(t *testing.T) {
	shim := buildPitlane(t)
	x := t.TempDir()
	for _, d := range []string{"node20/bin", "node24/bin", "node20_alpine/bin", "tools"} {
		if err := os.MkdirAll(filepath.Join(x, d), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	real := "#!/bin/sh\necho \"real $* ${ACTIONS_CACHE_URL:-none}\"\n"
	for _, d := range []string{"node20", "node24", "node20_alpine"} {
		if err := os.WriteFile(filepath.Join(x, d, "bin/node"), []byte(real), 0o755); err != nil { //nolint:gosec // G306: an executable
			t.Fatal(err)
		}
	}
	if err := os.Symlink("node", filepath.Join(x, "node24/bin/nodejs")); err != nil {
		t.Fatal(err)
	}
	if out, err := exec.Command("sh", "-c", shimScript(x, shim)).CombinedOutput(); err != nil {
		t.Fatalf("%v: %s", err, out)
	}
	want, _ := os.ReadFile(shim)
	var inode uint64
	for _, d := range []string{"node20", "node24", "node20_alpine"} {
		n := filepath.Join(x, d, "bin/node")
		if b, _ := os.ReadFile(n); !bytes.Equal(b, want) {
			t.Fatalf("%s: shim not installed", d)
		}
		if b, _ := os.ReadFile(n + ".pitlane"); string(b) != real {
			t.Fatalf("%s: real node not kept", d)
		}
		st, err := os.Stat(n)
		if err != nil || st.Mode().Perm() != 0o755 {
			t.Fatalf("%s: %v %v", d, st.Mode(), err)
		}
		if ino := st.Sys().(*syscall.Stat_t).Ino; inode == 0 {
			inode = ino
		} else if ino != inode {
			t.Fatalf("%s: the runtimes do not share the shim binary", d)
		}
	}
	cmd := exec.Command(filepath.Join(x, "node24/bin/nodejs"), "script.js") //nolint:gosec // G204: test
	cmd.Env = []string{"PATH=/usr/bin:/bin", "PITLANE_CACHE_URL=http://gw:8091/t/", "GITHUB_REPOSITORY=acme/api"}
	out, err := cmd.CombinedOutput()
	if err != nil || strings.TrimSpace(string(out)) != "real script.js http://gw:8091/t/acme/api/" {
		t.Fatalf("shim run: %v %q", err, out)
	}
	// Running it again changes nothing (the real binary is never lost).
	if out, err := exec.Command("sh", "-c", shimScript(x, shim)).CombinedOutput(); err != nil {
		t.Fatalf("%v: %s", err, out)
	}
	if b, _ := os.ReadFile(filepath.Join(x, "node20/bin/node.pitlane")); string(b) != real {
		t.Fatal("second run replaced the real binary")
	}
	// Nothing to shim: success.
	if out, err := exec.Command("sh", "-c", shimScript(t.TempDir(), shim)).CombinedOutput(); err != nil {
		t.Fatalf("empty externals: %v: %s", err, out)
	}
}

// A missing or broken shim binary leaves every runtime untouched.
func TestShimScriptWithoutWorkingShim(t *testing.T) {
	broken := filepath.Join(t.TempDir(), "broken")
	if err := os.WriteFile(broken, []byte("#!/bin/sh\nexit 1\n"), 0o755); err != nil { //nolint:gosec // G306: an executable
		t.Fatal(err)
	}
	for _, shim := range []string{filepath.Join(t.TempDir(), "missing"), broken} {
		x := t.TempDir()
		if err := os.MkdirAll(filepath.Join(x, "node24/bin"), 0o755); err != nil {
			t.Fatal(err)
		}
		n := filepath.Join(x, "node24/bin/node")
		if err := os.WriteFile(n, []byte("real"), 0o755); err != nil { //nolint:gosec // G306: an executable
			t.Fatal(err)
		}
		if out, err := exec.Command("sh", "-c", shimScript(x, shim)).CombinedOutput(); err != nil {
			t.Fatalf("%v: %s", err, out)
		}
		if b, _ := os.ReadFile(n); string(b) != "real" {
			t.Fatalf("%s: node replaced", shim)
		}
		if _, err := os.Lstat(n + ".pitlane"); !os.IsNotExist(err) {
			t.Fatalf("%s: %v", shim, err)
		}
	}
}

// Externals copied by an older Pitlane (no shim) are copied again.
func TestExternalsRecopiedForShim(t *testing.T) {
	ops := &fakeSlotOps{image: "sha256:v1"}
	p := newTestPool("owner", SlotState{Index: 0, Status: "busy", Owner: "acme"})
	p.sops = ops
	p.externals[0] = "sha256:v1" // older layout
	if err := p.ensureExternals(context.Background(), 0, "runner:1"); err != nil || ops.copies != 1 {
		t.Fatalf("copies=%d err=%v", ops.copies, err)
	}
}

func TestCacheEnvOnlyForNonStrictJobsWhenOn(t *testing.T) {
	p := newTestPool("owner")
	b := newBroker()
	cfg := p.Config()
	cfg.ActionsCache = true
	strict := RunnerSpec{Name: "pitlane-s", Owner: "acme", Lease: Lease{Strict: true}}
	// No server attached, strict job, or feature off: no URL, no token
	// (and Docker is never asked for the slot gateway).
	if e := p.cacheEnv(context.Background(), cfg, RunnerSpec{Name: "pitlane-a", Owner: "acme"}); e != "" {
		t.Fatalf("no server: %q", e)
	}
	p.SetCache(b, 8091)
	if b.limit != 20<<30 {
		t.Fatalf("limit %d", b.limit)
	}
	if e := p.cacheEnv(context.Background(), cfg, strict); e != "" || len(b.reg) != 0 {
		t.Fatalf("strict job got %q", e)
	}
	off := cfg
	off.ActionsCache = false
	if e := p.cacheEnv(context.Background(), off, RunnerSpec{Name: "pitlane-a", Owner: "acme"}); e != "" || len(b.reg) != 0 {
		t.Fatalf("feature off: %q", e)
	}
}

func TestReleaseRevokesCacheToken(t *testing.T) {
	b := newBroker()
	p := newTestPool("owner", SlotState{Index: 0, Status: "busy"})
	p.sops = &fakeSlotOps{}
	p.SetCache(b, 8091)
	b.Register("pitlane-a", cache.Identity{Owner: "acme"})
	p.Release(context.Background(), Lease{Slot: 0, Runner: "pitlane-a"})
	if _, ok := b.reg["pitlane-a"]; ok || len(b.revoked) != 1 {
		t.Fatalf("token not revoked: %v", b.revoked)
	}
}

// An adopted non-strict runner keeps its token (same identity, repository
// restriction included); strict and untrusted ones get none.
func TestAdoptedRunnerKeepsCacheToken(t *testing.T) {
	for _, tc := range []struct {
		name   string
		labels map[string]string
		want   *cache.Identity
	}{
		{"repository target", map[string]string{LabelStrict: "false", LabelTarget: "7", LabelRepo: "api"}, &cache.Identity{Owner: "acme", Repo: "api"}},
		{"organization target", map[string]string{LabelStrict: "false", LabelTarget: "7"}, &cache.Identity{Owner: "acme"}},
		{"strict", map[string]string{LabelStrict: "true", LabelTarget: "7"}, nil},
		{"no target", map[string]string{LabelStrict: "false"}, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			b := newBroker()
			p := newTestPool("owner", SlotState{Index: 0, Status: "ready"})
			p.SetCache(b, 8091)
			tc.labels[LabelSlot], tc.labels[LabelRunner], tc.labels[LabelOwner] = "0", "pitlane-abc", "acme"
			p.adoptContainer("cid", tc.labels, "running", 0)
			id, ok := b.reg["pitlane-abc"]
			if (tc.want == nil) == ok || (ok && id != *tc.want) {
				t.Fatalf("registered=%v %+v, want %+v", ok, id, tc.want)
			}
		})
	}
}

// Low disk: actions cache entries go after slot build caches and images,
// before the per-owner volumes.
func TestReclaimDiskEvictsActionsCache(t *testing.T) {
	p := newTestPool("owner", SlotState{Index: 0, Status: "ready", Owner: "acme"})
	b := newBroker()
	b.stored = 30 * gib
	f := newFake(p, 10*gib)
	f.freed["0/build"] = 2 * gib
	f.vols = []volInfo{{Name: "pitlane-cache-acme", Size: 50 * gib}}
	p.SetCache(&diskBroker{fakeBroker: b, f: f}, 8091)
	f.otherFS = true // the cache lives on another filesystem: nothing to gain
	p.reclaimDisk(context.Background(), 20*gib, f)
	if b.stored != 30*gib {
		t.Fatalf("evicted entries on another filesystem: stored=%d", b.stored>>30)
	}
	f = newFake(p, 10*gib)
	f.freed["0/build"] = 2 * gib
	f.vols = []volInfo{{Name: "pitlane-cache-acme", Size: 50 * gib}}
	p.SetCache(&diskBroker{fakeBroker: b, f: f}, 8091)
	p.reclaimDisk(context.Background(), 20*gib, f)
	if b.stored != 22*gib || strings.Contains(strings.Join(f.calls, ","), "vol:") {
		t.Fatalf("stored=%d calls=%v", b.stored>>30, f.calls)
	}
}

// diskBroker frees its evictions on the fake disk.
type diskBroker struct {
	*fakeBroker
	f *fakeDisk
}

func (d *diskBroker) Evict(n int64) int64 {
	freed := d.fakeBroker.Evict(n)
	d.f.free += freed
	return freed
}

func TestCachePort(t *testing.T) {
	for in, want := range map[string]int{":8091": 8091, "127.0.0.1:9000": 9000, "[::]:80": 80, "": 0, "8091": 0, ":x": 0, ":0": 0} {
		if got := CachePort(in); got != want {
			t.Errorf("%q: %d, want %d", in, got, want)
		}
	}
}

// TestShimOnPinnedRunnerImage runs the externals copy and the shim install
// on the runner image pinned in images/runner/Dockerfile, then starts every
// Node runtime it ships through the shim the way the runner does (no shell
// in between), glibc ones in the runner image, the musl one in an Alpine job
// container, and checks the environment they get. It needs Docker and
// network access: PITLANE_E2E_DOCKER=1 go test ./internal/pool -run PinnedRunner
func TestShimOnPinnedRunnerImage(t *testing.T) {
	if os.Getenv("PITLANE_E2E_DOCKER") == "" {
		t.Skip("set PITLANE_E2E_DOCKER=1 (needs Docker and network)")
	}
	df, err := os.ReadFile("../../images/runner/Dockerfile")
	if err != nil {
		t.Fatal(err)
	}
	var img string
	for line := range strings.SplitSeq(string(df), "\n") {
		if f := strings.Fields(line); len(f) == 2 && f[0] == "FROM" && strings.Contains(f[1], "actions-runner") {
			img = f[1]
		}
	}
	if img == "" {
		t.Fatal("runner base image not found in the Dockerfile")
	}
	const alpine = "alpine:3.22"
	shim := buildPitlane(t)
	docker := func(args ...string) []byte {
		t.Helper()
		out, err := exec.Command("docker", args...).Output() //nolint:gosec // G204: test
		if err != nil {
			var stderr []byte
			if ee, ok := err.(*exec.ExitError); ok {
				stderr = ee.Stderr
			}
			t.Fatalf("docker %s: %v\n%s%s", strings.Join(args, " "), err, out, stderr)
		}
		return out
	}
	// The copy as Pitlane does it for a slot (shim streamed into the copy
	// container), into a throwaway slot's externals volume.
	p, err := New(Config{StateFile: filepath.Join(t.TempDir(), "state.json")}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	p.shimExe = shim
	const slot = 9999
	vol := slotExternals(slot)
	t.Cleanup(func() { _ = exec.Command("docker", "volume", "rm", "-f", vol).Run() })
	if err := (dockerSlotOps{p}).copyExternals(context.Background(), slot, img); err != nil {
		t.Fatal(err)
	}
	out := docker("run", "--rm", "-v", vol+":/x:ro", "-v", shim+":/shim:ro", "--entrypoint", "sh", img, "-c",
		`set -e; for n in /x/node*/bin/node; do cmp -s "$n" /shim || { echo "no shim: $n"; exit 1; }; [ -x "$n.pitlane" ]; echo "${n#/x/}"; done`)
	var runtimes []string
	for line := range strings.SplitSeq(strings.TrimSpace(string(out)), "\n") {
		runtimes = append(runtimes, strings.TrimSuffix(line, "/bin/node"))
	}
	if !slices.Contains(runtimes, "node20") || !slices.Contains(runtimes, "node24") {
		t.Fatalf("runtimes %q", runtimes)
	}

	// Started like Runner.Worker starts an action: the node path, its
	// arguments, the environment; externals mounted at /__e as in job
	// containers.
	const printEnv = `console.log(JSON.stringify({v: process.version, env: process.env}))`
	node := func(image, rt string, extra ...string) (string, map[string]string) {
		t.Helper()
		args := []string{"run", "--rm", "-v", vol + ":/__e:ro", "-e", "INPUT_INCLUDE-HIDDEN-FILES=false", "-e", "INPUT_A.B=1",
			"-e", "GITHUB_REPOSITORY=acme/api", "-e", "ACTIONS_CACHE_SERVICE_V2=True", "-e", "ACTIONS_CACHE_URL=https://github/"}
		for _, e := range extra {
			args = append(args, "-e", e)
		}
		args = append(args, "--entrypoint", "/__e/"+rt+"/bin/node", image, "-e", printEnv)
		var got struct {
			V   string
			Env map[string]string
		}
		if err := json.Unmarshal(docker(args...), &got); err != nil {
			t.Fatal(err)
		}
		return got.V, got.Env
	}
	checked := 0
	for _, rt := range runtimes {
		image := img
		if strings.HasSuffix(rt, "_alpine") {
			image = alpine // musl build, for Alpine job containers
		}
		v, env := node(image, rt, "PITLANE_CACHE_URL=http://x/")
		if major := strings.TrimSuffix(strings.TrimPrefix(rt, "node"), "_alpine"); !strings.HasPrefix(v, "v"+major+".") {
			t.Fatalf("%s runs Node %s: the shim started another runtime", rt, v)
		}
		if env["INPUT_INCLUDE-HIDDEN-FILES"] != "false" || env["INPUT_A.B"] != "1" {
			t.Fatalf("%s lost inputs: %v", rt, env)
		}
		if env["ACTIONS_CACHE_URL"] != "http://x/acme/api/" {
			t.Fatalf("%s: ACTIONS_CACHE_URL=%q", rt, env["ACTIONS_CACHE_URL"])
		}
		if _, ok := env["ACTIONS_CACHE_SERVICE_V2"]; ok {
			t.Fatalf("%s: ACTIONS_CACHE_SERVICE_V2 kept", rt)
		}
		// Without Pitlane's cache (strict job, job container), nothing changes.
		_, env = node(image, rt)
		if env["INPUT_INCLUDE-HIDDEN-FILES"] != "false" || env["ACTIONS_CACHE_URL"] != "https://github/" || env["ACTIONS_CACHE_SERVICE_V2"] != "True" {
			t.Fatalf("%s changed the environment without Pitlane: %v", rt, env)
		}
		checked++
		t.Log(rt, v, "in", image, "ok")
	}

	// The real toolkit reads a hyphenated boolean input through the shim
	// (what actions/upload-artifact does with include-hidden-files).
	mod := t.TempDir()
	if err := os.Chmod(mod, 0o755); err != nil { //nolint:gosec // G302: read by the container user
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(mod, "package.json"), []byte(`{"private":true}`), 0o644); err != nil { //nolint:gosec // G306: read by the container user
		t.Fatal(err)
	}
	if out, err := exec.Command("npm", "install", "--prefix", mod, "--no-audit", "--no-fund", "@actions/core").CombinedOutput(); err != nil {
		t.Fatalf("npm install: %v\n%s", err, out)
	}
	if err := os.WriteFile(filepath.Join(mod, "input.mjs"), []byte(`import * as core from '@actions/core'
console.log('include-hidden-files=' + core.getBooleanInput('include-hidden-files'))
`), 0o644); err != nil { //nolint:gosec // G306: read by the container user
		t.Fatal(err)
	}
	for _, rt := range []string{"node20", "node24"} {
		out := docker("run", "--rm", "-v", vol+":/__e:ro", "-v", mod+":/action:ro", "-e", "INPUT_INCLUDE-HIDDEN-FILES=false",
			"-e", "PITLANE_CACHE_URL=http://x/", "-e", "GITHUB_REPOSITORY=acme/api", "--entrypoint", "/__e/"+rt+"/bin/node", img, "/action/input.mjs")
		if got := strings.TrimSpace(string(out)); got != "include-hidden-files=false" {
			t.Fatalf("%s: %q", rt, got)
		}
	}
	t.Log(checked, "Node runtimes shimmed in", img)
}
