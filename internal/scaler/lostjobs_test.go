package scaler

import (
	"context"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/actions/scaleset"

	"github.com/TuroYT/pitlane/internal/store"
)

func testTarget(t *testing.T) *target {
	t.Helper()
	st, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	m := &Manager{store: st, pool: newFakePool(4), log: slog.New(slog.NewTextHandler(io.Discard, nil)), changed: func() {},
		targets: map[int64]*target{}, adopted: map[int64][]*runner{}, lost: map[int64]map[string]lostJob{}}
	return newTarget(m, store.Target{ID: 1, MaxRunners: 4})
}

func started(runner, job string) scaleset.JobStarted {
	return scaleset.JobStarted{RunnerName: runner, JobMessageBase: scaleset.JobMessageBase{JobID: job}}
}

func completed(runner, job string) scaleset.JobCompleted {
	return scaleset.JobCompleted{RunnerName: runner, Result: "failed", JobMessageBase: scaleset.JobMessageBase{JobID: job}}
}

func TestPruneLostJobs(t *testing.T) {
	now := time.Now()
	lost := map[string]lostJob{
		"fresh": {runner: "a", at: now.Add(-time.Minute)},
		"old":   {runner: "b", at: now.Add(-lostJobTTL - time.Second)},
	}
	if n := pruneLostJobs(lost, now); n != 1 {
		t.Fatalf("got %d", n)
	}
	if _, ok := lost["old"]; ok {
		t.Fatal("expired entry kept")
	}
	if effectiveDesired(1, 3) != 0 || effectiveDesired(5, 2) != 3 {
		t.Fatal("effectiveDesired")
	}
}

func TestLostJobLifecycle(t *testing.T) {
	ctx := context.Background()
	tg := testTarget(t)
	s := &scaler{t: tg}
	r := &runner{name: "pitlane-a"}
	tg.runners[r.name] = r

	// The job starts on our runner, which is then killed before completing.
	if err := s.Scale(ctx, &scaleset.RunnerScaleSetMessage{JobStartedMessages: []*scaleset.JobStarted{ptr(started("pitlane-a", "j1"))}}); err != nil {
		t.Fatal(err)
	}
	if r.jobID != "j1" || !r.busy {
		t.Fatalf("runner not tracking its job: %+v", r)
	}
	tg.runnerGone(r)
	if _, ok := tg.lost["j1"]; !ok {
		t.Fatal("job not recorded as lost")
	}

	// A redelivered JobStarted for the dead runner keeps the entry.
	_ = s.Scale(ctx, &scaleset.RunnerScaleSetMessage{JobStartedMessages: []*scaleset.JobStarted{ptr(started("pitlane-a", "j1"))}})
	if _, ok := tg.lost["j1"]; !ok {
		t.Fatal("redelivered JobStarted dropped the entry")
	}

	// JobCompleted clears it.
	_ = s.Scale(ctx, &scaleset.RunnerScaleSetMessage{JobCompletedMessages: []*scaleset.JobCompleted{ptr(completed("pitlane-a", "j1"))}})
	if len(tg.lost) != 0 {
		t.Fatal("JobCompleted did not clear the entry")
	}
}

func TestCompletedJobIsNotLost(t *testing.T) {
	ctx := context.Background()
	tg := testTarget(t)
	s := &scaler{t: tg}
	r := &runner{name: "pitlane-a"}
	tg.runners[r.name] = r
	_ = s.Scale(ctx, &scaleset.RunnerScaleSetMessage{
		JobStartedMessages:   []*scaleset.JobStarted{ptr(started("pitlane-a", "j1"))},
		JobCompletedMessages: []*scaleset.JobCompleted{ptr(completed("pitlane-a", "j1"))},
	})
	tg.runnerGone(r) // normal exit after the job completed
	if len(tg.lost) != 0 {
		t.Fatal("completed job recorded as lost")
	}
}

func TestLostJobStartedElsewhere(t *testing.T) {
	tg := testTarget(t)
	s := &scaler{t: tg}
	tg.lost["j1"] = lostJob{runner: "pitlane-dead", at: time.Now()}
	tg.lost["j2"] = lostJob{runner: "pitlane-dead2", at: time.Now()}
	_ = s.Scale(context.Background(), &scaleset.RunnerScaleSetMessage{JobStartedMessages: []*scaleset.JobStarted{ptr(started("pitlane-b", "j1"))}})
	if _, ok := tg.lost["j1"]; ok {
		t.Fatal("job restarted on another runner still counted as lost")
	}
	tg.forgetLostRunner("pitlane-dead2")
	if len(tg.lost) != 0 {
		t.Fatal("forgetLostRunner kept the entry")
	}
}

func TestFindLostJobsAtStartup(t *testing.T) {
	tg := testTarget(t)
	m := tg.m
	for _, j := range []store.Job{
		{TargetID: 1, JobID: "alive", RunnerName: "pitlane-alive", Slot: -1, StartedAt: 100},
		{TargetID: 1, JobID: "dead", RunnerName: "pitlane-dead", Slot: -1, StartedAt: 100},
	} {
		if err := m.store.UpsertJob(j); err != nil {
			t.Fatal(err)
		}
	}
	for i, n := range []string{"pitlane-alive", "pitlane-dead"} {
		_ = m.store.AddRunner(store.RegisteredRunner{ID: int64(i + 1), TargetID: 1, Name: n})
	}
	alive := &runner{name: "pitlane-alive", busy: true}
	m.findLostJobs(context.Background(), map[string]*runner{alive.name: alive})
	if alive.jobID != "alive" {
		t.Fatalf("adopted runner did not learn its job: %q", alive.jobID)
	}
	if l, ok := m.lost[1]["dead"]; !ok || l.runner != "pitlane-dead" || len(m.lost[1]) != 1 {
		t.Fatalf("lost jobs: %+v", m.lost)
	}
}

func ptr[T any](v T) *T { return &v }

func TestMarkLostIgnoresUnknownJob(t *testing.T) {
	tg := testTarget(t)
	tg.mu.Lock()
	tg.markLostLocked("", "pitlane-a", time.Now())
	tg.mu.Unlock()
	if len(tg.lost) != 0 {
		t.Fatalf("a runner without a job left %v", tg.lost)
	}
}

func TestFindLostJobsStoreFailure(t *testing.T) {
	tg := testTarget(t)
	rec := &recordingLog{}
	tg.m.log = slog.New(rec)
	_ = tg.m.store.Close()
	tg.m.findLostJobs(context.Background(), nil)
	if len(tg.m.lost) != 0 || !rec.has("find in-flight jobs") {
		t.Fatalf("lost %v, logs %v", tg.m.lost, rec.logs)
	}
}

func TestKeepLostMergesWithWhatIsKept(t *testing.T) {
	tg := testTarget(t)
	m := tg.m
	now := time.Now()
	m.mu.Lock()
	m.keepLostLocked(1, tg) // nothing to keep: no entry made
	m.mu.Unlock()
	if _, ok := m.lost[1]; ok {
		t.Fatal("an empty map was kept")
	}
	m.lost[1] = map[string]lostJob{"old": {runner: "a", at: now}}
	tg.lost["new"] = lostJob{runner: "b", at: now}
	m.mu.Lock()
	m.keepLostLocked(1, tg)
	m.mu.Unlock()
	if len(m.lost[1]) != 2 {
		t.Fatalf("kept %v, want both jobs", m.lost[1])
	}
}
