package scaler

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/actions/scaleset"

	"github.com/TuroYT/pitlane/internal/store"
)

func jobRow(t *testing.T, tg *target, job string) (queued, started int64) {
	t.Helper()
	jobs, _, err := tg.m.store.JobsPage(context.Background(), store.JobFilter{}, 50, 0)
	if err != nil {
		t.Fatal(err)
	}
	for _, j := range jobs {
		if j.JobID == job {
			return j.QueuedAt, j.StartedAt
		}
	}
	t.Fatalf("job %s not recorded", job)
	return 0, 0
}

func TestQueueTimeFallsBackToScaleSetAssignTime(t *testing.T) {
	tg := testTarget(t)
	s := &scaler{t: tg}
	assign := time.Now().Add(-time.Hour).Truncate(time.Second)
	st := started("pitlane-a", "j1")
	st.ScaleSetAssignTime = assign
	st.RunnerAssignTime = assign.Add(30 * time.Second)
	if err := s.Scale(context.Background(), &scaleset.RunnerScaleSetMessage{JobStartedMessages: []*scaleset.JobStarted{&st}}); err != nil {
		t.Fatal(err)
	}
	q, sa := jobRow(t, tg, "j1")
	if q != assign.Unix() || sa != assign.Add(30*time.Second).Unix() {
		t.Fatalf("queued %d started %d, want %d and %d", q, sa, assign.Unix(), assign.Add(30*time.Second).Unix())
	}
}

func TestJobStartedDoesNotFakeQueueTime(t *testing.T) {
	tg := testTarget(t)
	s := &scaler{t: tg}
	st := started("pitlane-a", "j1")
	st.RunnerAssignTime = time.Now().Add(-time.Minute)
	if err := s.Scale(context.Background(), &scaleset.RunnerScaleSetMessage{JobStartedMessages: []*scaleset.JobStarted{&st}}); err != nil {
		t.Fatal(err)
	}
	if q, sa := jobRow(t, tg, "j1"); q != 0 || sa == 0 {
		t.Fatalf("queued %d started %d, want queued unknown", q, sa)
	}
}

func TestJobAssignedKeepsQueueTime(t *testing.T) {
	tg := testTarget(t)
	s := &scaler{t: tg}
	queue := time.Now().Add(-time.Hour).Truncate(time.Second)
	a := &scaleset.JobAssigned{JobMessageBase: scaleset.JobMessageBase{JobID: "j1", QueueTime: queue}}
	undated := &scaleset.JobAssigned{JobMessageBase: scaleset.JobMessageBase{JobID: "j2"}}
	if err := s.Scale(context.Background(), &scaleset.RunnerScaleSetMessage{JobAssignedMessages: []*scaleset.JobAssigned{a, undated}}); err != nil {
		t.Fatal(err)
	}
	if q, _ := jobRow(t, tg, "j1"); q != queue.Unix() {
		t.Fatalf("queued %d, want %d", q, queue.Unix())
	}
	jobs, _, _ := tg.m.store.JobsPage(context.Background(), store.JobFilter{}, 50, 0)
	if len(jobs) != 1 {
		t.Fatalf("%d jobs recorded, want only the dated one", len(jobs))
	}
}

func TestQueueTimeFromAPIWhenMissing(t *testing.T) {
	tg := testTarget(t)
	s := &scaler{t: tg}
	created := time.Now().Add(-10 * time.Minute).Truncate(time.Second)
	var calls atomic.Int32
	tg.m.JobQueuedAt = func(_ context.Context, _ store.Target, owner, repo string, runID int64, runner string) (time.Time, error) {
		if owner != "acme" || repo != "app" || runID != 7 || runner != "pitlane-a" {
			t.Errorf("lookup %s/%s run %d runner %s", owner, repo, runID, runner)
		}
		if calls.Add(1) == 1 {
			return time.Time{}, errors.New("runner not listed yet")
		}
		return created, nil
	}
	tg.m.queueLookupRetry = time.Millisecond

	st := started("pitlane-a", "j1")
	st.OwnerName, st.RepositoryName, st.WorkflowRunID = "acme", "app", 7
	st.ScaleSetAssignTime = time.Now().Add(-time.Minute)
	if err := s.Scale(context.Background(), &scaleset.RunnerScaleSetMessage{JobStartedMessages: []*scaleset.JobStarted{&st}}); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(2 * time.Second)
	for {
		q, _ := jobRow(t, tg, "j1")
		if q == created.Unix() {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("queued %d, want %d", q, created.Unix())
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func TestQueueTimeNotLookedUpWhenSent(t *testing.T) {
	tg := testTarget(t)
	s := &scaler{t: tg}
	tg.m.JobQueuedAt = func(context.Context, store.Target, string, string, int64, string) (time.Time, error) {
		t.Error("lookup despite a queue time in the message")
		return time.Time{}, nil
	}
	st := started("pitlane-a", "j1")
	st.WorkflowRunID = 7
	st.QueueTime = time.Now().Add(-time.Hour).Truncate(time.Second)
	if err := s.Scale(context.Background(), &scaleset.RunnerScaleSetMessage{JobStartedMessages: []*scaleset.JobStarted{&st}}); err != nil {
		t.Fatal(err)
	}
	if q, _ := jobRow(t, tg, "j1"); q != st.QueueTime.Unix() {
		t.Fatalf("queued %d, want %d", q, st.QueueTime.Unix())
	}
}
