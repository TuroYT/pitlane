package scaler

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"slices"
	"testing"
	"time"

	"github.com/TuroYT/pitlane/internal/gh"
	"github.com/TuroYT/pitlane/internal/pool"
	"github.com/TuroYT/pitlane/internal/store"
)

func TestStartAdoptsRunners(t *testing.T) {
	st, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	tg, err := st.CreateTarget(store.Target{Kind: "org", Owner: "acme", Enabled: true, MaxRunners: 4})
	if err != nil {
		t.Fatal(err)
	}
	created := time.Now().Add(-10 * time.Minute)
	p := newFakePool(0)
	p.orphans = []pool.Orphan{
		// The pool reports a container without the strict label as strict.
		{ID: "c-strict", Name: "pitlane-strict", Slot: 0, TargetID: tg.ID, Owner: "acme", Strict: true, Running: true, Created: created},
		{ID: "c-loose", Name: "pitlane-loose", Slot: 1, TargetID: tg.ID, Owner: "acme", Running: true, Created: created},
		{ID: "c-dead", Name: "pitlane-dead", Slot: 2, TargetID: tg.ID, Owner: "acme", Strict: true},
	}
	_ = st.AddRunner(store.RegisteredRunner{ID: 11, TargetID: tg.ID, Name: "pitlane-strict"})
	_ = st.AddRunner(store.RegisteredRunner{ID: 12, TargetID: tg.ID, Name: "pitlane-gone"})
	// A job left running on the strict runner, one on a runner gone meanwhile.
	_ = st.UpsertJob(store.Job{TargetID: tg.ID, JobID: "running", RunnerName: "pitlane-strict", Slot: 0, StartedAt: created.Unix()})
	_ = st.UpsertJob(store.Job{TargetID: tg.ID, JobID: "orphaned", RunnerName: "pitlane-gone", Slot: -1, StartedAt: created.Unix()})
	api := newFakeAPI()
	m := New(st, p, t.TempDir(), func() Credentials { return Credentials{PAT: "x"} }, func() {}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	m.connect = func(Credentials, store.Target, string) (scaleSetAPI, error) { return api, nil }

	// Before Start, Sync starts nothing (slots are still starting).
	m.Sync()
	if len(m.Snapshot()[0].Runners) != 0 {
		t.Fatal("Sync before Start must not start a listener")
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := m.Start(ctx); err != nil {
		t.Fatal(err)
	}
	defer m.Shutdown()
	<-api.session.running

	// The exited container goes straight through finishRunner, as strict.
	waitFor(t, "dead runner released", func() bool { return len(p.snapshot().released) == 1 })
	if l := p.snapshot().released[0]; !l.Strict || l.Owner != pool.StrictOwner || l.Slot != 2 || l.Runner != "pitlane-dead" {
		t.Fatalf("dead runner released as %+v", l)
	}

	m.mu.Lock()
	target := m.targets[tg.ID]
	adoptedLeft := len(m.adopted)
	m.mu.Unlock()
	if target == nil || adoptedLeft != 0 {
		t.Fatalf("adopted runners not handed to their target (left %d)", adoptedLeft)
	}
	target.mu.Lock()
	strict, loose := target.runners["pitlane-strict"], target.runners["pitlane-loose"]
	_, lost := target.lost["orphaned"]
	target.mu.Unlock()
	if strict == nil || loose == nil {
		t.Fatal("running containers not adopted")
	}
	if !strict.lease.Strict || strict.lease.Owner != pool.StrictOwner || strict.runnerID != 11 || strict.jobID != "running" {
		t.Fatalf("strict runner adopted as %+v", strict)
	}
	if loose.lease.Strict || loose.lease.Owner != "acme" {
		t.Fatalf("non-strict runner adopted as %+v", loose)
	}
	if !strict.busy || !strict.busySince.Equal(created) {
		t.Fatal("adopted runners count as busy since their creation (the time limit still applies)")
	}
	if !lost {
		t.Fatal("job of a runner gone while Pitlane was stopped not recorded as lost")
	}

	// The adopted runners' containers are watched: an exit releases the slot
	// the way the runner was started.
	p.exit("c-strict")
	waitFor(t, "strict runner released", func() bool { return len(p.snapshot().released) == 2 })
	if l := p.snapshot().released[1]; !l.Strict || l.Slot != 0 {
		t.Fatalf("adopted strict runner released as %+v", l)
	}
	target.mu.Lock()
	_, lostRunning := target.lost["running"]
	target.mu.Unlock()
	if !lostRunning {
		t.Fatal("adopted runner exited before its job completed: job not recorded as lost")
	}
}

func TestCleanupOrphans(t *testing.T) {
	old := time.Now().Add(-time.Hour).Unix()
	recs := []store.RegisteredRunner{
		{ID: 1, Name: "offline-idle", CreatedAt: old},
		{ID: 2, Name: "online", CreatedAt: old},
		{ID: 3, Name: "busy", CreatedAt: old},
		{ID: 4, Name: "young", CreatedAt: time.Now().Unix()},
		{ID: 5, Name: "has-container", CreatedAt: old},
		{ID: 6, Name: "unlisted-gone", CreatedAt: old},
		{ID: 7, Name: "unlisted-unknown", CreatedAt: old},
		{ID: 8, Name: "tracked", CreatedAt: old},
	}
	remote := []gh.Runner{
		{ID: 1, Name: "offline-idle", Status: "offline"},
		{ID: 2, Name: "online", Status: "online"},
		{ID: 3, Name: "busy", Status: "offline", Busy: true},
		{ID: 4, Name: "young", Status: "offline"},
		{ID: 5, Name: "has-container", Status: "offline"},
		{ID: 8, Name: "tracked", Status: "offline"},
	}
	setup := func(t *testing.T) *harness {
		h := newHarness(t, store.Target{Kind: "org", Owner: "acme", Enabled: true, MaxRunners: 4}, 0)
		for _, r := range recs {
			r.TargetID = h.t.cfg().ID
			if err := h.st.AddRunner(r); err != nil {
				t.Fatal(err)
			}
		}
		h.p.names["has-container"] = true
		h.addRunner("tracked", 8, time.Hour, false, false)
		h.api.getErr[6] = notFound()
		h.api.getErr[7] = errors.New("502")
		h.t.lost["j-idle"] = lostJob{runner: "offline-idle", at: time.Now()}
		h.m.ListRunners = func(context.Context, store.Target) ([]gh.Runner, error) { return remote, nil }
		return h
	}
	left := func(h *harness) []string {
		rs, _ := h.st.RegisteredRunners(h.t.cfg().ID)
		var out []string
		for _, r := range rs {
			out = append(out, r.Name)
		}
		slices.Sort(out)
		return out
	}
	all := []string{"busy", "has-container", "offline-idle", "online", "tracked", "unlisted-gone", "unlisted-unknown", "young"}

	t.Run("removes only offline idle old runners without container", func(t *testing.T) {
		h := setup(t)
		h.t.cleanupOrphans()
		if got := h.api.removedIDs(); !slices.Equal(got, []int64{1}) {
			t.Fatalf("deregistered %v", got)
		}
		want := []string{"busy", "has-container", "online", "tracked", "unlisted-unknown", "young"}
		if got := left(h); !slices.Equal(got, want) {
			t.Fatalf("records left %v, want %v", got, want)
		}
		if _, ok := h.t.lost["j-idle"]; ok {
			t.Fatal("the removed runner's lost job must be forgotten")
		}
		// Rate limited: a listener restarting in a loop does not hammer GitHub.
		h.t.cleanupOrphans()
		if got := h.api.removedIDs(); len(got) != 1 {
			t.Fatalf("cleanup ran again within a minute: %v", got)
		}
	})
	t.Run("a refused removal keeps the record", func(t *testing.T) {
		h := setup(t)
		h.api.removeErr[1] = errors.New("500")
		h.t.cleanupOrphans()
		if !slices.Contains(left(h), "offline-idle") {
			t.Fatal("record dropped although the removal failed")
		}
	})
	for name, breakIt := range map[string]func(h *harness){
		"GitHub runner list fails": func(h *harness) {
			h.m.ListRunners = func(context.Context, store.Target) ([]gh.Runner, error) { return nil, errors.New("rate limited") }
		},
		"container list fails": func(h *harness) { h.p.namesErr = errors.New("docker down") },
		"no runner list":       func(h *harness) { h.m.ListRunners = nil },
	} {
		t.Run(name+" keeps everything", func(t *testing.T) {
			h := setup(t)
			breakIt(h)
			h.t.cleanupOrphans()
			if got := h.api.removedIDs(); len(got) != 0 {
				t.Fatalf("deregistered %v", got)
			}
			if got := left(h); !slices.Equal(got, all) {
				t.Fatalf("records left %v", got)
			}
			if len(h.api.gets) != 0 {
				t.Fatal("no runner may be looked up when the lists are unavailable")
			}
		})
	}
}

// A runner container left by a previous run that no listener will adopt
// (target label missing or invalid, target deleted or disabled) is stopped
// gracefully and finished at once, its slot released as strict: it is never
// left running untracked with its slot held.
func TestStartStopsUntrackedRunners(t *testing.T) {
	st, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	on, err := st.CreateTarget(store.Target{Kind: "org", Owner: "acme", Enabled: true, MaxRunners: 4})
	if err != nil {
		t.Fatal(err)
	}
	off, err := st.CreateTarget(store.Target{Kind: "org", Owner: "other", Enabled: false, MaxRunners: 4})
	if err != nil {
		t.Fatal(err)
	}
	created := time.Now().Add(-10 * time.Minute)
	p := newFakePool(0)
	p.orphans = []pool.Orphan{
		{ID: "c-ok", Name: "pitlane-ok", Slot: 0, TargetID: on.ID, Owner: "acme", Running: true, Created: created},
		// What the pool reports for a container without a valid target label.
		{ID: "c-nolabel", Name: "pitlane-nolabel", Slot: 1, Strict: true, Untrusted: true, Running: true, Created: created},
		{ID: "c-deleted", Name: "pitlane-deleted", Slot: 2, TargetID: 999, Owner: "gone", Running: true, Created: created},
		{ID: "c-disabled", Name: "pitlane-disabled", Slot: 3, TargetID: off.ID, Owner: "other", Running: true, Created: created},
		{ID: "c-exited", Name: "pitlane-exited", Slot: 4, TargetID: 999, Owner: "gone", Created: created},
		{ID: "c-noslot", Name: "pitlane-noslot", Slot: -1, Strict: true, Untrusted: true, Running: true, Created: created},
	}
	api := newFakeAPI()
	m := New(st, p, t.TempDir(), func() Credentials { return Credentials{PAT: "x"} }, func() {}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	m.connect = func(Credentials, store.Target, string) (scaleSetAPI, error) { return api, nil }
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := m.Start(ctx); err != nil {
		t.Fatal(err)
	}
	defer m.Shutdown()
	<-api.session.running

	waitFor(t, "untracked runners finished", func() bool {
		s := p.snapshot()
		return len(s.released) == 4 && len(s.removed) == 5
	})
	s := p.snapshot()
	slices.Sort(s.stopped)
	if want := []string{"c-deleted", "c-disabled", "c-nolabel", "c-noslot"}; !slices.Equal(s.stopped, want) {
		t.Fatalf("stopped %v, want %v (running untracked containers only)", s.stopped, want)
	}
	slots := map[int]bool{}
	for _, l := range s.released {
		if !l.Strict || l.Owner != pool.StrictOwner || l.TargetID != 0 {
			t.Fatalf("untracked runner released as %+v, want strict", l)
		}
		slots[l.Slot] = true
	}
	if !slots[1] || !slots[2] || !slots[3] || !slots[4] || slots[0] {
		t.Fatalf("released slots %v", slots)
	}
	if slices.Contains(s.removed, "c-ok") {
		t.Fatal("runner of an enabled target must be adopted, not removed")
	}
	m.mu.Lock()
	target, adoptedLeft := m.targets[on.ID], len(m.adopted)
	m.mu.Unlock()
	if target == nil || adoptedLeft != 0 {
		t.Fatalf("untracked runners must not be kept for adoption (left %d)", adoptedLeft)
	}
	target.mu.Lock()
	_, ok := target.runners["pitlane-ok"]
	n := len(target.runners)
	target.mu.Unlock()
	if !ok || n != 1 {
		t.Fatalf("enabled target adopted %d runners", n)
	}
}

// A failed graceful stop still removes the container (forcibly) and
// releases its slot.
func TestStartUntrackedRunnerStopFails(t *testing.T) {
	st, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	p := newFakePool(0)
	p.stopErr = errors.New("daemon busy")
	p.orphans = []pool.Orphan{{ID: "c-x", Name: "pitlane-x", Slot: 0, Strict: true, Untrusted: true, Running: true}}
	m := New(st, p, t.TempDir(), func() Credentials { return Credentials{} }, func() {}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := m.Start(ctx); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "untracked runner removed", func() bool { s := p.snapshot(); return len(s.released) == 1 && len(s.removed) == 1 })
	if l := p.snapshot().released[0]; !l.Strict || l.Slot != 0 {
		t.Fatalf("released as %+v", l)
	}
}

// TestStartHandsOverBeforeWatching covers a runner adopted at startup whose
// container exits at once: its watch finishes it while the target is being
// filled, so the target must be complete before any watch starts (the race
// detector catches the opposite).
func TestStartHandsOverBeforeWatching(t *testing.T) {
	st, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	tg, err := st.CreateTarget(store.Target{Kind: "org", Owner: "acme", Enabled: true, MaxRunners: 4})
	if err != nil {
		t.Fatal(err)
	}
	created := time.Now().Add(-10 * time.Minute)
	p := newFakePool(0)
	p.orphans = []pool.Orphan{
		{ID: "c-gone", Name: "pitlane-gone", Slot: 0, TargetID: tg.ID, Owner: "acme", Running: true, Created: created},
		{ID: "c-live", Name: "pitlane-live", Slot: 1, TargetID: tg.ID, Owner: "acme", Running: true, Created: created},
	}
	p.exit("c-gone") // its Wait returns at once
	// A job on the runner that exits, one on a runner gone before startup
	// (in-flight jobs only count on runners still registered).
	_ = st.AddRunner(store.RegisteredRunner{ID: 21, TargetID: tg.ID, Name: "pitlane-gone"})
	_ = st.AddRunner(store.RegisteredRunner{ID: 22, TargetID: tg.ID, Name: "pitlane-vanished"})
	_ = st.UpsertJob(store.Job{TargetID: tg.ID, JobID: "unfinished", RunnerName: "pitlane-gone", Slot: 0, StartedAt: created.Unix()})
	_ = st.UpsertJob(store.Job{TargetID: tg.ID, JobID: "stale", RunnerName: "pitlane-vanished", Slot: -1, StartedAt: created.Unix()})
	api := newFakeAPI()
	m := New(st, p, t.TempDir(), func() Credentials { return Credentials{PAT: "x"} }, func() {}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	m.connect = func(Credentials, store.Target, string) (scaleSetAPI, error) { return api, nil }

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := m.Start(ctx); err != nil {
		t.Fatal(err)
	}
	defer m.Shutdown()

	waitFor(t, "exited runner released", func() bool { return len(p.snapshot().released) == 1 })
	if l := p.snapshot().released[0]; l.Slot != 0 || l.Runner != "pitlane-gone" {
		t.Fatalf("released %+v, want the exited runner's lease", l)
	}
	m.mu.Lock()
	target := m.targets[tg.ID]
	m.mu.Unlock()
	if target == nil {
		t.Fatal("target not started")
	}
	waitFor(t, "exited runner forgotten", func() bool { return target.runnerCount() == 1 })
	target.mu.Lock()
	defer target.mu.Unlock()
	if target.runners["pitlane-live"] == nil {
		t.Fatal("live adopted runner not kept")
	}
	for _, job := range []string{"unfinished", "stale"} {
		if _, ok := target.lost[job]; !ok {
			t.Errorf("job %q not kept as lost: %+v", job, target.lost)
		}
	}
}
