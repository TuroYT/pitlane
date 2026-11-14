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

func TestTargetMaxJobMinutes(t *testing.T) {
	s := open(t)
	created, err := s.CreateTarget(Target{Kind: "repo", Owner: "acme", Repo: "api", MaxJobMinutes: DefaultMaxJobMinutes})
	if err != nil {
		t.Fatal(err)
	}
	got, err := s.Target(created.ID)
	if err != nil || got.MaxJobMinutes != 360 {
		t.Fatalf("got %+v, %v", got, err)
	}
	got.MaxJobMinutes = 0 // unlimited
	if err := s.UpdateTarget(got); err != nil {
		t.Fatal(err)
	}
	if got, _ = s.Target(created.ID); got.MaxJobMinutes != 0 {
		t.Fatalf("unlimited not stored: %+v", got)
	}
	// Rows created before the migration get the default.
	if _, err := s.db.Exec(`INSERT INTO targets (kind, owner, repo, created_at) VALUES ('org', 'old', '', 0)`); err != nil {
		t.Fatal(err)
	}
	ts, _ := s.Targets()
	for _, x := range ts {
		if x.Owner == "old" && x.MaxJobMinutes != DefaultMaxJobMinutes {
			t.Fatalf("column default not applied: %+v", x)
		}
	}
}

func TestRegisteredRunners(t *testing.T) {
	s := open(t)
	if err := s.AddRunner(RegisteredRunner{ID: 7, TargetID: 1, Name: "pitlane-a"}); err != nil {
		t.Fatal(err)
	}
	_ = s.AddRunner(RegisteredRunner{ID: 8, TargetID: 2, Name: "pitlane-b"})
	if id := s.RunnerIDByName("pitlane-a"); id != 7 {
		t.Fatal(id)
	}
	if id := s.RunnerIDByName("nope"); id != 0 {
		t.Fatal(id)
	}
	rs, err := s.RegisteredRunners(1)
	if err != nil || len(rs) != 1 || rs[0].Name != "pitlane-a" || rs[0].CreatedAt == 0 {
		t.Fatalf("%+v %v", rs, err)
	}
	_ = s.DeleteRunner(7)
	if rs, _ := s.RegisteredRunners(1); len(rs) != 0 {
		t.Fatalf("not deleted: %+v", rs)
	}
}
