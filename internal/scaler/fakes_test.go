package scaler

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/actions/scaleset"
	"github.com/actions/scaleset/listener"

	"github.com/TuroYT/pitlane/internal/pool"
	"github.com/TuroYT/pitlane/internal/store"
)

// fakePool is an in-memory Pool. Every runner container "runs" until exit
// is called with its ID.
type fakePool struct {
	mu        sync.Mutex
	free      int // slots Acquire may still hand out
	acquired  []pool.Lease
	prepared  []pool.Lease
	released  []pool.Lease
	removed   []string
	stopped   []string
	started   []pool.RunnerSpec
	orphans   []pool.Orphan
	names     map[string]bool
	namesErr  error
	stopErr   error
	prepErr   error
	startErr  error
	logs      string
	exits     map[string]chan struct{}
	signal    chan struct{}
	nextID    int
	freeDisks int
}

func newFakePool(free int) *fakePool {
	return &fakePool{free: free, exits: map[string]chan struct{}{}, signal: make(chan struct{}, 1), names: map[string]bool{}}
}

func (f *fakePool) RunnerContainers(context.Context) ([]pool.Orphan, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.orphans, nil
}

func (f *fakePool) RunnerNames(context.Context) (map[string]bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := map[string]bool{}
	for k, v := range f.names {
		out[k] = v
	}
	return out, f.namesErr
}

func (f *fakePool) Released() <-chan struct{} { return f.signal }

func (f *fakePool) Acquire(_ context.Context, owner string, targetID int64, strict bool) (pool.Lease, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.free <= 0 {
		return pool.Lease{}, false
	}
	f.free--
	if strict {
		owner = pool.StrictOwner
	}
	l := pool.Lease{Slot: len(f.acquired), Owner: owner, TargetID: targetID, Strict: strict}
	f.acquired = append(f.acquired, l)
	return l, true
}

func (f *fakePool) Prepare(_ context.Context, l pool.Lease) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.prepared = append(f.prepared, l)
	return f.prepErr
}

func (f *fakePool) Release(_ context.Context, l pool.Lease) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.released = append(f.released, l)
	f.free++
}

func (f *fakePool) StartRunner(_ context.Context, spec pool.RunnerSpec) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.startErr != nil {
		return "", f.startErr
	}
	f.nextID++
	id := fmt.Sprintf("c%d", f.nextID)
	f.started = append(f.started, spec)
	f.exits[id] = make(chan struct{})
	return id, nil
}

// container returns the exit channel of a container, creating it (adopted
// containers are not started through StartRunner).
func (f *fakePool) container(id string) chan struct{} {
	f.mu.Lock()
	defer f.mu.Unlock()
	ch, ok := f.exits[id]
	if !ok {
		ch = make(chan struct{})
		f.exits[id] = ch
	}
	return ch
}

func (f *fakePool) exit(id string) { close(f.container(id)) }

func (f *fakePool) Wait(ctx context.Context, id string) int64 {
	select {
	case <-f.container(id):
	case <-ctx.Done():
	}
	return 0
}

func (f *fakePool) Logs(_ context.Context, _ string, _ bool, w io.Writer) error {
	f.mu.Lock()
	logs := f.logs
	f.mu.Unlock()
	_, err := io.WriteString(w, logs)
	return err
}

func (f *fakePool) Remove(_ context.Context, id string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.removed = append(f.removed, id)
}

func (f *fakePool) StopRunner(_ context.Context, id string, _ time.Duration) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.stopped = append(f.stopped, id)
	return f.stopErr
}

func (f *fakePool) EnsureFreeDisk(context.Context) {
	f.mu.Lock()
	f.freeDisks++
	f.mu.Unlock()
}

func (f *fakePool) snapshot() fakePool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return fakePool{free: f.free, acquired: append([]pool.Lease(nil), f.acquired...), prepared: append([]pool.Lease(nil), f.prepared...),
		released: append([]pool.Lease(nil), f.released...), removed: append([]string(nil), f.removed...),
		stopped: append([]string(nil), f.stopped...), started: append([]pool.RunnerSpec(nil), f.started...)}
}

// fakeAPI is an in-memory scaleSetAPI.
type fakeAPI struct {
	mu         sync.Mutex
	nextRunner int64
	removed    []int64
	removeErr  map[int64]error
	getErr     map[int64]error
	gets       []int64
	deleted    []int
	session    *fakeSession
}

func newFakeAPI() *fakeAPI {
	return &fakeAPI{nextRunner: 100, removeErr: map[int64]error{}, getErr: map[int64]error{}, session: newFakeSession()}
}

func (f *fakeAPI) ensureScaleSet(context.Context, string) (int, bool, error) { return 42, false, nil }

func (f *fakeAPI) openSession(context.Context, int, string) (jobSession, error) {
	return f.session, nil
}

func (f *fakeAPI) generateJIT(_ context.Context, name string, _ int) (jitConfig, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.nextRunner++
	return jitConfig{encoded: "jit-" + name, runnerID: f.nextRunner}, nil
}

func (f *fakeAPI) removeRunner(_ context.Context, id int64) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.removeErr[id]; err != nil {
		return err
	}
	f.removed = append(f.removed, id)
	return nil
}

func (f *fakeAPI) getRunner(_ context.Context, id int64) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.gets = append(f.gets, id)
	return f.getErr[id]
}

func (f *fakeAPI) deleteScaleSet(_ context.Context, id int) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.deleted = append(f.deleted, id)
	return nil
}

func (f *fakeAPI) removedIDs() []int64 {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]int64(nil), f.removed...)
}

// fakeSession blocks in run until its context ends.
type fakeSession struct {
	mu       sync.Mutex
	acquired []int64
	max      int
	running  chan struct{}
	once     sync.Once
}

func newFakeSession() *fakeSession { return &fakeSession{running: make(chan struct{})} }

func (f *fakeSession) acquireJobs(_ context.Context, ids []int64) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.acquired = append(f.acquired, ids...)
	return nil
}

func (f *fakeSession) run(ctx context.Context, _, maxRunners int, _ *slog.Logger, _ listener.Scaler) error {
	f.setMaxRunners(maxRunners)
	f.once.Do(func() { close(f.running) })
	<-ctx.Done()
	return ctx.Err()
}

func (f *fakeSession) setMaxRunners(n int) {
	f.mu.Lock()
	f.max = n
	f.mu.Unlock()
}

func (f *fakeSession) close(context.Context) error { return nil }

// harness is a manager wired to fakes, with one target already listening.
type harness struct {
	m   *Manager
	t   *target
	p   *fakePool
	api *fakeAPI
	st  *store.Store
}

func newHarness(t *testing.T, cfg store.Target, free int) *harness {
	t.Helper()
	st, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	if cfg.ID == 0 {
		if cfg.Kind == "" {
			cfg.Kind, cfg.Owner = "org", "acme"
		}
		created, err := st.CreateTarget(cfg)
		if err != nil {
			t.Fatal(err)
		}
		cfg = created
	}
	p := newFakePool(free)
	api := newFakeAPI()
	m := New(st, p, t.TempDir(), func() Credentials { return Credentials{PAT: "x"} }, func() {},
		slog.New(slog.NewTextHandler(io.Discard, nil)))
	m.connect = func(Credentials, store.Target, string) (scaleSetAPI, error) { return api, nil }
	tg := newTarget(m, cfg)
	tg.client, tg.session, tg.scaleSetID = api, api.session, 42
	t.Cleanup(tg.cancel)
	m.mu.Lock()
	m.targets[cfg.ID] = tg
	m.mu.Unlock()
	return &harness{m: m, t: tg, p: p, api: api, st: st}
}

// addRunner puts a live runner into the target, its container waiting to exit.
func (h *harness) addRunner(name string, runnerID int64, age time.Duration, busy, strict bool) *runner {
	r := &runner{name: name, containerID: "ctr-" + name, runnerID: runnerID, busy: busy,
		startedAt: time.Now().Add(-age), lease: pool.Lease{Slot: 100 + len(h.t.runners), Owner: "acme", Strict: strict, Runner: name}}
	h.t.mu.Lock()
	h.t.runners[name] = r
	h.t.mu.Unlock()
	return r
}

// waitFor polls cond until it holds or the test times out.
func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func (h *harness) runnerCount() int {
	h.t.mu.Lock()
	defer h.t.mu.Unlock()
	return len(h.t.runners)
}

func (h *harness) startingCount() int {
	h.t.mu.Lock()
	defer h.t.mu.Unlock()
	return h.t.starting
}

func notFound() error { return fmt.Errorf("wrapped: %w", scaleset.RunnerNotFoundError) }
