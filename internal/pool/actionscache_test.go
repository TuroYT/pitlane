package pool

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
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

func (b *fakeBroker) Evict(n int64) int64 {
	freed := min(n, b.stored)
	b.stored -= freed
	return freed
}

// The externals copy puts the shim in front of every Node runtime, keeps the
// real binary next to it, and leaves anything else alone.
func TestShimScript(t *testing.T) {
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
	if out, err := exec.Command("sh", "-c", shimScript(x)).CombinedOutput(); err != nil {
		t.Fatalf("%v: %s", err, out)
	}
	for _, d := range []string{"node20", "node24", "node20_alpine"} {
		b, _ := os.ReadFile(filepath.Join(x, d, "bin/node"))
		if string(b) != cache.NodeShim {
			t.Fatalf("%s: shim not installed:\n%s", d, b)
		}
		if b, _ := os.ReadFile(filepath.Join(x, d, "bin/node.pitlane")); string(b) != real {
			t.Fatalf("%s: real node not kept", d)
		}
	}
	cmd := exec.Command(filepath.Join(x, "node24/bin/nodejs"), "script.js") //nolint:gosec // G204: test
	cmd.Env = []string{"PATH=/usr/bin:/bin", "PITLANE_CACHE_URL=http://gw:8091/t/", "GITHUB_REPOSITORY=acme/api"}
	out, err := cmd.CombinedOutput()
	if err != nil || strings.TrimSpace(string(out)) != "real script.js http://gw:8091/t/acme/api/" {
		t.Fatalf("shim run: %v %q", err, out)
	}
	// Running it again changes nothing (the real binary is never lost).
	if out, err := exec.Command("sh", "-c", shimScript(x)).CombinedOutput(); err != nil {
		t.Fatalf("%v: %s", err, out)
	}
	if b, _ := os.ReadFile(filepath.Join(x, "node20/bin/node.pitlane")); string(b) != real {
		t.Fatal("second run replaced the real binary")
	}
	// Nothing to shim: success.
	if out, err := exec.Command("sh", "-c", shimScript(t.TempDir())).CombinedOutput(); err != nil {
		t.Fatalf("empty externals: %v: %s", err, out)
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

// TestShimOnPinnedRunnerImage runs the externals copy and the shim on the
// runner image pinned in images/runner/Dockerfile and checks every Node
// runtime it ships. It needs Docker and network access:
// PITLANE_E2E_DOCKER=1 go test ./internal/pool -run PinnedRunner
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
	script := `set -e
mkdir /x && cp -a ` + externalsDir + `/. /x/
` + shimScript("/x") + `
n=0
for node in /x/node*/bin/node; do
  head -c 9 "$node" | grep -q '^#!/bin/sh' || { echo "no shim: $node"; exit 1; }
  [ -x "$node.pitlane" ] || { echo "no real binary: $node"; exit 1; }
  case "$node" in *_alpine/*) continue ;; esac # musl build, for alpine job containers
  "$node" --version >/dev/null
  out=$(env PITLANE_CACHE_URL=http://gw:8091/t/ GITHUB_REPOSITORY=acme/api ACTIONS_CACHE_SERVICE_V2=True ACTIONS_CACHE_URL=https://github/ \
    "$node" -e 'console.log(process.env.ACTIONS_CACHE_URL + " " + (process.env.ACTIONS_CACHE_SERVICE_V2 || "unset"))')
  [ "$out" = "http://gw:8091/t/acme/api/ unset" ] || { echo "$node: $out"; exit 1; }
  out=$(env ACTIONS_CACHE_SERVICE_V2=True "$node" -e 'console.log(process.env.ACTIONS_CACHE_SERVICE_V2)')
  [ "$out" = "True" ] || { echo "$node changed the environment without Pitlane: $out"; exit 1; }
  n=$((n+1))
done
[ "$n" -gt 0 ] && echo "ok $n"`
	out, err := exec.Command("docker", "run", "--rm", "--user", "root", "--entrypoint", "sh", img, "-c", script).CombinedOutput() //nolint:gosec // G204: test
	if err != nil || !strings.HasPrefix(strings.TrimSpace(string(out)), "ok") {
		t.Fatalf("%v:\n%s", err, out)
	}
	t.Log(strings.TrimSpace(string(out)), "Node runtimes shimmed in", img)
}
