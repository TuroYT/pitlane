package pool

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/docker/docker/client"
)

// fakeDocker serves the few Docker API calls StartRunner makes and records
// the containers removed.
type fakeDocker struct {
	mu      sync.Mutex
	removed []string
	create  func(w http.ResponseWriter, r *http.Request)
	start   func(w http.ResponseWriter, r *http.Request)
	remove  int // status returned by DELETE (0: 204)
}

func (f *fakeDocker) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	path := r.URL.Path
	if i := strings.Index(path[1:], "/"); strings.HasPrefix(path, "/v") && i > 0 {
		path = path[i+1:] // drop the API version
	}
	w.Header().Set("Content-Type", "application/json")
	switch {
	case r.Method == http.MethodPost && path == "/containers/create":
		if f.create != nil {
			f.create(w, r)
			return
		}
		_, _ = io.WriteString(w, `{"Id":"c1"}`)
	case r.Method == http.MethodPost && strings.HasSuffix(path, "/start"):
		f.start(w, r)
	case r.Method == http.MethodDelete && strings.HasPrefix(path, "/containers/"):
		f.mu.Lock()
		f.removed = append(f.removed, strings.TrimPrefix(path, "/containers/"))
		f.mu.Unlock()
		if f.remove != 0 {
			w.WriteHeader(f.remove)
			_, _ = io.WriteString(w, `{"message":"boom"}`)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	default:
		http.Error(w, `{"message":"unexpected `+r.Method+" "+path+`"}`, http.StatusNotImplemented)
	}
}

func (f *fakeDocker) removedIDs() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.removed...)
}

func dockerPool(t *testing.T, f *fakeDocker) (*Pool, *bytes.Buffer) {
	t.Helper()
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)
	cli, err := client.NewClientWithOpts(client.WithHost("tcp://"+strings.TrimPrefix(srv.URL, "http://")), client.WithVersion("1.47"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cli.Close() })
	p := newTestPool("owner", SlotState{Index: 0, Status: "busy"})
	var logs bytes.Buffer
	p.cli, p.log = cli, slog.New(slog.NewTextHandler(&logs, nil))
	for _, v := range []string{"pitlane-cache-acme", "pitlane-toolcache-acme"} {
		p.chowned[v] = true // no volume preparation
	}
	return p, &logs
}

// hang cancels the caller's context and holds the request until the client
// gives up on it (the body is read first: the server notices a closed
// connection only after that).
func hang(r *http.Request, cancel context.CancelFunc) {
	_, _ = io.Copy(io.Discard, r.Body)
	cancel()
	select {
	case <-r.Context().Done():
	case <-time.After(5 * time.Second):
	}
}

var testSpec = RunnerSpec{Name: "pitlane-test", JITConfig: "jit", TargetID: 1, Owner: "acme", Lease: Lease{Slot: 0, Owner: "acme", TargetID: 1}}

// A target stopped while its runner starts cancels the start: the created
// container must still be removed, not left with its JIT config.
func TestStartRunnerCanceledRemovesContainer(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	f := &fakeDocker{start: func(w http.ResponseWriter, r *http.Request) {
		hang(r, cancel)
	}}
	p, _ := dockerPool(t, f)
	if _, err := p.StartRunner(ctx, testSpec); err == nil {
		t.Fatal("start must fail")
	}
	if got := f.removedIDs(); len(got) != 1 || got[0] != "c1" {
		t.Fatalf("removed %v, want [c1]", got)
	}
}

// A create canceled half-way may still have created the container on the
// daemon: it is removed by name.
func TestStartRunnerCanceledCreateRemovesByName(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	f := &fakeDocker{create: func(w http.ResponseWriter, r *http.Request) {
		hang(r, cancel)
	}}
	p, _ := dockerPool(t, f)
	if _, err := p.StartRunner(ctx, testSpec); err == nil {
		t.Fatal("start must fail")
	}
	if got := f.removedIDs(); len(got) != 1 || got[0] != testSpec.Name {
		t.Fatalf("removed %v, want [%s]", got, testSpec.Name)
	}
}

func TestStartRunnerStartErrorRemovesContainer(t *testing.T) {
	f := &fakeDocker{start: func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, `{"message":"no such image"}`, http.StatusInternalServerError)
	}}
	p, _ := dockerPool(t, f)
	if _, err := p.StartRunner(context.Background(), testSpec); err == nil || !strings.Contains(err.Error(), "start runner") {
		t.Fatalf("err = %v", err)
	}
	if got := f.removedIDs(); len(got) != 1 || got[0] != "c1" {
		t.Fatalf("removed %v, want [c1]", got)
	}
}

func TestRemoveLogsFailuresOnly(t *testing.T) {
	for _, c := range []struct {
		status int
		logged bool
	}{{0, false}, {http.StatusNotFound, false}, {http.StatusConflict, false}, {http.StatusInternalServerError, true}} {
		f := &fakeDocker{remove: c.status}
		p, logs := dockerPool(t, f)
		p.Remove(context.Background(), "c1")
		if got := strings.Contains(logs.String(), "remove container"); got != c.logged {
			t.Errorf("status %d: logged=%v, want %v (%s)", c.status, got, c.logged, logs.String())
		}
	}
}

func TestRunnerContainerSpec(t *testing.T) {
	cfg := Config{RunnerImage: "runner:test", ImageOS: "ubuntu24", CacheScope: "owner", CPUShares: 512, MemoryMB: 1024}
	spec := RunnerSpec{Name: "pitlane-r1", JITConfig: "jit", TargetID: 7, Owner: "Acme", Repo: "acme/app", Lease: Lease{Slot: 2}}
	cacheVol, toolVol := runnerVolumes(cfg.CacheScope, spec)
	if cacheVol != "pitlane-cache-acme" || toolVol != "pitlane-toolcache-acme" {
		t.Fatalf("volumes: %s %s", cacheVol, toolVol)
	}
	env := runnerEnv(cfg, spec)
	want := []string{
		"ACTIONS_RUNNER_INPUT_JITCONFIG=jit", "DOCKER_HOST=tcp://127.0.0.1:2375",
		"RUNNER_TOOL_CACHE=/opt/hostedtoolcache", "AGENT_TOOLSDIRECTORY=/opt/hostedtoolcache", "ImageOS=ubuntu24",
		"XDG_CACHE_HOME=/home/runner/.cache", "npm_config_cache=/home/runner/.cache/npm",
		"npm_config_store_dir=/home/runner/_work/.pnpm-store", "YARN_CACHE_FOLDER=/home/runner/.cache/yarn",
		"BUN_INSTALL_CACHE_DIR=/home/runner/.cache/bun", "GOMODCACHE=/home/runner/.cache/go-mod",
		"PIP_CACHE_DIR=/home/runner/.cache/pip", "PITLANE_SLOT=2", "ACTIONS_RUNNER_PRINT_LOG_TO_STDOUT=0",
	}
	if strings.Join(env, "\n") != strings.Join(want, "\n") {
		t.Fatalf("env:\n%s", strings.Join(env, "\n"))
	}
	cc, hc := runnerContainer(cfg, spec, env, cacheVol, toolVol)
	if cc.Labels[LabelSlot] != "2" || cc.Labels[LabelStrict] != "false" || cc.Labels[LabelRepo] != "acme/app" || cc.Labels[LabelOwner] != "Acme" {
		t.Fatalf("labels: %v", cc.Labels)
	}
	if string(hc.NetworkMode) != "container:pitlane-slot-2" || hc.Memory != 1024<<20 || hc.CPUShares != 512 || len(hc.Mounts) != 4 {
		t.Fatalf("host config: %+v", hc)
	}
	spec.Lease.Strict = true
	if c, tl := runnerVolumes(cfg.CacheScope, spec); c != "pitlane-strict-pitlane-r1-cache" || tl != "pitlane-strict-pitlane-r1-tools" {
		t.Fatalf("strict volumes: %s %s", c, tl)
	}
}

func TestParsePages(t *testing.T) {
	got := parsePages("1.0 10 a.log\n2.0 5 b'c.log\n3.0 7 d.txt\nbad\n4.0 3 e.log\n")
	if len(got) != 2 || got[0] != (page{"a.log", 10}) || got[1] != (page{"e.log", 3}) {
		t.Fatalf("pages: %+v", got)
	}
	if parsePages("") != nil {
		t.Fatal("empty listing")
	}
}
