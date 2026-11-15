package cache

import (
	"bytes"
	"crypto/rand"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// installShim builds Pitlane and installs it as dir/node, the Node shim
// (internal/nodeshim), in front of real (dir/node.pitlane), as the externals
// copy does.
func installShim(t *testing.T, real string) string {
	t.Helper()
	dir := t.TempDir()
	cmd := exec.Command("go", "build", "-o", filepath.Join(dir, "node"), "../../cmd/pitlane")
	cmd.Env = append(os.Environ(), "CGO_ENABLED=0")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("build pitlane: %v\n%s", err, out)
	}
	if err := os.Symlink(real, filepath.Join(dir, "node.pitlane")); err != nil {
		t.Fatal(err)
	}
	return filepath.Join(dir, "node")
}

// runnerEnv is the environment Runner.Worker gives a JavaScript action on
// github.com, plus what Pitlane adds to the runner container.
func runnerEnv(extra ...string) []string {
	env := []string{"PATH=" + os.Getenv("PATH"), "HOME=" + os.Getenv("HOME"),
		"ACTIONS_CACHE_SERVICE_V2=True", "ACTIONS_RESULTS_URL=http://127.0.0.1:9/", "ACTIONS_CACHE_URL=http://127.0.0.1:9/",
		"GITHUB_REPOSITORY=acme/api"}
	return append(env, extra...)
}

// TestToolkitClient drives the real @actions/cache client (as bundled in
// actions/cache and the setup-* actions) through the Node shim against the
// server. A hyphenated input goes through as well (a shell shim loses it).
// It needs node, npm and network access: PITLANE_E2E=1 go test ./internal/cache -run Toolkit
func TestToolkitClient(t *testing.T) {
	if os.Getenv("PITLANE_E2E") == "" {
		t.Skip("set PITLANE_E2E=1 (needs node, npm and network)")
	}
	nodeBin, err := exec.LookPath("node")
	if err != nil {
		t.Fatal("node not found")
	}
	mod := t.TempDir()
	if err := os.WriteFile(filepath.Join(mod, "package.json"), []byte(`{"type":"module","private":true}`), 0o600); err != nil {
		t.Fatal(err)
	}
	version := os.Getenv("PITLANE_E2E_TOOLKIT")
	if version == "" {
		version = "6.3.0"
	}
	if out, err := exec.Command("npm", "install", "--prefix", mod, "--no-audit", "--no-fund", "@actions/cache@"+version).CombinedOutput(); err != nil { //nolint:gosec // G204: test
		t.Fatalf("npm install: %v\n%s", err, out)
	}
	script := filepath.Join(mod, "run.mjs")
	if err := os.WriteFile(script, []byte(`import * as cache from '@actions/cache'
const [mode, dir, key, ...restore] = process.argv.slice(2)
if (mode === 'save') console.log('result=' + await cache.saveCache([dir], key))
else console.log('result=' + (await cache.restoreCache([dir], key, restore) ?? ''))
if (process.env['INPUT_INCLUDE-HIDDEN-FILES'] !== 'false') throw new Error('hyphenated input lost')
`), 0o600); err != nil {
		t.Fatal(err)
	}

	s, _ := newServer(t, "")
	ts := httptest.NewServer(s.Handler())
	defer ts.Close()
	tok := s.Register("pitlane-e2e", Identity{Owner: "acme"})
	node := installShim(t, nodeBin)
	work := t.TempDir()
	run := func(args ...string) string {
		cmd := exec.Command(node, append([]string{script}, args...)...) //nolint:gosec // G204: test
		cmd.Dir = work
		cmd.Env = runnerEnv("PITLANE_CACHE_URL="+ts.URL+"/"+tok+"/", "RUNNER_TEMP="+t.TempDir(), "GITHUB_WORKSPACE="+work,
			"ACTIONS_RUNTIME_TOKEN="+jobToken("refs/heads/main"), "RUNNER_DEBUG=1", "INPUT_INCLUDE-HIDDEN-FILES=false")
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("%v\n%s", err, out)
		}
		if bytes.Contains(out, []byte("::warning::")) || bytes.Contains(out, []byte("::error::")) {
			t.Fatalf("client warning:\n%s", out)
		}
		for line := range strings.SplitSeq(string(out), "\n") {
			if r, ok := strings.CutPrefix(line, "result="); ok {
				return r
			}
		}
		t.Fatalf("no result:\n%s", out)
		return ""
	}

	// 40 MB of random data: more than one 32 MB upload chunk.
	data := make([]byte, 40<<20)
	_, _ = rand.Read(data)
	dir := filepath.Join(work, "deps")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "blob"), data, 0o600); err != nil {
		t.Fatal(err)
	}
	if id := run("save", "deps", "deps-linux-1"); id == "" || id == "-1" {
		t.Fatalf("save returned %q", id)
	}
	if err := os.RemoveAll(dir); err != nil {
		t.Fatal(err)
	}
	if k := run("restore", "deps", "deps-linux-2", "deps-linux-"); k != "deps-linux-1" {
		t.Fatalf("restore returned %q", k)
	}
	got, err := os.ReadFile(filepath.Join(dir, "blob"))
	if err != nil || !bytes.Equal(got, data) {
		t.Fatalf("restored content differs (%v)", err)
	}
	if k := run("restore", "deps", "other-key"); k != "" {
		t.Fatalf("unexpected hit %q", k)
	}
	if st := s.Stats(); st.Entries != 1 || st.Hits != 1 || st.Misses != 1 || st.Saves != 1 {
		t.Fatalf("stats: %+v", st)
	}
}
