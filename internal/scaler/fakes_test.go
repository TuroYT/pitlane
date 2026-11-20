package scaler

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"strings"
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
	listErr   error // RunnerContainers fails
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
	// memory admission: budget 0 = off; reserved by leased slot
	budget       int64
	reserved     map[int]int64
	requests     []pool.Request
	strictDemand bool
	onRemove     func(id string) // called by Remove, outside mu
}

func newFakePool(free int) *fakePool {
	return &fakePool{free: free, exits: map[string]chan struct{}{}, signal: make(chan struct{}, 1), names: map[string]bool{}}
}

func (f *fakePool) RunnerContainers(context.Context) ([]pool.Orphan, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.orphans, f.listErr
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

func (f *fakePool) MeasureJob(context.Context, string, int) *pool.Meter { return nil }

func (f *fakePool) Admit(_ context.Context, r pool.Request) (pool.Lease, string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.requests = append(f.requests, r)
	if f.free <= 0 {
		return pool.Lease{}, pool.WaitSlot
	}
	if f.budget > 0 {
		used := int64(0)
		for _, v := range f.reserved {
			used += v
		}
		if len(f.reserved) > 0 && used+r.Predicted > f.budget {
			return pool.Lease{}, pool.WaitMemory
		}
	}
	f.free--
	owner := r.Owner
	if r.Strict {
		owner = pool.StrictOwner
	}
	l := pool.Lease{Slot: len(f.acquired), Owner: owner, TargetID: r.TargetID, Strict: r.Strict, Predicted: r.Predicted}
	f.acquired = append(f.acquired, l)
	if f.reserved == nil {
		f.reserved = map[int]int64{}
	}
	f.reserved[l.Slot] = r.Predicted
	return l, ""
}

func (f *fakePool) SetPrediction(slot int, bytes int64) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.reserved == nil {
		f.reserved = map[int]int64{}
	}
	f.reserved[slot] = bytes
}

func (f *fakePool) SetStrictDemand(on bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.strictDemand = on
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
	delete(f.reserved, l.Slot)
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
	f.removed = append(f.removed, id)
	hook := f.onRemove
	f.mu.Unlock()
	if hook != nil {
		hook(id)
	}
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
	// ensureID is the scale set ensureScaleSet returns (42 when 0); created
	// is what it reports. The errors make the matching call fail.
	ensureID  int
	created   bool
	ensureErr error
	openErr   error
	jitErr    error
	deleteErr error
}

func newFakeAPI() *fakeAPI {
	return &fakeAPI{nextRunner: 100, removeErr: map[int64]error{}, getErr: map[int64]error{}, session: newFakeSession()}
}

func (f *fakeAPI) ensureScaleSet(context.Context, string) (int, bool, error) {
	if f.ensureErr != nil {
		return 0, false, f.ensureErr
	}
	if f.ensureID != 0 {
		return f.ensureID, f.created, nil
	}
	return 42, f.created, nil
}

func (f *fakeAPI) openSession(context.Context, int, string) (jobSession, error) {
	if f.openErr != nil {
		return nil, f.openErr
	}
	return f.session, nil
}

func (f *fakeAPI) generateJIT(_ context.Context, name string, _ int) (jitConfig, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.jitErr != nil {
		return jitConfig{}, f.jitErr
	}
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
	return f.deleteErr
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
	// runFn, when set, replaces the wait for the context in run.
	runFn      func(ctx context.Context) error
	acquireErr error
	closeErr   error
	closed     int
}

func newFakeSession() *fakeSession { return &fakeSession{running: make(chan struct{})} }

func (f *fakeSession) acquireJobs(_ context.Context, ids []int64) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.acquired = append(f.acquired, ids...)
	return f.acquireErr
}

func (f *fakeSession) run(ctx context.Context, _, maxRunners int, _ *slog.Logger, _ listener.Scaler) error {
	f.setMaxRunners(maxRunners)
	f.once.Do(func() { close(f.running) })
	if f.runFn != nil {
		return f.runFn(ctx)
	}
	<-ctx.Done()
	return ctx.Err()
}

func (f *fakeSession) setMaxRunners(n int) {
	f.mu.Lock()
	f.max = n
	f.mu.Unlock()
}

func (f *fakeSession) close(context.Context) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.closed++
	return f.closeErr
}

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
	// Generous: CI runs on a shared, sometimes saturated host; the condition
	// normally holds within milliseconds.
	deadline := time.Now().Add(30 * time.Second)
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

// recordingLog is a slog handler that keeps what was logged.
type recordingLog struct {
	mu   sync.Mutex
	logs []string
}

func (r *recordingLog) Enabled(context.Context, slog.Level) bool { return true }
func (r *recordingLog) WithAttrs([]slog.Attr) slog.Handler       { return r }
func (r *recordingLog) WithGroup(string) slog.Handler            { return r }
func (r *recordingLog) Handle(_ context.Context, rec slog.Record) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.logs = append(r.logs, rec.Level.String()+" "+rec.Message)
	return nil
}

// has reports whether a message containing s was logged.
func (r *recordingLog) has(s string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, l := range r.logs {
		if strings.Contains(l, s) {
			return true
		}
	}
	return false
}

// fakeClock replaces the manager's time sources: after answers at once and
// remembers the delays, now only moves when the test advances it.
type fakeClock struct {
	mu     sync.Mutex
	at     time.Time
	delays []time.Duration
	// onAfter, when set, runs for each delay (outside mu) with its number
	// (from 1) and may move the clock.
	onAfter func(n int, d time.Duration)
}

func newFakeClock() *fakeClock { return &fakeClock{at: time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)} }

func (c *fakeClock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.at
}

func (c *fakeClock) advance(d time.Duration) {
	c.mu.Lock()
	c.at = c.at.Add(d)
	c.mu.Unlock()
}

func (c *fakeClock) after(d time.Duration) <-chan time.Time {
	c.mu.Lock()
	c.delays = append(c.delays, d)
	n, hook := len(c.delays), c.onAfter
	c.mu.Unlock()
	if hook != nil {
		hook(n, d)
	}
	ch := make(chan time.Time, 1)
	ch <- c.now()
	return ch
}

func (c *fakeClock) waited() []time.Duration {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]time.Duration(nil), c.delays...)
}

// install makes the manager use the fake time sources.
func (c *fakeClock) install(m *Manager) { m.now, m.after = c.now, c.after }

// addTarget adds a second live target to the harness's manager.
func (h *harness) addTarget(t *testing.T, cfg store.Target) *target {
	t.Helper()
	created, err := h.st.CreateTarget(cfg)
	if err != nil {
		t.Fatal(err)
	}
	tg := newTarget(h.m, created)
	tg.client, tg.session, tg.scaleSetID = h.api, h.api.session, 42
	t.Cleanup(tg.cancel)
	h.m.mu.Lock()
	h.m.targets[created.ID] = tg
	h.m.mu.Unlock()
	return tg
}

// startedManager marks the manager as started, the way Start does, without
// the background loops.
func (h *harness) startedManager(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	h.m.mu.Lock()
	h.m.ctx = ctx
	h.m.mu.Unlock()
	return ctx
}

// fakeTickers hands the manager loop channels the test fires by hand. A
// send returns once the loop received it, so sending twice on the same
// channel also tells the first tick's work is done.
type fakeTickers struct {
	ch map[time.Duration]chan time.Time
}

func newFakeTickers() *fakeTickers {
	f := &fakeTickers{ch: map[time.Duration]chan time.Time{}}
	for _, d := range []time.Duration{reconcileInterval, housekeepingInterval, timeoutCheckInterval, orphanInterval} {
		f.ch[d] = make(chan time.Time)
	}
	return f
}

func (f *fakeTickers) tick(d time.Duration) (<-chan time.Time, func()) { return f.ch[d], func() {} }

func (f *fakeTickers) fire(d time.Duration) { f.ch[d] <- time.Time{} }

// newBareManager is a manager over a fresh store, without any target.
func newBareManager(t *testing.T, p Pool) (*Manager, *store.Store) {
	t.Helper()
	st, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	m := New(st, p, t.TempDir(), func() Credentials { return Credentials{PAT: "x"} }, func() {},
		slog.New(slog.NewTextHandler(io.Discard, nil)))
	return m, st
}
