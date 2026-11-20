package scaler

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/actions/scaleset"

	"github.com/TuroYT/pitlane/internal/pool"
	"github.com/TuroYT/pitlane/internal/store"
)

// --- reconcile ---

func (h *harness) requestedMemory() []int64 {
	h.p.mu.Lock()
	defer h.p.mu.Unlock()
	var out []int64
	for _, r := range h.p.requests {
		out = append(out, r.Predicted)
	}
	return out
}

func TestReconcileDoesNothingOnceCanceled(t *testing.T) {
	h := newHarness(t, store.Target{Kind: "org", Owner: "acme", Enabled: true, MaxRunners: 4}, 4)
	h.setDemand(2)
	h.t.cancel()
	h.t.reconcile()
	if n := len(h.requestedMemory()); n != 0 {
		t.Fatalf("%d slots requested by a stopped listener", n)
	}
}

func TestReconcileIgnoresExitedRunners(t *testing.T) {
	h := newHarness(t, store.Target{Kind: "org", Owner: "acme", Enabled: true, MaxRunners: 4}, 4)
	h.addRunner("gone", 1, time.Hour, false, false).exited = true
	h.setDemand(1)
	h.t.reconcile()
	waitFor(t, "replacement started", func() bool { return h.startingCount() == 0 && len(h.p.snapshot().started) == 1 })
	if got := h.api.removedIDs(); len(got) != 0 {
		t.Fatalf("an exited runner is not scaled down again: %v", got)
	}
}

// Pending jobs reserve their memory heaviest first, whichever order the
// runners are started in.
func TestReconcileReservesHeaviestPendingJobsFirst(t *testing.T) {
	h := newHarness(t, store.Target{Kind: "org", Owner: "acme", Enabled: true, MaxRunners: 4}, 4)
	now := time.Now()
	h.t.mu.Lock()
	h.t.pending["j1"] = pendingJob{predicted: 100, at: now}
	h.t.pending["j2"] = pendingJob{predicted: 300, at: now}
	h.t.pending["stale"] = pendingJob{predicted: 900, at: now.Add(-pendingTTL - time.Minute)}
	h.t.desired = 2
	h.t.mu.Unlock()
	h.t.reconcile()
	waitFor(t, "runners started", func() bool { return h.startingCount() == 0 && h.runnerCount() == 2 })
	if got := h.requestedMemory(); !slices.Equal(got, []int64{300, 100}) {
		t.Fatalf("reserved %v, want the two live pending jobs, heaviest first", got)
	}
	h.t.mu.Lock()
	defer h.t.mu.Unlock()
	if _, ok := h.t.pending["stale"]; ok {
		t.Fatal("a pending job never seen again must be forgotten")
	}
}

func TestPendingPredictions(t *testing.T) {
	now := time.Now()
	pending := map[string]pendingJob{
		"a":     {predicted: 5, at: now},
		"b":     {predicted: 50, at: now.Add(-time.Hour)},
		"c":     {predicted: 20, at: now.Add(-pendingTTL / 2)},
		"stale": {predicted: 1000, at: now.Add(-pendingTTL - time.Second)},
	}
	if got := pendingPredictions(pending, now); !slices.Equal(got, []int64{50, 20, 5}) {
		t.Fatalf("got %v", got)
	}
	if len(pending) != 3 {
		t.Fatalf("expired entry kept: %v", pending)
	}
}

// Scale down removes only as many idle runners as there are in excess, and
// a runner GitHub refuses to deregister does not count: the next idle one
// goes instead.
func TestScaleDownRemovesExactlyTheExcess(t *testing.T) {
	h := newHarness(t, store.Target{Kind: "org", Owner: "acme", Enabled: true, MaxRunners: 4}, 0)
	h.addRunner("a", 1, time.Hour, false, false)
	h.addRunner("b", 2, time.Hour, false, false)
	h.addRunner("c", 3, time.Hour, false, false)
	h.api.removeErr[2] = errors.New("job just assigned") // the demand is one runner: two are in excess
	h.setDemand(1)
	h.t.reconcile()
	removed := h.p.snapshot().removed
	slices.Sort(removed)
	if !slices.Equal(removed, []string{"ctr-a", "ctr-c"}) {
		t.Fatalf("removed %v, want both runners GitHub let go, and never the refused one", removed)
	}
}

func TestScaleDownKeepsWhatTheDemandNeeds(t *testing.T) {
	h := newHarness(t, store.Target{Kind: "org", Owner: "acme", Enabled: true, MinRunners: 1, MaxRunners: 4}, 0)
	h.addRunner("a", 1, time.Hour, false, false)
	h.addRunner("b", 2, time.Hour, false, false)
	h.addRunner("busy", 3, time.Hour, true, false)
	h.setDemand(1) // 1 warm + 1 job: the busy runner and one idle one
	h.t.reconcile()
	if got := h.p.snapshot().removed; len(got) != 1 || got[0] == "ctr-busy" {
		t.Fatalf("removed %v, want exactly one idle runner", got)
	}
}

// A runner without a GitHub registration is removed without asking GitHub.
func TestRetireRunnerWithoutRegistration(t *testing.T) {
	h := newHarness(t, store.Target{Kind: "org", Owner: "acme", Enabled: true, MaxRunners: 4}, 0)
	r := h.addRunner("anon", 0, time.Hour, false, false)
	if !h.t.retire(h.api, r, "test") {
		t.Fatal("retire refused")
	}
	if got := h.p.snapshot().removed; !slices.Equal(got, []string{"ctr-anon"}) || len(h.api.removedIDs()) != 0 {
		t.Fatalf("removed %v, deregistered %v", got, h.api.removedIDs())
	}
}

func TestRecordWait(t *testing.T) {
	h := newHarness(t, store.Target{Kind: "org", Owner: "acme", Enabled: true, MaxRunners: 4}, 0)
	notified := 0
	h.m.changed = func() { notified++ }
	state := func() (int, string, time.Time) {
		h.t.mu.Lock()
		defer h.t.mu.Unlock()
		return h.t.waiting, h.t.waitReason, h.t.waitSince
	}

	// Missing runners without a reason are held back by the target's own cap
	// or by runners still starting: shown as waiting for a slot.
	h.t.recordWait(2, 0, "")
	if n, why, since := state(); n != 2 || why != pool.WaitSlot || since.IsZero() {
		t.Fatalf("waiting %d for %q since %v", n, why, since)
	}
	if notified != 1 {
		t.Fatalf("UI notified %d times", notified)
	}
	h.t.recordWait(2, 0, "") // unchanged: no notification
	if notified != 1 {
		t.Fatalf("an unchanged wait notified the UI (%d)", notified)
	}

	// A target that got a runner and still waits goes to the back of the queue.
	old := time.Now().Add(-time.Hour)
	h.t.mu.Lock()
	h.t.waitSince = old
	h.t.mu.Unlock()
	h.t.recordWait(2, 0, pool.WaitMemory)
	if _, why, since := state(); why != pool.WaitMemory || !since.Equal(old) {
		t.Fatalf("a target that got nothing keeps its turn: %q %v", why, since)
	}
	h.t.recordWait(1, 1, pool.WaitMemory)
	if _, _, since := state(); !since.After(old) {
		t.Fatal("a target that started a runner must give up its turn")
	}

	h.t.recordWait(0, 1, pool.WaitMemory)
	if n, why, _ := state(); n != 0 || why != "" {
		t.Fatalf("nothing missing: waiting %d for %q", n, why)
	}
}

// --- launching runners ---

func TestLaunchGitHubRefusesRegistration(t *testing.T) {
	h := newHarness(t, store.Target{Kind: "org", Owner: "acme", Enabled: true, MaxRunners: 4}, 4)
	h.api.jitErr = errors.New("403 forbidden")
	h.setDemand(1)
	h.t.reconcile()
	waitFor(t, "launch", func() bool { return h.startingCount() == 0 && len(h.p.snapshot().released) == 1 })
	if s, e := h.status(); s != "error" || !strings.Contains(e, "generate JIT config") || !strings.Contains(e, "403") {
		t.Fatalf("status %q %q", s, e)
	}
	if n := len(h.p.snapshot().started); n != 0 || h.runnerCount() != 0 {
		t.Fatalf("a container was started without a registration (%d)", n)
	}
	// The next reconcile asks again.
	h.api.jitErr = nil
	h.t.reconcile()
	waitFor(t, "retry", func() bool { return h.startingCount() == 0 && h.runnerCount() == 1 })
}

// A slot the pool is rebuilding is the pool's: releasing it would put a
// dirty slot back in service.
func TestLaunchKeepsSlotBeingRebuilt(t *testing.T) {
	h := newHarness(t, store.Target{Kind: "org", Owner: "acme", Enabled: true, MaxRunners: 4}, 4)
	h.p.prepErr = fmt.Errorf("wipe: %w", pool.ErrSlotRebuilding)
	h.t.mu.Lock()
	h.t.starting = 1
	h.t.mu.Unlock()
	h.t.launch(pool.Lease{Slot: 3, Owner: "acme"})
	if got := h.p.snapshot(); len(got.released) != 0 || len(got.started) != 0 {
		t.Fatalf("released %v started %d", got.released, len(got.started))
	}
	if h.startingCount() != 0 {
		t.Fatal("the launch is over")
	}
}

// A target switched to strict while its slot was being prepared must not run
// a runner in a slot prepared for the shared mode.
func TestLaunchDropsSlotPreparedBeforeStrict(t *testing.T) {
	h := newHarness(t, store.Target{Kind: "org", Owner: "acme", Enabled: true, MaxRunners: 4}, 4)
	next := h.t.cfg()
	next.Strict = true
	h.t.update(next)
	h.t.mu.Lock()
	h.t.starting = 1
	h.t.mu.Unlock()
	lease := pool.Lease{Slot: 2, Owner: "acme"}
	h.t.launch(lease)
	got := h.p.snapshot()
	if len(got.released) != 1 || got.released[0] != lease || len(got.started) != 0 {
		t.Fatalf("released %+v, started %d", got.released, len(got.started))
	}
}

// A registration that cannot be recorded is only logged: the runner works,
// but the orphan cleanup will not see it.
func TestStartRunnerWithoutRegistrationRecord(t *testing.T) {
	h := newHarness(t, store.Target{Kind: "org", Owner: "acme", Enabled: true, MaxRunners: 4}, 4)
	rec := &recordingLog{}
	h.t.log = slog.New(rec)
	_ = h.st.Close()
	if err := h.t.startRunner(pool.Lease{Slot: 0, Owner: "acme"}); err != nil {
		t.Fatal(err)
	}
	if h.runnerCount() != 1 || !rec.has("record runner registration") {
		t.Fatalf("runners %d, logs %v", h.runnerCount(), rec.logs)
	}
}

// --- job messages ---

func TestScaleWithoutMessageReconciles(t *testing.T) {
	h := newHarness(t, store.Target{Kind: "org", Owner: "acme", Enabled: true, MaxRunners: 4}, 4)
	h.setDemand(1)
	s := &scaler{t: h.t, session: h.api.session}
	if err := s.Scale(context.Background(), nil); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "runner started", func() bool { return h.startingCount() == 0 && h.runnerCount() == 1 })
}

func TestScaleFailsWhenJobsCannotBeAcquired(t *testing.T) {
	h := newHarness(t, store.Target{Kind: "org", Owner: "acme", Enabled: true, MaxRunners: 4}, 4)
	h.api.session.acquireErr = errors.New("session expired")
	s := &scaler{t: h.t, session: h.api.session}
	avail := &scaleset.JobAvailable{JobMessageBase: scaleset.JobMessageBase{JobID: "j1", RunnerRequestID: 11}}
	err := s.Scale(context.Background(), &scaleset.RunnerScaleSetMessage{
		JobAvailableMessages: []*scaleset.JobAvailable{avail},
		Statistics:           &scaleset.RunnerScaleSetStatistic{TotalAssignedJobs: 3},
	})
	if err == nil || !strings.Contains(err.Error(), "acquire jobs") || !errors.Is(err, h.api.session.acquireErr) {
		t.Fatalf("Scale returned %v", err)
	}
	h.t.mu.Lock()
	desired := h.t.desired
	h.t.mu.Unlock()
	if desired != 0 {
		t.Fatal("the message failed: its statistics must not be applied, the listener retries it")
	}
	if got := h.api.session.acquired; !slices.Equal(got, []int64{11}) {
		t.Fatalf("acquired %v", got)
	}
}

func TestJobsAssignedSkipsNilEntries(t *testing.T) {
	h := newHarness(t, store.Target{Kind: "org", Owner: "acme", Enabled: true, MaxRunners: 4}, 0)
	s := &scaler{t: h.t, session: h.api.session}
	if err := s.Scale(context.Background(), &scaleset.RunnerScaleSetMessage{JobAssignedMessages: []*scaleset.JobAssigned{nil}}); err != nil {
		t.Fatal(err)
	}
}

func TestUpsertJobFailureIsOnlyLogged(t *testing.T) {
	h := newHarness(t, store.Target{Kind: "org", Owner: "acme", Enabled: true, MaxRunners: 4}, 0)
	rec := &recordingLog{}
	h.t.log = slog.New(rec)
	_ = h.st.Close()
	(&scaler{t: h.t}).upsertJob(store.Job{JobID: "j1"})
	if !rec.has("record job") {
		t.Fatalf("failure not logged: %v", rec.logs)
	}
}

func TestFetchQueueTimeGivesUpAfterThreeTries(t *testing.T) {
	h := newHarness(t, store.Target{Kind: "org", Owner: "acme", Enabled: true, MaxRunners: 4}, 0)
	clk := newFakeClock()
	clk.install(h.m)
	h.m.queueLookupRetry = 7 * time.Second
	calls := 0
	h.m.JobQueuedAt = func(context.Context, store.Target, string, string, int64, string) (time.Time, error) {
		calls++
		return time.Time{}, errors.New("not listed yet")
	}
	s := &scaler{t: h.t, session: h.api.session}
	s.fetchQueueTime(store.Job{TargetID: h.t.cfg().ID, JobID: "j1", RunID: 3, RunnerName: "pitlane-a", Slot: -1})
	if calls != queueLookupAttempts {
		t.Fatalf("%d lookups, want %d", calls, queueLookupAttempts)
	}
	if got := clk.waited(); !slices.Equal(got, []time.Duration{7 * time.Second, 7 * time.Second}) {
		t.Fatalf("waited %v between tries", got)
	}
}

// --- usage ---

func TestRecordUsageRetriesOnceTheJobIsKnown(t *testing.T) {
	h := newHarness(t, store.Target{Kind: "org", Owner: "acme", Enabled: true, MaxRunners: 4}, 0)
	h.m.usageRetry = 15 * time.Second
	asked := make(chan time.Duration, 1)
	gate := make(chan time.Time)
	h.m.after = func(d time.Duration) <-chan time.Time { asked <- d; return gate }
	u := pool.JobUsage{Wall: time.Second, Runner: pool.CgroupUsage{MemPeak: 7, CPUUsec: -1, CPUSomeUsec: -1, IOFullUsec: -1, MemFullUsec: -1, AnonPeak: -1},
		Slot: pool.CgroupUsage{MemPeak: -1, CPUUsec: -1, CPUSomeUsec: -1, IOFullUsec: -1, MemFullUsec: -1, AnonPeak: -1}}

	h.m.recordUsage("pitlane-x", u)
	if d := <-asked; d != 15*time.Second {
		t.Fatalf("retry after %v", d)
	}
	// The job message arrives after the runner exited, then the retry runs.
	if err := h.st.UpsertJob(store.Job{TargetID: h.t.cfg().ID, JobID: "j1", RunnerName: "pitlane-x", Slot: 0, StartedAt: 1}); err != nil {
		t.Fatal(err)
	}
	close(gate)
	waitFor(t, "usage recorded by the retry", func() bool {
		j, err := h.st.JobByRunner(context.Background(), "pitlane-x")
		return err == nil && j.Usage != nil && *j.Usage.Runner.MemPeak == 7
	})
}

func TestRecordUsageStoreFailureIsNotRetried(t *testing.T) {
	h := newHarness(t, store.Target{Kind: "org", Owner: "acme", Enabled: true, MaxRunners: 4}, 0)
	rec := &recordingLog{}
	h.m.log = slog.New(rec)
	clk := newFakeClock()
	clk.install(h.m)
	_ = h.st.Close()
	h.m.recordUsage("pitlane-x", pool.JobUsage{Wall: time.Second, Runner: pool.CgroupUsage{MemPeak: 7, CPUUsec: -1, CPUSomeUsec: -1, IOFullUsec: -1, MemFullUsec: -1, AnonPeak: -1},
		Slot: pool.CgroupUsage{MemPeak: -1, CPUUsec: -1, CPUSomeUsec: -1, IOFullUsec: -1, MemFullUsec: -1, AnonPeak: -1}})
	if !rec.has("record job resource usage") || len(clk.waited()) != 0 {
		t.Fatalf("logs %v, retries %v", rec.logs, clk.waited())
	}
}

// --- housekeeping ---

func TestHousekeepingFailuresAreOnlyLogged(t *testing.T) {
	h := newHarness(t, store.Target{}, 0)
	rec := &recordingLog{}
	h.m.log = slog.New(rec)
	_ = h.st.Close()
	h.m.housekeeping()
	for _, what := range []string{"prune job history", "prune audit log", "prune expired sessions"} {
		if !rec.has(what) {
			t.Errorf("%q not logged: %v", what, rec.logs)
		}
	}
}

// --- archived logs ---

type failingWriter struct{ after int }

func (f *failingWriter) Write(b []byte) (int, error) {
	if f.after <= 0 {
		return 0, errors.New("disk full")
	}
	n := min(len(b), f.after)
	f.after -= n
	if n < len(b) {
		return n, errors.New("disk full")
	}
	return n, nil
}

func TestCapWriter(t *testing.T) {
	var out bytes.Buffer
	c := &capWriter{w: &out, n: 5}
	if n, err := c.Write([]byte("abc")); n != 3 || err != nil {
		t.Fatalf("within the cap: %d %v", n, err)
	}
	if n, err := c.Write([]byte("defgh")); n != 2 || !errors.Is(err, errLogFull) {
		t.Fatalf("crossing the cap: %d %v", n, err)
	}
	if n, err := c.Write([]byte("x")); n != 0 || !errors.Is(err, errLogFull) {
		t.Fatalf("past the cap: %d %v", n, err)
	}
	if out.String() != "abcde" {
		t.Fatalf("kept %q", out.String())
	}
	// Exactly the cap is not a truncation.
	out.Reset()
	if err := archiveLog(&out, 4, func(w io.Writer) error { _, err := w.Write([]byte("abcd")); return err }); err != nil || out.String() != "abcd" {
		t.Fatalf("log of exactly the cap: %q %v", out.String(), err)
	}
	// The destination failing is not a truncation either.
	c = &capWriter{w: &failingWriter{after: 2}, n: 10}
	if n, err := c.Write([]byte("abcd")); n != 2 || err == nil || errors.Is(err, errLogFull) {
		t.Fatalf("failing destination: %d %v", n, err)
	}
	if err := archiveLog(&out, 10, func(io.Writer) error { return errors.New("docker gone") }); err == nil || err.Error() != "docker gone" {
		t.Fatalf("copy error: %v", err)
	}
}

func TestArchiveRunnerLog(t *testing.T) {
	h := newHarness(t, store.Target{Kind: "org", Owner: "acme", Enabled: true, MaxRunners: 4}, 0)
	r := &runner{name: "pitlane-a", containerID: "c1"}

	t.Run("size cap with a notice", func(t *testing.T) {
		h.m.logLimit = 1 << 20
		h.p.logs = strings.Repeat("x", 3<<20)
		h.m.archiveRunnerLog(context.Background(), r)
		b, err := os.ReadFile(h.m.LogFile(r.name))
		if err != nil {
			t.Fatal(err)
		}
		body, notice, _ := strings.Cut(string(b), "\n[pitlane]")
		if len(body) != 1<<20 || !strings.Contains(notice, "truncated at 1 MB") {
			t.Fatalf("kept %d bytes, notice %q", len(body), notice)
		}
	})
	t.Run("a docker failure keeps what was read", func(t *testing.T) {
		h.m.logLimit = 0
		rec := &recordingLog{}
		h.m.log = slog.New(rec)
		h.p.logs, h.p.logsErr = "partial", errors.New("stream cut")
		h.m.archiveRunnerLog(context.Background(), r)
		if b, _ := os.ReadFile(h.m.LogFile(r.name)); string(b) != "partial" {
			t.Fatalf("log %q", b)
		}
		if !rec.has("archive runner log") {
			t.Fatalf("failure not logged: %v", rec.logs)
		}
	})
	t.Run("an unwritable directory only loses the log", func(t *testing.T) {
		rec := &recordingLog{}
		h.m.log = slog.New(rec)
		h.m.logDir = filepath.Join(t.TempDir(), "missing")
		h.m.archiveRunnerLog(context.Background(), r)
		if !rec.has("archive runner log") {
			t.Fatalf("failure not logged: %v", rec.logs)
		}
	})
}

// --- strict requirement ---

func TestApplyStrictRequirement(t *testing.T) {
	m, st := newBareManager(t, nil)
	rec := &recordingLog{}
	m.log = slog.New(rec)
	mk := func(strict bool, required string) store.Target {
		tg, err := st.CreateTarget(store.Target{Kind: "repo", Owner: "acme", Repo: fmt.Sprint(time.Now().UnixNano()), Enabled: true, Strict: strict, StrictRequired: required})
		if err != nil {
			t.Fatal(err)
		}
		return tg
	}
	pub := store.StrictPublicRepo
	for _, tc := range []struct {
		name   string
		target store.Target
		reason string
		want   bool
		// strict and required are the stored state afterwards.
		strict   bool
		required string
	}{
		{"nothing required, nothing recorded", mk(false, ""), "", false, false, ""},
		{"already strict for that reason", mk(true, pub), pub, false, true, pub},
		{"newly required", mk(false, ""), pub, true, true, pub},
		{"requirement lifted keeps strict", mk(true, pub), "", true, true, ""},
		{"required but not strict yet", mk(false, pub), pub, true, true, pub},
	} {
		if got := m.ApplyStrictRequirement(tc.target, tc.reason); got != tc.want {
			t.Errorf("%s: got %v want %v", tc.name, got, tc.want)
		}
		stored, err := st.Target(tc.target.ID)
		if err != nil {
			t.Fatal(err)
		}
		if stored.Strict != tc.strict || stored.StrictRequired != tc.required {
			t.Errorf("%s: stored strict=%v required=%q, want strict=%v required=%q", tc.name, stored.Strict, stored.StrictRequired, tc.strict, tc.required)
		}
	}

	_ = st.Close()
	if m.ApplyStrictRequirement(store.Target{ID: 1}, store.StrictPublicRepo) || !rec.has("record strict requirement") {
		t.Fatalf("a store failure must report no change and be logged: %v", rec.logs)
	}
}

func TestEnforceStrictAllWithoutCheckOrStore(t *testing.T) {
	m, st := newBareManager(t, nil)
	rec := &recordingLog{}
	m.log = slog.New(rec)
	m.EnforceStrictAll(context.Background()) // no check configured: nothing happens
	m.StrictReason = func(context.Context, store.Target) (string, error) {
		t.Error("asked GitHub although the targets cannot be loaded")
		return "", nil
	}
	_ = st.Close()
	m.EnforceStrictAll(context.Background())
	if !rec.has("load targets") {
		t.Fatalf("failure not logged: %v", rec.logs)
	}
}

// A target found public is switched to strict, its running listener follows
// and the idle runner started in the shared mode is replaced.
func TestEnforceStrictAllReplacesIdleRunners(t *testing.T) {
	h := newHarness(t, store.Target{Kind: "repo", Owner: "acme", Repo: "pub", Enabled: true, MinRunners: 1, MaxRunners: 4}, 4)
	h.startedManager(t)
	h.addRunner("loose", 9, time.Minute, false, false)
	h.m.StrictReason = func(context.Context, store.Target) (string, error) { return store.StrictPublicRepo, nil }
	h.m.EnforceStrictAll(context.Background())

	if got := h.t.cfg(); !got.Strict || got.StrictRequired != store.StrictPublicRepo {
		t.Fatalf("listener config %+v", got)
	}
	waitFor(t, "idle shared runner replaced", func() bool {
		return slices.Contains(h.p.snapshot().removed, "ctr-loose") && h.startingCount() == 0 && len(h.p.snapshot().acquired) == 1
	})
	if acq := h.p.snapshot().acquired; !acq[0].Strict {
		t.Fatalf("replacement acquired as %+v, want strict", acq[0])
	}
	h.p.mu.Lock()
	defer h.p.mu.Unlock()
	if !h.p.strictDemand {
		t.Fatal("the pool must know about the strict demand")
	}
}
