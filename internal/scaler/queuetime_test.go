package scaler

import (
	"context"
	"testing"
	"time"

	"github.com/TuroYT/pitlane/internal/store"
	"github.com/actions/scaleset"
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
