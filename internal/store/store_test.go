package store

import (
	"context"
	"testing"
)

func open(t *testing.T) *Store {
	t.Helper()
	s, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func TestSecretsRoundTrip(t *testing.T) {
	s := open(t)
	if err := s.SetSecret("app_pem", "-----BEGIN KEY-----"); err != nil {
		t.Fatal(err)
	}
	if raw := s.Get("app_pem", ""); raw == "" || raw == "-----BEGIN KEY-----" {
		t.Fatalf("secret stored in clear: %q", raw)
	}
	got, err := s.GetSecret("app_pem")
	if err != nil || got != "-----BEGIN KEY-----" {
		t.Fatalf("got %q, %v", got, err)
	}
}

func TestUpsertJobKeepsKnownFields(t *testing.T) {
	s := open(t)
	ctx := context.Background()
	if err := s.UpsertJob(Job{TargetID: 1, JobID: "j1", RunID: 42, JobName: "build", Slot: -1, QueuedAt: 100}); err != nil {
		t.Fatal(err)
	}
	if err := s.UpsertJob(Job{TargetID: 1, JobID: "j1", RunnerName: "pitlane-x", Slot: 3, StartedAt: 105}); err != nil {
		t.Fatal(err)
	}
	if err := s.UpsertJob(Job{TargetID: 1, JobID: "j1", Slot: -1, FinishedAt: 130, Result: "succeeded"}); err != nil {
		t.Fatal(err)
	}
	jobs, err := s.Jobs(ctx, 10)
	if err != nil || len(jobs) != 1 {
		t.Fatalf("jobs=%v err=%v", jobs, err)
	}
	j := jobs[0]
	if j.RunID != 42 || j.JobName != "build" || j.RunnerName != "pitlane-x" || j.Slot != 3 ||
		j.QueuedAt != 100 || j.StartedAt != 105 || j.FinishedAt != 130 || j.Result != "succeeded" {
		t.Fatalf("unexpected merge: %+v", j)
	}
}

func TestTargets(t *testing.T) {
	s := open(t)
	created, err := s.CreateTarget(Target{Kind: "repo", Owner: "acme", Repo: "api", Enabled: true})
	if err != nil {
		t.Fatal(err)
	}
	if created.ScaleSetName != "pitlane" || created.MaxRunners != 4 {
		t.Fatalf("defaults not applied: %+v", created)
	}
	if _, err := s.CreateTarget(Target{Kind: "repo", Owner: "acme", Repo: "api"}); err == nil {
		t.Fatal("duplicate target accepted")
	}
	if created.URL() != "https://github.com/acme/api" {
		t.Fatal(created.URL())
	}
}
