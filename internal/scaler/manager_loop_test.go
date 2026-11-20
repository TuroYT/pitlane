package scaler

import (
	"context"
	"errors"
	"log/slog"
	"slices"
	"testing"
	"time"

	"github.com/TuroYT/pitlane/internal/gh"
	"github.com/TuroYT/pitlane/internal/store"
)

func TestStartFailures(t *testing.T) {
	t.Run("containers cannot be listed", func(t *testing.T) {
		p := newFakePool(0)
		p.listErr = errors.New("docker down")
		m, _ := newBareManager(t, p)
		err := m.Start(context.Background())
		if err == nil || !errors.Is(err, p.listErr) {
			t.Fatalf("Start returned %v", err)
		}
		if m.ctx != nil {
			t.Fatal("a failed Start must leave the manager stopped")
		}
	})
	t.Run("targets cannot be loaded", func(t *testing.T) {
		m, st := newBareManager(t, newFakePool(0))
		_ = st.Close()
		if err := m.Start(context.Background()); err == nil {
			t.Fatal("Start must fail when the store is unusable")
		}
		if m.ctx != nil {
			t.Fatal("a failed Start must leave the manager stopped")
		}
	})
}

// The loop reconciles on the pool's release signal and every tick, enforces
// the job time limit, prunes old data and checks strict mode and orphans
// on their own ticks, until its context ends.
func TestLoopRunsPeriodicWork(t *testing.T) {
	h := newHarness(t, store.Target{Kind: "org", Owner: "acme", Enabled: true, MaxRunners: 4, MaxJobMinutes: 1}, 4)
	tk := newFakeTickers()
	h.m.tick = tk.tick
	if err := h.st.Set("history_days", "1"); err != nil {
		t.Fatal(err)
	}
	if err := h.st.UpsertJob(store.Job{TargetID: h.t.cfg().ID, JobID: "old", Slot: -1, QueuedAt: time.Now().Add(-72 * time.Hour).Unix()}); err != nil {
		t.Fatal(err)
	}
	if err := h.st.AddRunner(store.RegisteredRunner{ID: 5, TargetID: h.t.cfg().ID, Name: "pitlane-old", CreatedAt: 1}); err != nil {
		t.Fatal(err)
	}
	strictAsked := make(chan struct{}, 4)
	h.m.StrictReason = func(context.Context, store.Target) (string, error) { strictAsked <- struct{}{}; return "", nil }
	orphansListed := make(chan struct{}, 4)
	h.m.ListRunners = func(context.Context, store.Target) ([]gh.Runner, error) {
		orphansListed <- struct{}{}
		return nil, nil
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() {
		h.m.loop(ctx)
		close(done)
	}()

	// A reconcile tick starts the runner the demand asks for.
	h.setDemand(1)
	tk.fire(reconcileInterval)
	waitFor(t, "runner started by the tick", func() bool { return h.startingCount() == 0 && h.runnerCount() == 1 })

	// So does the pool's signal that a slot was released.
	h.setDemand(2)
	h.p.signal <- struct{}{}
	waitFor(t, "runner started by the release signal", func() bool { return h.startingCount() == 0 && h.runnerCount() == 2 })

	// A job over its limit is stopped by the timeout tick.
	late := h.addRunner("late", 0, time.Hour, true, false)
	late.busySince = time.Now().Add(-2 * time.Minute)
	tk.fire(timeoutCheckInterval)
	waitFor(t, "overdue runner stopped", func() bool { return slices.Contains(h.stopped(), "ctr-late") })

	// The hourly tick prunes the history and re-checks strict mode. The
	// second tick is only taken once the first one's pruning is done.
	tk.fire(housekeepingInterval)
	tk.fire(housekeepingInterval)
	<-strictAsked
	if jobs, _ := h.st.Jobs(context.Background(), 10); len(jobs) != 0 {
		t.Fatalf("old job kept: %+v", jobs)
	}

	// The orphan tick lists GitHub's runners to compare with the records.
	tk.fire(orphanInterval)
	<-orphansListed

	cancel()
	<-done
}

// The target waiting longest is served first, one that waits for nothing last.
func TestReconcileAllServesLongestWaitingFirst(t *testing.T) {
	h := newHarness(t, store.Target{Kind: "org", Owner: "a", Enabled: true, MaxRunners: 4}, 0)
	b := h.addTarget(t, store.Target{Kind: "org", Owner: "b", Enabled: true, MaxRunners: 4})
	c := h.addTarget(t, store.Target{Kind: "org", Owner: "c", Enabled: true, MaxRunners: 4})
	now := time.Now()
	for tg, waited := range map[*target]time.Duration{h.t: time.Minute, b: 5 * time.Minute, c: 0} {
		tg.mu.Lock()
		tg.desired = 1
		if waited > 0 {
			tg.waiting, tg.waitSince = 1, now.Add(-waited)
		}
		tg.mu.Unlock()
	}
	h.m.reconcileAll()
	h.p.mu.Lock()
	defer h.p.mu.Unlock()
	var order []int64
	for _, r := range h.p.requests {
		order = append(order, r.TargetID)
	}
	if want := []int64{b.cfg().ID, h.t.cfg().ID, c.cfg().ID}; !slices.Equal(order, want) {
		t.Fatalf("admission order %v, want %v (longest waiting first)", order, want)
	}
}

func TestRunnerContainer(t *testing.T) {
	h := newHarness(t, store.Target{Kind: "org", Owner: "acme", Enabled: true, MaxRunners: 4}, 0)
	h.addRunner("pitlane-a", 1, time.Minute, true, false)
	if id, ok := h.m.RunnerContainer("pitlane-a"); !ok || id != "ctr-pitlane-a" {
		t.Fatalf("got %q %v", id, ok)
	}
	if id, ok := h.m.RunnerContainer("pitlane-gone"); ok || id != "" {
		t.Fatalf("unknown runner: %q %v", id, ok)
	}
}

// Shutdown stops the listeners only: runners keep their containers, to be
// adopted by the next process.
func TestShutdownKeepsRunners(t *testing.T) {
	h := newHarness(t, store.Target{Kind: "org", Owner: "acme", Enabled: true, MaxRunners: 4}, 0)
	h.addRunner("idle", 1, time.Hour, false, false)
	h.addRunner("busy", 2, time.Hour, true, false)
	h.m.Shutdown()
	if h.t.ctx.Err() == nil {
		t.Fatal("listener not stopped")
	}
	if got := h.p.snapshot().removed; len(got) != 0 {
		t.Fatalf("Shutdown removed containers %v", got)
	}
	if h.runnerCount() != 2 {
		t.Fatal("runners must stay tracked")
	}
}

func TestSyncUpdatesRunningTarget(t *testing.T) {
	h := newHarness(t, store.Target{Kind: "org", Owner: "acme", Enabled: true, MaxRunners: 2}, 0)
	h.startedManager(t)
	cur, _ := h.st.Target(h.t.cfg().ID)
	cur.MaxRunners, cur.Strict = 5, true
	if err := h.st.UpdateTarget(cur); err != nil {
		t.Fatal(err)
	}
	h.m.Sync()
	h.m.mu.Lock()
	same := h.m.targets[cur.ID] == h.t
	h.m.mu.Unlock()
	if !same {
		t.Fatal("a capacity change must not restart the listener")
	}
	if got := h.t.cfg(); got.MaxRunners != 5 || !got.Strict {
		t.Fatalf("target config %+v", got)
	}
	h.api.session.mu.Lock()
	max := h.api.session.max
	h.api.session.mu.Unlock()
	if max != 5 {
		t.Fatalf("listener told max %d, want 5", max)
	}
	h.p.mu.Lock()
	defer h.p.mu.Unlock()
	if !h.p.strictDemand {
		t.Fatal("the pool must know a strict target is enabled")
	}
}

// A disabled target loses its idle runners at once, and its scale set is
// deleted when its busy runners are done.
func TestSyncStopsDisabledTargetAndDeletesItsScaleSet(t *testing.T) {
	h := newHarness(t, store.Target{Kind: "org", Owner: "acme", Enabled: true, MaxRunners: 2}, 0)
	h.startedManager(t)
	clk := newFakeClock()
	clk.install(h.m)
	h.addRunner("idle", 1, time.Hour, false, false)
	h.addRunner("busy", 2, time.Hour, true, false)
	h.p.onRemove = func(string) { // the removed container's watch forgets its runner
		h.t.mu.Lock()
		delete(h.t.runners, "idle")
		h.t.mu.Unlock()
	}
	clk.onAfter = func(n int, _ time.Duration) {
		if n == 2 { // its job is done by the second poll
			h.t.mu.Lock()
			delete(h.t.runners, "busy")
			h.t.mu.Unlock()
		}
	}
	cur, _ := h.st.Target(h.t.cfg().ID)
	cur.Enabled = false
	if err := h.st.UpdateTarget(cur); err != nil {
		t.Fatal(err)
	}
	h.m.Sync()
	h.m.mu.Lock()
	n := len(h.m.targets)
	h.m.mu.Unlock()
	if n != 0 {
		t.Fatalf("%d listeners left", n)
	}
	waitFor(t, "scale set deleted", func() bool {
		h.api.mu.Lock()
		defer h.api.mu.Unlock()
		return len(h.api.deleted) == 1
	})
	if h.api.deleted[0] != 42 {
		t.Fatalf("deleted scale set %v", h.api.deleted)
	}
	if got := h.p.snapshot().removed; !slices.Equal(got, []string{"ctr-idle"}) {
		t.Fatalf("removed %v, want only the idle runner", got)
	}
	if got := clk.waited(); !slices.Equal(got, []time.Duration{drainPoll, drainPoll}) {
		t.Fatalf("polled the drain with %v", got)
	}
}

// A renamed scale set needs a new listener, which inherits the jobs the old
// one knew were lost.
func TestSyncMovesTargetToNewScaleSet(t *testing.T) {
	h := newHarness(t, store.Target{Kind: "org", Owner: "acme", Enabled: true, MaxRunners: 2}, 0)
	h.startedManager(t)
	old := h.t
	old.mu.Lock()
	old.lost["j1"] = lostJob{runner: "pitlane-dead", at: time.Now()}
	old.mu.Unlock()
	cur, _ := h.st.Target(old.cfg().ID)
	cur.ScaleSetName = "renamed"
	if err := h.st.UpdateTarget(cur); err != nil {
		t.Fatal(err)
	}
	h.m.Sync()
	<-old.ctx.Done()
	<-h.api.session.running

	h.m.mu.Lock()
	fresh := h.m.targets[cur.ID]
	kept := len(h.m.lost)
	h.m.mu.Unlock()
	if fresh == nil || fresh == old {
		t.Fatal("no new listener for the renamed scale set")
	}
	if kept != 0 {
		t.Fatal("the lost jobs must be handed to the new listener")
	}
	fresh.mu.Lock()
	_, ok := fresh.lost["j1"]
	fresh.mu.Unlock()
	if !ok {
		t.Fatal("lost job not carried over to the new listener")
	}
}

func TestSyncWithoutCredentialsStartsNothing(t *testing.T) {
	h := newHarness(t, store.Target{Kind: "org", Owner: "acme", Enabled: true, MaxRunners: 2}, 0)
	h.startedManager(t)
	h.m.creds = func() Credentials { return Credentials{} }
	extra, err := h.st.CreateTarget(store.Target{Kind: "org", Owner: "other", Enabled: true, MaxRunners: 2})
	if err != nil {
		t.Fatal(err)
	}
	h.m.Sync()
	h.m.mu.Lock()
	_, started := h.m.targets[extra.ID]
	h.m.mu.Unlock()
	if started {
		t.Fatal("a listener was started without credentials")
	}
	for _, ts := range h.m.Snapshot() {
		if ts.ID == extra.ID && ts.Status != "no-credentials" {
			t.Fatalf("status %q, want no-credentials", ts.Status)
		}
	}
}

func TestSyncKeepsListenersWhenStoreFails(t *testing.T) {
	h := newHarness(t, store.Target{Kind: "org", Owner: "acme", Enabled: true, MaxRunners: 2}, 0)
	h.startedManager(t)
	rec := &recordingLog{}
	h.m.log = slog.New(rec)
	_ = h.st.Close()
	h.m.Sync()
	h.m.mu.Lock()
	n := len(h.m.targets)
	h.m.mu.Unlock()
	if n != 1 || !rec.has("load targets") {
		t.Fatalf("%d listeners, logs %v", n, rec.logs)
	}
}

func TestRestartReplacesListenersAndKeepsLostJobs(t *testing.T) {
	h := newHarness(t, store.Target{Kind: "org", Owner: "acme", Enabled: true, MaxRunners: 2}, 0)
	h.startedManager(t)
	old := h.t
	old.mu.Lock()
	old.lost["j1"] = lostJob{runner: "pitlane-dead", at: time.Now()}
	old.mu.Unlock()
	h.m.Restart()
	<-old.ctx.Done()
	<-h.api.session.running
	h.m.mu.Lock()
	fresh := h.m.targets[old.cfg().ID]
	h.m.mu.Unlock()
	if fresh == nil || fresh == old {
		t.Fatal("listener not restarted")
	}
	fresh.mu.Lock()
	_, ok := fresh.lost["j1"]
	fresh.mu.Unlock()
	if !ok {
		t.Fatal("lost job dropped by the restart")
	}
}

// A store failure shows no targets, but is logged: an empty dashboard must
// not look like a configuration with no target.
func TestSnapshotLogsStoreFailure(t *testing.T) {
	h := newHarness(t, store.Target{Kind: "org", Owner: "acme", Enabled: true, MaxRunners: 2}, 0)
	rec := &recordingLog{}
	h.m.log = slog.New(rec)
	_ = h.st.Close()
	got := h.m.Snapshot()
	if got == nil || len(got) != 0 {
		t.Fatalf("snapshot %v, want an empty list", got)
	}
	if !rec.has("ERROR load targets") {
		t.Fatalf("failure not logged: %v", rec.logs)
	}
}
